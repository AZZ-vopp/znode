package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
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
	// Keep the routed probe on the same lightweight endpoint used for the
	// connectivity control.  The previous Cloudflare endpoint intermittently
	// timed out from customer paths, which made every WireGuard member look
	// unhealthy and caused the balancer to flap during normal traffic.
	wgBalancerProbeDestination  = "https://www.gstatic.com/generate_204"
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
	if len(config.Outbounds) < 2 || len(config.Outbounds) > 8 {
		return "", nil, nil, nil, fmt.Errorf("WireGuard balancer must contain 2 to 8 outbounds")
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
		if err := validateWireGuardOutbound(outbound); err != nil {
			return "", nil, nil, nil, fmt.Errorf("validate WireGuard outbound %q: %w", userTag, err)
		}
		seenTags[userTag] = struct{}{}
		// Manager.Select is prefix based. Do not use an operator supplied member
		// tag as a selector: map it to a reserved per-route internal tag so an
		// unrelated custom outbound can never be selected accidentally.
		outbound.Tag = fmt.Sprintf("__znode_wg_balancer_%d_%d", routeID, index+1)
		if hasOutboundWithTag(existing, outbound.Tag) {
			return "", nil, nil, nil, fmt.Errorf("WireGuard balancer internal tag %q is already in use", outbound.Tag)
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

// isWireGuardBalancerActionValue keeps default_out backwards compatible with a
// normal OutboundObject while allowing the explicit multi-WireGuard shape used
// by route_wg_balancer. An invalid object with outbounds is deliberately sent
// through the strict balancer validator instead of falling back to direct.
func isWireGuardBalancerActionValue(value *string) bool {
	if value == nil || strings.TrimSpace(*value) == "" {
		return false
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*value), &config); err != nil {
		return false
	}
	_, ok := config["outbounds"]
	return ok
}

func isWireGuardOutboundActionValue(value *string) bool {
	if value == nil || strings.TrimSpace(*value) == "" {
		return false
	}
	var outbound struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal([]byte(*value), &outbound); err != nil {
		return false
	}
	return outbound.Protocol == "wireguard"
}

// appendIPv6BlockBeforeDefaultWireGuard prevents an IPv4-only WireGuard
// catch-all from receiving literal IPv6 destinations. DomainStrategy=ForceIPv4
// covers domains resolved inside Xray, but clients can still send cached or
// hard-coded IPv6 literals. Rejecting them before the catch-all avoids long
// network-unreachable/QUIC timeout cascades and never leaks them via direct.
func appendIPv6BlockBeforeDefaultWireGuard(routerConfig *coreConf.RouterConfig, inboundTag string) error {
	rule := map[string]interface{}{
		"inboundTag":  inboundTag,
		"ip":          []string{"::/0"},
		"outboundTag": "block",
	}
	rawRule, err := json.Marshal(rule)
	if err != nil {
		return fmt.Errorf("marshal IPv6 guard: %w", err)
	}
	routerConfig.RuleList = append(routerConfig.RuleList, rawRule)
	return nil
}

// validateWireGuardOutbound enforces the standalone Xray WireGuard shape.
// WireGuard is a native device outbound and must not carry transport settings.
func validateWireGuardOutbound(outbound *coreConf.OutboundDetourConfig) error {
	if outbound == nil || outbound.Settings == nil {
		return fmt.Errorf("settings are required")
	}
	if outbound.StreamSetting != nil {
		return fmt.Errorf("streamSettings is not supported")
	}
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(*outbound.Settings, &rawSettings); err != nil {
		return fmt.Errorf("decode settings: %w", err)
	}
	for key := range rawSettings {
		switch key {
		case "secretKey", "address", "peers", "mtu", "reserved", "domainStrategy":
		case "noKernelTun":
			// Older ZBoard releases injected this field and forced gVisor TUN.
			// Remove it so native Xray can prefer kernel TUN when the host supports
			// it, while still falling back to gVisor on unsupported hosts.
			delete(rawSettings, key)
		default:
			return fmt.Errorf("settings field %q is not supported", key)
		}
	}
	if err := normalizeWireGuardIPv4(rawSettings); err != nil {
		return err
	}
	normalizedSettings, err := json.Marshal(rawSettings)
	if err != nil {
		return fmt.Errorf("normalize settings: %w", err)
	}
	outbound.Settings = (*json.RawMessage)(&normalizedSettings)
	var settings coreConf.WireGuardConfig
	if err := json.Unmarshal(*outbound.Settings, &settings); err != nil {
		return fmt.Errorf("decode settings: %w", err)
	}
	if strings.TrimSpace(settings.SecretKey) == "" {
		return fmt.Errorf("secretKey is required")
	}
	if _, err := coreConf.ParseWireGuardKey(settings.SecretKey); err != nil {
		return fmt.Errorf("invalid secretKey: %w", err)
	}
	if len(settings.Address) == 0 {
		return fmt.Errorf("address must contain at least one value")
	}
	for i, address := range settings.Address {
		if strings.TrimSpace(address) == "" {
			return fmt.Errorf("address[%d] is empty", i)
		}
	}
	if len(settings.Peers) != 1 {
		return fmt.Errorf("peers must contain exactly one peer")
	}
	for i, peer := range settings.Peers {
		if peer == nil {
			return fmt.Errorf("peers[%d] is null", i)
		}
		if strings.TrimSpace(peer.PublicKey) == "" {
			return fmt.Errorf("peers[%d].publicKey is required", i)
		}
		if _, err := coreConf.ParseWireGuardKey(peer.PublicKey); err != nil {
			return fmt.Errorf("peers[%d].publicKey is invalid: %w", i, err)
		}
		if strings.TrimSpace(peer.Endpoint) == "" {
			return fmt.Errorf("peers[%d].endpoint is required", i)
		}
		if len(peer.AllowedIPs) == 0 {
			return fmt.Errorf("peers[%d].allowedIPs must contain at least one value", i)
		}
		var rawPeer map[string]json.RawMessage
		if err := json.Unmarshal(peerJSON(rawSettings["peers"], i), &rawPeer); err != nil {
			return fmt.Errorf("decode peers[%d]: %w", i, err)
		}
		for key := range rawPeer {
			switch key {
			case "publicKey", "endpoint", "allowedIPs", "keepAlive":
			default:
				return fmt.Errorf("peers[%d] field %q is not supported", i, key)
			}
		}
	}
	if len(settings.Reserved) != 0 && len(settings.Reserved) != 3 {
		return fmt.Errorf("reserved must contain exactly 3 bytes")
	}
	if settings.MTU != 0 && (settings.MTU < 576 || settings.MTU > 9000) {
		return fmt.Errorf("mtu must be between 576 and 9000")
	}
	return nil
}

// normalizeWireGuardIPv4 keeps WireGuard balancers on IPv4-only paths. A
// ForceIP device may randomly select an IPv6 address when both A and AAAA
// records exist; many WG peers advertise ::/0 but do not actually route IPv6,
// which makes health probes flap and causes the balancer to oscillate. Strip
// IPv6 addresses/routes and force Xray's resolver to IPv4 for every WG outbound.
func normalizeWireGuardIPv4(settings map[string]json.RawMessage) error {
	addresses, err := filterIPv4CIDRs(settings["address"], "address")
	if err != nil {
		return err
	}
	if len(addresses) == 0 {
		return fmt.Errorf("address must contain at least one IPv4 value")
	}
	addressJSON, err := json.Marshal(addresses)
	if err != nil {
		return fmt.Errorf("normalize address: %w", err)
	}
	settings["address"] = addressJSON

	var peers []json.RawMessage
	if err := json.Unmarshal(settings["peers"], &peers); err != nil {
		return fmt.Errorf("decode peers: %w", err)
	}
	for i, rawPeer := range peers {
		var peer map[string]json.RawMessage
		if err := json.Unmarshal(rawPeer, &peer); err != nil {
			return fmt.Errorf("decode peers[%d]: %w", i, err)
		}
		allowedIPs, err := filterIPv4CIDRs(peer["allowedIPs"], fmt.Sprintf("peers[%d].allowedIPs", i))
		if err != nil {
			return err
		}
		if len(allowedIPs) == 0 {
			return fmt.Errorf("peers[%d].allowedIPs must contain at least one IPv4 value", i)
		}
		allowedJSON, err := json.Marshal(allowedIPs)
		if err != nil {
			return fmt.Errorf("normalize peers[%d].allowedIPs: %w", i, err)
		}
		peer["allowedIPs"] = allowedJSON
		peers[i], err = json.Marshal(peer)
		if err != nil {
			return fmt.Errorf("normalize peers[%d]: %w", i, err)
		}
	}
	peersJSON, err := json.Marshal(peers)
	if err != nil {
		return fmt.Errorf("normalize peers: %w", err)
	}
	settings["peers"] = peersJSON
	settings["domainStrategy"] = json.RawMessage(`"ForceIPv4"`)
	return nil
}

func filterIPv4CIDRs(raw json.RawMessage, field string) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("decode %s: %w", field, err)
	}
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if ip, _, err := net.ParseCIDR(trimmed); err == nil && ip.To4() == nil {
			continue
		}
		filtered = append(filtered, trimmed)
	}
	return filtered, nil
}

func peerJSON(raw json.RawMessage, index int) json.RawMessage {
	var peers []json.RawMessage
	if err := json.Unmarshal(raw, &peers); err != nil || index < 0 || index >= len(peers) {
		return json.RawMessage(`null`)
	}
	return peers[index]
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
	if outbound.Protocol == "wireguard" {
		if err := validateWireGuardOutbound(outbound); err != nil {
			return "", nil, fmt.Errorf("validate WireGuard outbound %q: %w", outbound.Tag, err)
		}
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
	balancerConfigs := make(map[string]string)
	observedOutboundTags := make(map[string]struct{})

	for _, info := range infos {
		if info == nil || info.Common == nil {
			return nil, nil, nil, nil, fmt.Errorf("custom routing received an empty node configuration")
		}
		if len(info.Common.Routes) == 0 {
			continue
		}
		defaultOutCount := 0
		for _, route := range info.Common.Routes {
			if route.Action == "default_out" {
				defaultOutCount++
			}
		}
		if defaultOutCount > 1 {
			return nil, nil, nil, nil, fmt.Errorf("node %d has %d default_out routes; assign exactly one catch-all route", info.Id, defaultOutCount)
		}
		// Older panel snapshots may have saved a catch-all before a specific
		// route. Keep the relative order in both groups, but always emit the
		// catch-alls last so they cannot shadow a later rule for this inbound.
		routes := make([]panel.Route, 0, len(info.Common.Routes))
		for _, route := range info.Common.Routes {
			if route.Action != "default_out" {
				routes = append(routes, route)
			}
		}
		for _, route := range info.Common.Routes {
			if route.Action == "default_out" {
				routes = append(routes, route)
			}
		}
		for _, route := range routes {
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
				canonical := ""
				if route.ActionValue != nil {
					var compact bytes.Buffer
					if err := json.Compact(&compact, []byte(*route.ActionValue)); err == nil {
						canonical = compact.String()
					}
				}
				runtimeTag := fmt.Sprintf("__znode_wg_balancer_group_%d", route.Id)
				if previous, exists := balancerConfigs[runtimeTag]; exists && previous != canonical {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: conflicting WireGuard balancer configuration reuses route ID", info.Id, route.Id)
				}
				balancerTag, customOutbounds, balancer, observedTags, err := buildWireGuardBalancer(route.Id, route.ActionValue, coreOutboundConfig)
				if err == nil && route.Id <= 0 {
					return nil, nil, nil, nil, fmt.Errorf("node %d route %d: WireGuard balancer route ID must be positive", info.Id, route.Id)
				}
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
					balancerConfigs[balancerTag] = canonical
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
				// Legacy default_out routes did not persist an outbound payload. Keep
				// them valid by targeting the built-in direct outbound; a supplied
				// payload continues through the normal custom-outbound validation.
				// A default route may target a complete WireGuard balancer. Unlike a
				// missing legacy value, this never falls back to Default/direct: the
				// balancer's fallback remains its first WireGuard member.
				if isWireGuardBalancerActionValue(route.ActionValue) {
					canonical := ""
					var compact bytes.Buffer
					if err := json.Compact(&compact, []byte(*route.ActionValue)); err == nil {
						canonical = compact.String()
					}
					runtimeTag := fmt.Sprintf("__znode_wg_balancer_group_%d", route.Id)
					if previous, exists := balancerConfigs[runtimeTag]; exists && previous != canonical {
						return nil, nil, nil, nil, fmt.Errorf("node %d route %d: conflicting WireGuard balancer configuration reuses route ID", info.Id, route.Id)
					}
					balancerTag, customOutbounds, balancer, observedTags, err := buildWireGuardBalancer(route.Id, route.ActionValue, coreOutboundConfig)
					if err == nil && route.Id <= 0 {
						return nil, nil, nil, nil, fmt.Errorf("node %d route %d: WireGuard balancer route ID must be positive", info.Id, route.Id)
					}
					if err != nil {
						balancerTag = runtimeTag
						if _, reused := balancerTags[balancerTag]; !reused {
							return nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
						}
					} else if _, exists := balancerTags[balancerTag]; !exists {
						coreOutboundConfig = append(coreOutboundConfig, customOutbounds...)
						coreRouterConfig.Balancers = append(coreRouterConfig.Balancers, balancer)
						balancerTags[balancerTag] = struct{}{}
						balancerConfigs[balancerTag] = canonical
						for _, tag := range observedTags {
							observedOutboundTags[tag] = struct{}{}
						}
					}
					if err := appendIPv6BlockBeforeDefaultWireGuard(coreRouterConfig, info.Tag); err != nil {
						return nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
					}
					rule := map[string]interface{}{
						"inboundTag":  info.Tag,
						"network":     "tcp,udp",
						"balancerTag": balancerTag,
					}
					rawRule, err := json.Marshal(rule)
					if err != nil {
						return nil, nil, nil, nil, fmt.Errorf("node %d route %d: marshal default WireGuard balancer route: %w", info.Id, route.Id, err)
					}
					coreRouterConfig.RuleList = append(coreRouterConfig.RuleList, rawRule)
					continue
				}
				outboundTag := "Default"
				var customOutbound *core.OutboundHandlerConfig
				if route.ActionValue != nil && strings.TrimSpace(*route.ActionValue) != "" {
					var err error
					outboundTag, customOutbound, err = resolveRouteOutbound(route.ActionValue, coreOutboundConfig)
					if err != nil {
						return nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
					}
				}
				if isWireGuardOutboundActionValue(route.ActionValue) {
					if err := appendIPv6BlockBeforeDefaultWireGuard(coreRouterConfig, info.Tag); err != nil {
						return nil, nil, nil, nil, fmt.Errorf("node %d route %d: %w", info.Id, route.Id, err)
					}
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
