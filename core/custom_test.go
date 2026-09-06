package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/transport/internet"
)

func TestDefaultEgressKeepsDNSAndFreedomOnIPv4(t *testing.T) {
	dnsConfig, outbounds, _, _, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{},
	}})
	if err != nil {
		t.Fatalf("build custom config: %v", err)
	}
	if dnsConfig.GetQueryStrategy() != dns.QueryStrategy_USE_IP4 {
		t.Fatalf("DNS query strategy = %s, want IPv4", dnsConfig.GetQueryStrategy())
	}
	if len(outbounds) == 0 || outbounds[0].ProxySettings == nil {
		t.Fatal("default freedom outbound is missing")
	}
	instance, err := outbounds[0].ProxySettings.GetInstance()
	if err != nil {
		t.Fatalf("decode freedom outbound: %v", err)
	}
	settings, ok := instance.(*freedom.Config)
	if !ok {
		t.Fatalf("default outbound settings type = %T", instance)
	}
	if settings.GetDomainStrategy() != internet.DomainStrategy_USE_IP4 {
		t.Fatalf("freedom domain strategy = %s, want IPv4", settings.GetDomainStrategy())
	}
}

func TestCustomRoutingRejectsMalformedOrUnsupportedRules(t *testing.T) {
	malformed := "{"
	for name, infos := range map[string][]*panel.NodeInfo{
		"nil node":       {nil},
		"missing common": {{Id: 1}},
		"malformed outbound": {{
			Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
				Id: 2, Action: "route", Match: []string{"example.com"}, ActionValue: &malformed,
			}}},
		}},
		"unknown action": {{
			Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
				Id: 3, Action: "future_fail_open_action",
			}}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, _, err := GetCustomConfig(infos); err == nil {
				t.Fatal("unsafe routing configuration was silently ignored")
			}
		})
	}
}

func TestWireGuardBalancerBuildsBalancingRuleAndHealthObserver(t *testing.T) {
	value := `{"tag":"wg-group","strategy":"roundRobin","outbounds":[` +
		`{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}},` +
		`{"tag":"wg-b","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.3/32"],"peers":[{"endpoint":"198.51.100.11:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
	_, outbounds, routes, observer, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
			Id: 7, Action: "route_wg_balancer", Match: []string{"domain:example.com"}, ActionValue: &value,
		}}},
	}})
	if err != nil {
		t.Fatalf("build WireGuard balancer: %v", err)
	}
	if len(routes.GetBalancingRule()) != 1 || routes.GetBalancingRule()[0].GetTag() != "__znode_wg_balancer_group_7" {
		t.Fatalf("balancing rules = %#v", routes.GetBalancingRule())
	}
	if len(routes.GetRule()) < 2 || routes.GetRule()[1].GetBalancingTag() != "__znode_wg_balancer_group_7" {
		t.Fatalf("route does not reference balancer: %#v", routes.GetRule())
	}
	if observer == nil || len(observer.GetSubjectSelector()) != 2 || observer.GetPingConfig().GetInterval() != int64(30*time.Second) {
		t.Fatalf("missing conservative health observer: %#v", observer)
	}
	for _, selector := range observer.GetSubjectSelector() {
		if !strings.HasPrefix(selector, "__znode_wg_balancer_7_") {
			t.Fatalf("unsafe prefix selector %q", selector)
		}
	}
	if len(outbounds) != 5 { // default, block, DNS and two WG outbounds
		t.Fatalf("outbounds = %d, want 5", len(outbounds))
	}
	if got := routes.GetBalancingRule()[0].GetFallbackTag(); got != "__znode_wg_balancer_7_1" {
		t.Fatalf("cold-start fallback = %q", got)
	}
}

func TestWireGuardBalancerUsesRouteIDForRuntimeTags(t *testing.T) {
	value := `{"tag":"legacy-same-tag","strategy":"roundRobin","outbounds":[` +
		`{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}},` +
		`{"tag":"wg-b","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.3/32"],"peers":[{"endpoint":"198.51.100.11:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
	_, _, routes, _, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{
			{Id: 7, Action: "route_wg_balancer", Match: []string{"domain:one.example"}, ActionValue: &value},
			{Id: 8, Action: "route_wg_balancer", Match: []string{"domain:two.example"}, ActionValue: &value},
		}},
	}})
	if err != nil {
		t.Fatalf("build duplicate legacy group tags: %v", err)
	}
	if len(routes.GetBalancingRule()) != 2 || routes.GetBalancingRule()[0].GetTag() == routes.GetBalancingRule()[1].GetTag() {
		t.Fatalf("runtime balancers aliased: %#v", routes.GetBalancingRule())
	}
}

func TestWireGuardBalancerBuildsLeastPingAndLeastLoad(t *testing.T) {
	for _, strategy := range []string{"leastPing", "leastLoad"} {
		t.Run(strategy, func(t *testing.T) {
			value := `{"tag":"wg-group","strategy":"` + strategy + `","outbounds":[` +
				`{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}},` +
				`{"tag":"wg-b","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.3/32"],"peers":[{"endpoint":"198.51.100.11:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
			_, _, routes, observer, err := GetCustomConfig([]*panel.NodeInfo{{
				Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
					Id: 9, Action: "route_wg_balancer", Match: []string{"domain:example.com"}, ActionValue: &value,
				}}},
			}})
			if err != nil {
				t.Fatalf("build %s balancer: %v", strategy, err)
			}
			if len(routes.GetBalancingRule()) != 1 || routes.GetBalancingRule()[0].GetStrategy() != strings.ToLower(strategy) {
				t.Fatalf("missing %s balancing strategy: %#v", strategy, routes.GetBalancingRule())
			}
			if observer == nil {
				t.Fatal("missing health observer")
			}
		})
	}
}

func TestWireGuardBalancerReusesRouteIDAcrossInbounds(t *testing.T) {
	value := `{"tag":"wg-group","strategy":"roundRobin","outbounds":[` +
		`{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}},` +
		`{"tag":"wg-b","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.3/32"],"peers":[{"endpoint":"198.51.100.11:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
	route := panel.Route{Id: 10, Action: "route_wg_balancer", Match: []string{"domain:example.com"}, ActionValue: &value}
	_, outbounds, routes, observer, err := GetCustomConfig([]*panel.NodeInfo{
		{Id: 1, Tag: "node-a", Common: &panel.CommonNode{Routes: []panel.Route{route}}},
		{Id: 2, Tag: "node-b", Common: &panel.CommonNode{Routes: []panel.Route{route}}},
	})
	if err != nil {
		t.Fatalf("reuse WireGuard balancer: %v", err)
	}
	if len(routes.GetBalancingRule()) != 1 || len(outbounds) != 5 || observer == nil {
		t.Fatalf("balancer was not reused: balancers=%d outbounds=%d observer=%#v", len(routes.GetBalancingRule()), len(outbounds), observer)
	}
	matched := 0
	for _, rule := range routes.GetRule() {
		if rule.GetBalancingTag() == "__znode_wg_balancer_group_10" {
			matched++
		}
	}
	if matched != 2 {
		t.Fatalf("balancer route rules = %d, want 2", matched)
	}
}

func TestWireGuardBalancerRejectsDuplicateOutboundTags(t *testing.T) {
	value := `{"tag":"wg-group","strategy":"roundRobin","outbounds":[{"tag":"wg-a","protocol":"wireguard"},{"tag":"wg-a","protocol":"wireguard"}]}`
	_, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{Action: "route_wg_balancer", ActionValue: &value}}},
	}})
	if err == nil {
		t.Fatal("duplicate WireGuard tags were accepted")
	}
}

func TestWireGuardBalancerValidatesNativeOutboundShape(t *testing.T) {
	base := `{"tag":"wg-group","strategy":"roundRobin","outbounds":[{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
	for name, mutate := range map[string]string{
		"missing settings":   `{"tag":"wg-group","strategy":"roundRobin","outbounds":[{"tag":"wg-a","protocol":"wireguard"}]}`,
		"missing address":    strings.Replace(base, `"address":["10.0.0.2/32"],`, "", 1),
		"null peer":          strings.Replace(base, `{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}`, "null", 1),
		"missing allowedIPs": strings.Replace(base, `,"allowedIPs":["0.0.0.0/0"]`, "", 1),
		"bad reserved":       strings.Replace(base, `}]}}]}`, `}],"reserved":"AQI="}}]}`, 1),
		"bad mtu":            strings.Replace(base, `}]}}]}`, `}],"mtu":9001}}]}`, 1),
		"stream settings":    strings.Replace(base, `,"settings":{`, `,"streamSettings":{},"settings":{`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{
				Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 20, Action: "route_wg_balancer", ActionValue: &mutate}}},
			}}); err == nil {
				t.Fatalf("invalid WireGuard shape was accepted")
			}
		})
	}
}

func TestWireGuardBalancerRejectsZeroAndConflictingRouteIDs(t *testing.T) {
	value := `{"tag":"wg-group","strategy":"roundRobin","outbounds":[{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}},{"tag":"wg-b","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.3/32"],"peers":[{"endpoint":"198.51.100.11:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
	if _, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{Action: "route_wg_balancer", ActionValue: &value}}}}}); err == nil {
		t.Fatal("zero route ID was accepted")
	}
	conflicting := strings.Replace(value, "198.51.100.11", "203.0.113.11", 1)
	if _, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{Id: 1, Tag: "node-a", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 21, Action: "route_wg_balancer", ActionValue: &value}}}}, {Id: 2, Tag: "node-b", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 21, Action: "route_wg_balancer", ActionValue: &conflicting}}}}}); err == nil {
		t.Fatal("conflicting route ID configuration was accepted")
	}
}

func TestWireGuardBalancerRejectsReservedGroupTags(t *testing.T) {
	for _, tag := range []string{"Default", "block", "dns_out"} {
		t.Run(tag, func(t *testing.T) {
			value := `{"tag":"` + tag + `","strategy":"roundRobin","outbounds":[{},{}]}`
			_, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{
				Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
					Action: "route_wg_balancer", ActionValue: &value,
				}}},
			}})
			if err == nil || !strings.Contains(err.Error(), "invalid WireGuard balancer tag") {
				t.Fatalf("reserved group tag %q accepted: %v", tag, err)
			}
		})
	}
}

func TestCustomOutboundCannotUseWireGuardBalancerPrefix(t *testing.T) {
	value := `{"tag":"__znode_wg_balancer_7_1_extra","protocol":"freedom","settings":{}}`
	_, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
			Action: "route", Match: []string{"domain:example.com"}, ActionValue: &value,
		}}},
	}})
	if err == nil || !strings.Contains(err.Error(), "reserved WireGuard balancer prefix") {
		t.Fatalf("reserved prefix accepted: %v", err)
	}
}

func TestEveryFreedomOutboundBlocksPrivateDestinationsFirst(t *testing.T) {
	raw := json.RawMessage(`{"domainStrategy":"UseIPv4","finalRules":[{"action":"allow"}]}`)
	outbound := &coreConf.OutboundDetourConfig{
		Protocol: "freedom",
		Tag:      "custom-direct",
		Settings: &raw,
	}
	if err := hardenFreedomOutbound(outbound); err != nil {
		t.Fatalf("harden freedom outbound: %v", err)
	}

	var settings struct {
		FinalRules []struct {
			Action string   `json:"action"`
			IP     []string `json:"ip"`
		} `json:"finalRules"`
	}
	if err := json.Unmarshal(*outbound.Settings, &settings); err != nil {
		t.Fatalf("decode hardened settings: %v", err)
	}
	if len(settings.FinalRules) < 2 || settings.FinalRules[0].Action != "block" {
		t.Fatalf("private block is not the first final rule: %#v", settings.FinalRules)
	}
	want := map[string]bool{
		"10.0.0.0/8":     false,
		"127.0.0.0/8":    false,
		"169.254.0.0/16": false,
		"192.168.0.0/16": false,
		"::/127":         false,
		"fc00::/7":       false,
		"fe80::/10":      false,
	}
	for _, cidr := range settings.FinalRules[0].IP {
		if _, ok := want[cidr]; ok {
			want[cidr] = true
		}
	}
	for cidr, found := range want {
		if !found {
			t.Fatalf("private block is missing %s: %#v", cidr, settings.FinalRules[0].IP)
		}
	}
}
