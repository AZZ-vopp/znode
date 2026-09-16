package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/xtls/xray-core/app/dns"
	xnet "github.com/xtls/xray-core/common/net"
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

func TestDefaultOutFallsBackToBuiltInOutboundWithoutActionValue(t *testing.T) {
	blank := "  \t"
	for name, actionValue := range map[string]*string{
		"nil":   nil,
		"blank": &blank,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, routes, _, err := GetCustomConfig([]*panel.NodeInfo{{
				Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{
					{Id: 1, Action: "default_out", ActionValue: actionValue},
					{Id: 2, Action: "block", Match: []string{"domain:blocked.example"}},
				}},
			}})
			if err != nil {
				t.Fatalf("build legacy default_out route: %v", err)
			}
			rules := routes.GetRule()
			if len(rules) != 3 { // DNS, the specific block rule, then the catch-all.
				t.Fatalf("rules = %d, want 3", len(rules))
			}
			if rules[1].GetTag() != "block" || len(rules[1].GetDomain()) == 0 {
				t.Fatalf("specific rule was not kept ahead of default_out: %#v", rules[1])
			}
			catchAll := rules[2]
			if got := catchAll.GetInboundTag(); len(got) != 1 || got[0] != "node" {
				t.Fatalf("default_out inbound tags = %#v, want [node]", got)
			}
			if got := catchAll.GetNetworks(); len(got) != 2 || got[0] != xnet.Network_TCP || got[1] != xnet.Network_UDP {
				t.Fatalf("default_out networks = %#v, want [TCP UDP]", got)
			}
			if got := catchAll.GetTag(); got != "Default" {
				t.Fatalf("default_out outbound tag = %q, want Default", got)
			}
		})
	}
}

func TestDefaultOutCanUseWireGuardBalancerWithoutDirectFallback(t *testing.T) {
	value := `{"tag":"wg-default","strategy":"roundRobin","outbounds":[` +
		`{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}},` +
		`{"tag":"wg-b","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.3/32"],"peers":[{"endpoint":"198.51.100.11:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
	_, outbounds, routes, observer, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{
			{Id: 1, Action: "block", Match: []string{"domain:blocked.example"}},
			{Id: 42, Action: "default_out", ActionValue: &value},
		}},
	}})
	if err != nil {
		t.Fatalf("build default WireGuard balancer: %v", err)
	}
	if len(routes.GetBalancingRule()) != 1 || routes.GetBalancingRule()[0].GetTag() != "__znode_wg_balancer_group_42" {
		t.Fatalf("balancing rules = %#v", routes.GetBalancingRule())
	}
	rules := routes.GetRule()
	if len(rules) < 2 || rules[len(rules)-2].GetTag() != "block" || len(rules[len(rules)-2].GetIp()) == 0 {
		t.Fatalf("IPv6 guard was not inserted before default WireGuard balancer: %#v", rules)
	}
	catchAll := rules[len(rules)-1]
	if got := catchAll.GetBalancingTag(); got != "__znode_wg_balancer_group_42" {
		t.Fatalf("default route balancer = %q", got)
	}
	if got := catchAll.GetTag(); got != "" {
		t.Fatalf("default route unexpectedly uses direct outbound %q", got)
	}
	if got := routes.GetBalancingRule()[0].GetFallbackTag(); got != "__znode_wg_balancer_42_1" {
		t.Fatalf("default WG fallback = %q", got)
	}
	if len(outbounds) != 5 || observer == nil {
		t.Fatalf("missing WireGuard outbounds or observer: outbounds=%d observer=%#v", len(outbounds), observer)
	}
}

func TestDefaultOutDirectWireGuardBlocksIPv6BeforeCatchAll(t *testing.T) {
	value := `{"tag":"wg-default","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}`
	_, _, routes, _, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{
			Id: 43, Action: "default_out", ActionValue: &value,
		}}},
	}})
	if err != nil {
		t.Fatalf("build direct WireGuard default: %v", err)
	}
	rules := routes.GetRule()
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want DNS, IPv6 guard and catch-all", len(rules))
	}
	guard := rules[1]
	if guard.GetTag() != "block" || len(guard.GetIp()) != 1 || len(guard.GetInboundTag()) != 1 || guard.GetInboundTag()[0] != "node" {
		t.Fatalf("invalid IPv6 guard: %#v", guard)
	}
	catchAll := rules[2]
	if catchAll.GetTag() != "wg-default" || len(catchAll.GetNetworks()) != 2 {
		t.Fatalf("invalid WireGuard catch-all: %#v", catchAll)
	}
}

func TestCustomRoutingRejectsMultipleDefaultOutRoutesForOneNode(t *testing.T) {
	blank := ""
	_, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{
		Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{
			{Id: 1, Action: "default_out", ActionValue: &blank},
			{Id: 2, Action: "default_out", ActionValue: &blank},
		}},
	}})
	if err == nil || !strings.Contains(err.Error(), "exactly one catch-all") {
		t.Fatalf("multiple default routes were accepted: %v", err)
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
	if got := observer.GetPingConfig().GetDestination(); got != wgBalancerProbeDestination {
		t.Fatalf("WireGuard health probe destination = %q, want %q", got, wgBalancerProbeDestination)
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

func TestWireGuardBalancerRequiresTwoOutboundsAndOnePeer(t *testing.T) {
	base := `{"tag":"wg-group","strategy":"roundRobin","outbounds":[{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
	if _, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 30, Action: "route_wg_balancer", ActionValue: &base}}}}}); err == nil || !strings.Contains(err.Error(), "2 to 8") {
		t.Fatalf("single WireGuard outbound was accepted: %v", err)
	}
	// Build a valid two-member group, then add a second peer to its first member.
	group := `{"tag":"wg-group","strategy":"roundRobin","outbounds":[` +
		`{"tag":"wg-a","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]},{"endpoint":"198.51.100.12:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}},` +
		`{"tag":"wg-b","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.3/32"],"peers":[{"endpoint":"198.51.100.11:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}]}}]}`
	if _, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 31, Action: "route_wg_balancer", ActionValue: &group}}}}}); err == nil || !strings.Contains(err.Error(), "exactly one peer") {
		t.Fatalf("multiple WireGuard peers were accepted: %v", err)
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

func TestDirectWireGuardOutboundUsesCanonicalNativeShape(t *testing.T) {
	base := `{"tag":"wg-direct","protocol":"wireguard","settings":{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0"]}],"mtu":1280,"reserved":[0,0,0]}}`
	for name, value := range map[string]string{
		"unknown setting": strings.Replace(base, `,"mtu":1280`, `,"unknown":true,"mtu":1280`, 1),
		"stream settings": strings.Replace(base, `,"settings":{`, `,"streamSettings":{},"settings":{`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, err := GetCustomConfig([]*panel.NodeInfo{{Id: 1, Tag: "node", Common: &panel.CommonNode{Routes: []panel.Route{{Id: 40, Action: "route", Match: []string{"domain:example.com"}, ActionValue: &value}}}}})
			if err == nil || !strings.Contains(err.Error(), "not supported") && !strings.Contains(err.Error(), "streamSettings") {
				t.Fatalf("non-canonical WireGuard outbound was accepted: %v", err)
			}
		})
	}
}

func TestWireGuardLegacyTUNFieldDoesNotForceGVisor(t *testing.T) {
	raw := json.RawMessage(`{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["10.0.0.2/32","2a07:b944::2:2/128"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["0.0.0.0/0","::/0"]}],"noKernelTun":false,"domainStrategy":"ForceIP","mtu":1280,"reserved":[0,0,0]}`)
	outbound := &coreConf.OutboundDetourConfig{Tag: "wg-direct", Protocol: "wireguard", Settings: &raw}
	if err := validateWireGuardOutbound(outbound); err != nil {
		t.Fatalf("normalize legacy WireGuard fields: %v", err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(*outbound.Settings, &settings); err != nil {
		t.Fatalf("decode normalized WireGuard settings: %v", err)
	}
	if _, exists := settings["noKernelTun"]; exists {
		t.Fatal("legacy noKernelTun still forces gVisor TUN")
	}
	if got := string(settings["domainStrategy"]); got != `"ForceIPv4"` {
		t.Fatalf("domainStrategy = %s, want ForceIPv4", got)
	}
	var addresses []string
	if err := json.Unmarshal(settings["address"], &addresses); err != nil {
		t.Fatalf("decode normalized address: %v", err)
	}
	if len(addresses) != 1 || addresses[0] != "10.0.0.2/32" {
		t.Fatalf("normalized address = %#v, want IPv4 only", addresses)
	}
	var peers []struct {
		AllowedIPs []string `json:"allowedIPs"`
	}
	if err := json.Unmarshal(settings["peers"], &peers); err != nil {
		t.Fatalf("decode normalized peers: %v", err)
	}
	if len(peers) != 1 || len(peers[0].AllowedIPs) != 1 || peers[0].AllowedIPs[0] != "0.0.0.0/0" {
		t.Fatalf("normalized allowedIPs = %#v, want IPv4 only", peers)
	}
}

func TestWireGuardRejectsIPv6OnlyConfig(t *testing.T) {
	raw := json.RawMessage(`{"secretKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","address":["2a07:b944::2:2/128"],"peers":[{"endpoint":"198.51.100.10:51820","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","allowedIPs":["::/0"]}]}`)
	outbound := &coreConf.OutboundDetourConfig{Tag: "wg-ipv6", Protocol: "wireguard", Settings: &raw}
	err := validateWireGuardOutbound(outbound)
	if err == nil || !strings.Contains(err.Error(), "IPv4") {
		t.Fatalf("IPv6-only WireGuard config was accepted: %v", err)
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
