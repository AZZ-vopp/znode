package core

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	"github.com/xtls/xray-core/app/observatory/burst"
	"github.com/xtls/xray-core/app/router"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	coreConf "github.com/xtls/xray-core/infra/conf"
)

const (
	wgBalancerProbeDestination  = "https://connectivitycheck.gstatic.com/generate_204"
	wgBalancerProbeConnectivity = "https://www.gstatic.com/generate_204"
	wgBalancerInternalTagPrefix = "__znode_wg_balancer_"
)

// wireGuardBalancerConfig is intentionally narrow. We do not accept arbitrary
// balancer settings from the panel: the node always probes conservatively and
// falls back only to the first WireGuard during probe cold-start (never direct).
type wireGuardBalancerConfig struct {
	Tag       string                          `json:"tag"`
	Strategy  string                          `json:"strategy"`
	Outbounds []coreConf.OutboundDetourConfig `json:"outbounds"`
}

func buildWireGuardBalancer(routeID int, value *string, existing []*core.OutboundHandlerConfig) (string, []*core.OutboundHandlerConfig, *coreConf.BalancingRule, []string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "", nil, nil, nil, fmt.Errorf("WireGuard balancer is missing")
	}
	config := wireGuardBalancerConfig{}
	if err := json.Unmarshal([]byte(*value), &config); err != nil {
		return "", nil, nil, nil, fmt.Errorf("decode WireGuard balancer: %w", err)
	}
	config.Tag = strings.TrimSpace(config.Tag)
	if config.Tag == "" || config.Tag == "Default" || config.Tag == "block" || config.Tag == "dns_out" || strings.HasPrefix(config.Tag, wgBalancerInternalTagPrefix) {
		return "", nil, nil, nil, fmt.Errorf("invalid WireGuard balancer tag")
	}
	strategy := strings.ToLower(strings.TrimSpace(config.Strategy))
	if strategy != "roundrobin" && strategy != "leastping" && strategy != "leastload" {
		return "", nil, nil, nil, fmt.Errorf("unsupported WireGuard balancer strategy %q", config.Strategy)
	}
	if len(config.Outbounds) < 1 || len(config.Outbounds) > 8 {
		return "", nil, nil, nil, fmt.Errorf("WireGuard balancer must contain 1 to 8 outbounds")
	}
	runtimeBalancerTag := fmt.Sprintf("__znode_wg_balancer_group_%d", routeID)

	seenTags := map[string]struct{}{config.Tag: {}}
	tags := make([]string, 0, len(config.Outbounds))
	builtOutbounds := make([]*core.OutboundHandlerConfig, 0, len(config.Outbounds))
	for index := range config.Outbounds {
		outbound := &config.Outbounds[index]
		userTag := strings.TrimSpace(outbound.Tag)
		if outbound.Protocol != "wireguard" || userTag == "" {
			return "", nil, nil, nil, fmt.Errorf("WireGuard balancer member %d must be a tagged WireGuard outbound", index+1)
		}
		if strings.HasPrefix(userTag, wgBalancerInternalTagPrefix) {
			return "", nil, nil, nil, fmt.Errorf("WireGuard balancer member %d tag uses reserved prefix", index+1)
		}
		if _, duplicate := seenTags[userTag]; duplicate {
			return "", nil, nil, nil, fmt.Errorf("WireGuard balancer outbound tag %q is already in use", userTag)
		}
		seenTags[userTag] = struct{}{}
		// Manager.Select is prefix based. Do not use an operator supplied member
		// tag as a selector: map it to a reserved per-route internal tag so an
		// unrelated custom outbound can never be selected accidentally.
		outbound.Tag = fmt.Sprintf("__znode_wg_balancer_%d_%d", routeID, index+1)
		if hasOutboundWithTag(existing, outbound.Tag) {
			return "", nil, nil, nil, fmt.Errorf("WireGuard balancer internal tag %q is already in use", outbound.Tag)
		}
		if err := applyXHTTPStreamDefaults(outbound.StreamSetting); err != nil {
			return "", nil, nil, nil, fmt.Errorf("apply WireGuard outbound defaults: %w", err)
		}
		built, err := outbound.Build()
		if err != nil {
			return "", nil, nil, nil, fmt.Errorf("build WireGuard outbound %q: %w", outbound.Tag, err)
		}
		builtOutbounds = append(builtOutbounds, built)
		tags = append(tags, outbound.Tag)
	}

	// Round-robin is the default because it works immediately while the first
	// probes run. Xray excludes reported unhealthy members; leastPing/leastLoad
	// use the first WG only during their cold start, never the direct outbound.
	strategyConfig := json.RawMessage(`{}`)
	if strategy == "leastload" {
		strategyConfig = json.RawMessage(`{"expected":1,"maxRTT":"3s","tolerance":0.2}`)
	}
	balancer := &coreConf.BalancingRule{
		// The panel tag is operator-facing. Route IDs are immutable, so a
		// generated runtime tag prevents two independently saved rules from
		// aliasing even if legacy data contains the same panel tag.
		Tag:         runtimeBalancerTag,
		Selectors:   tags,
		Strategy:    coreConf.StrategyConfig{Type: strategy, Settings: &strategyConfig},
		FallbackTag: tags[0],
	}
	return runtimeBalancerTag, builtOutbounds, balancer, tags, nil
}

func hasOutboundWithTag(list []*core.OutboundHandlerConfig, tag string) bool {
	for _, o := range list {
		if o != nil && o.Tag == tag {
			return true
		}
	}
	return false
}

func resolveRouteOutbound(value *string, existing []*core.OutboundHandlerConfig) (string, *core.OutboundHandlerConfig, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "", nil, fmt.Errorf("route outbound is missing")
	}
	outbound := &coreConf.OutboundDetourConfig{}
	if err := json.Unmarshal([]byte(*value), outbound); err != nil {
		return "", nil, fmt.Errorf("decode route outbound: %w", err)
	}
	if strings.TrimSpace(outbound.Tag) == "" {
		return "", nil, fmt.Errorf("route outbound tag is missing")
	}
	if strings.HasPrefix(outbound.Tag, wgBalancerInternalTagPrefix) {
		return "", nil, fmt.Errorf("route outbound tag uses reserved WireGuard balancer prefix")
	}
	if err := hardenFreedomOutbound(outbound); err != nil {
		return "", nil, fmt.Errorf("secure route outbound: %w", err)
	}
	if err := applyXHTTPStreamDefaults(outbound.StreamSetting); err != nil {
		return "", nil, fmt.Errorf("apply xhttp outbound defaults: %w", err)
	}
	if hasOutboundWithTag(existing, outbound.Tag) {
		return outbound.Tag, nil, nil
	}
	built, err := outbound.Build()
	if err != nil {
		return "", nil, fmt.Errorf("build route outbound %q: %w", outbound.Tag, err)
	}
	return outbound.Tag, built, nil
}

func GetCustomConfig(infos []*panel.NodeInfo) (*dns.Config, []*core.OutboundHandlerConfig, *router.Config, *burst.Config, error) {
	// Prefer the stable IPv4 egress used by the panel's advertised VPS
	// address. Merely having a public IPv6 address on an interface does not
	// prove that the VPS has a working IPv6 route; broken/black-holed IPv6 is a
	// common cause of intermittent QUIC failures in TikTok and Meta apps.
	queryStrategy := "UseIPv4"
	coreDnsConfig := &coreConf.DNSConfig{
		Servers: []*coreConf.NameServerConfig{
			{
				Address: &coreConf.Address{
					Address: xnet.ParseAddress("localhost"),
				},
			},
		},
		QueryStrategy: queryStrategy,
	}
	//outbound
	defaultoutbound, err := buildDefaultOutbound()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build default outbound: %w", err)
	}
	coreOutboundConfig := append([]*core.OutboundHandlerConfig{}, defaultoutbound)
	block, err := buildBlockOutbound()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build block outbound: %w", err)
	}
	coreOutboundConfig = append(coreOutboundConfig, block)
	dnsOutbound, err := buildDnsOutbound()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build DNS outbound: %w", err)
	}
	coreOutboundConfig = append(coreOutboundConfig, dnsOutbound)

	//route
	domainStrategy := "AsIs"
	dnsRule, _ := json.Marshal(map[string]interface{}{
		"port":        "53",
		"network":     "udp",
		"outboundTag": "dns_out",
	})
	coreRouterConfig := &coreConf.RouterConfig{
		RuleList:       []json.RawMessage{dnsRule},
		DomainStrategy: &domainStrategy,
	}
	balancerTags := make(map[string]struct{})
	observedOutboundTags := make(map[string]struct{})

	for _, info := range infos {
		if info == nil || info.Common == nil {
			return nil, nil, nil, nil, fmt.Errorf("custom routing received an empty node configuration")
		}
		if len(info.Common.Routes) == 0 {
			continue
		}
		for _, route := range info.Common.Routes {
			switch route.Action {
			case "dns":
				if route.ActionValue == nil {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: DNS server is missing", info.Id, route.Id)
				}
				server := &coreConf.NameServerConfig{
					Address: &coreConf.Address{
						Address: xnet.ParseAddress(*route.ActionValue),
					},
				}
				if len(route.Match) != 0 {
					server.Domains = route.Match
					server.SkipFallback = true
				}
				coreDnsConfig.Servers = append(coreDnsConfig.Servers, server)
			case "block":
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"domain":      route.Match,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "block_ip":
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"ip":          route.Match,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "block_port":
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"port":        strings.Join(route.Match, ","),
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "protocol":
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"protocol":    route.Match,
					"outboundTag": "block",
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					continue
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "route":
				outboundTag, customOutbound, err := resolveRouteOutbound(route.ActionValue, coreOutboundConfig)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
				}
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"domain":      route.Match,
					"outboundTag": outboundTag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: marshal domain route: %w", info.Id, route.Id, err)
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
				if customOutbound != nil {
					coreOutboundConfig = append(coreOutboundConfig, customOutbound)
				}
			case "route_ip":
				outboundTag, customOutbound, err := resolveRouteOutbound(route.ActionValue, coreOutboundConfig)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
				}
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"ip":          route.Match,
					"outboundTag": outboundTag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: marshal IP route: %w", info.Id, route.Id, err)
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
				if customOutbound != nil {
					coreOutboundConfig = append(coreOutboundConfig, customOutbound)
				}
			case "route_wg_balancer":
				balancerTag, customOutbounds, balancer, observedTags, err := buildWireGuardBalancer(route.Id, route.ActionValue, coreOutboundConfig)
				if err != nil {
					// The same saved route can be assigned to multiple node inbounds.
					// Once built, reuse that balancer rather than treating it as a tag clash.
					balancerTag = fmt.Sprintf("__znode_wg_balancer_group_%d", route.Id)
					if _, reused := balancerTags[balancerTag]; !reused {
						return nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
					}
				} else if _, exists := balancerTags[balancerTag]; !exists {
					coreOutboundConfig = append(coreOutboundConfig, customOutbounds...)
					coreRouterConfig.Balancers = append(coreRouterConfig.Balancers, balancer)
					balancerTags[balancerTag] = struct{}{}
					for _, tag := range observedTags {
						observedOutboundTags[tag] = struct{}{}
					}
				}
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"domain":      route.Match,
					"balancerTag": balancerTag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: marshal WireGuard balancer route: %w", info.Id, route.Id, err)
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
			case "default_out":
				outboundTag, customOutbound, err := resolveRouteOutbound(route.ActionValue, coreOutboundConfig)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
				}
				rule := map[string]interface{}{
					"inboundTag":  info.Tag,
					"network":     "tcp,udp",
					"outboundTag": outboundTag,
				}
				rawRule, err := json.Marshal(rule)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: marshal default route: %w", info.Id, route.Id, err)
				}
				coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
				if customOutbound != nil {
					coreOutboundConfig = append(coreOutboundConfig, customOutbound)
				}
			default:
				return nil, nil, nil, nil, fmt.Errorf("node %d route %d: unsupported action %q", info.Id, route.Id, route.Action)
			}
		}
	}
	var observatoryConfig *burst.Config
	if len(observedOutboundTags) > 0 {
		selectors := make([]string, 0, len(observedOutboundTags))
		for tag := range observedOutboundTags {
			selectors = append(selectors, tag)
		}
		// Xray's selector is prefix based. Tags have already been validated as
		// unique; sorting only makes generated configs and tests deterministic.
		sort.Strings(selectors)
		observatoryConfig = &burst.Config{
			SubjectSelector: selectors,
			PingConfig: &burst.HealthPingConfig{
				Destination:  wgBalancerProbeDestination,
				Connectivity: wgBalancerProbeConnectivity,
				Interval:     int64(30 * time.Second), SamplingCount: 3,
				Timeout: int64(5 * time.Second), HttpMethod: "HEAD",
			},
		}
	}
	DnsConfig, err := coreDnsConfig.Build()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	RouterConfig, err := coreRouterConfig.Build()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return DnsConfig, coreOutboundConfig, RouterConfig, observatoryConfig, nil
}
