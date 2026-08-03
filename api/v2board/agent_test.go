package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AZZ-vopp/znode/conf"
)

func TestAgentClientGetsManifestWithCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentManifestPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-ZNode-Agent-ID"); got != "agent-123" {
			t.Fatalf("unexpected agent ID header: %q", got)
		}
		if got := r.Header.Get("X-ZNode-Agent-Token"); got != "top-secret" {
			t.Fatalf("unexpected agent token header: %q", got)
		}
		if got := r.Header.Get("X-ZNode-Type"); got != conf.RequiredPanelType {
			t.Fatalf("unexpected ZBoard type header: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer top-secret" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		if got := r.URL.Query().Get("agent_id"); got != "" {
			t.Fatalf("agent ID leaked into query string: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"panel_type":"zboard","revision":"rev-9","node_revision":"nodes-9","fallback_revision":"fallback-9","nodes":[12,18],"poll_interval":7}`))
	}))
	defer server.Close()

	client, err := NewAgentClient(conf.AgentConfig{
		Enable:       true,
		APIHost:      server.URL,
		AgentID:      "agent-123",
		AgentToken:   "top-secret",
		PollInterval: 15,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := client.GetManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.EffectiveRevision() != "rev-9" || len(manifest.Nodes) != 2 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if manifest.EffectiveNodeRevision() != "nodes-9" || manifest.EffectiveFallbackRevision() != "fallback-9" {
		t.Fatalf("unexpected component revisions: %+v", manifest)
	}
	if manifest.EffectivePollInterval(15) != 7*time.Second {
		t.Fatalf("unexpected poll interval: %s", manifest.EffectivePollInterval(15))
	}

	deviceLimit := &conf.GlobalDeviceLimitConfig{Enable: true, RedisAddr: "redis.example:6379"}
	nodes := manifest.NodeConfigs(conf.AgentConfig{
		APIHost:                 server.URL,
		AgentID:                 "agent-123",
		AgentToken:              "top-secret",
		GlobalDeviceLimitConfig: deviceLimit,
	})
	if len(nodes) != 2 || nodes[0].NodeID != 12 || nodes[1].NodeID != 18 {
		t.Fatalf("unexpected node configs: %+v", nodes)
	}
	if nodes[0].AgentID != "agent-123" || nodes[0].Key != "top-secret" {
		t.Fatalf("agent credentials were not propagated: %+v", nodes[0])
	}
	if nodes[0].GlobalDeviceLimitConfig == nil || nodes[1].GlobalDeviceLimitConfig == nil ||
		nodes[0].GlobalDeviceLimitConfig.RedisAddr != "redis.example:6379" {
		t.Fatalf("agent Redis device-limit config was not propagated: %+v", nodes)
	}
	if nodes[0].GlobalDeviceLimitConfig == nodes[1].GlobalDeviceLimitConfig || nodes[0].GlobalDeviceLimitConfig == deviceLimit {
		t.Fatal("logical nodes unexpectedly share a mutable Redis config pointer")
	}
}

func TestAgentManifestValidationAndStableFallbackRevision(t *testing.T) {
	if err := (&AgentManifest{PanelType: conf.RequiredPanelType, Nodes: []int{2, 2}}).Validate(); err == nil {
		t.Fatal("expected duplicate node ID error")
	}
	if err := (&AgentManifest{PanelType: conf.RequiredPanelType, Nodes: []int{0}}).Validate(); err == nil {
		t.Fatal("expected invalid node ID error")
	}

	first := (&AgentManifest{Nodes: []int{7, 3}}).EffectiveRevision()
	second := (&AgentManifest{Nodes: []int{3, 7}}).EffectiveRevision()
	if first == "" || first != second {
		t.Fatalf("fallback revision is not stable: %q != %q", first, second)
	}
	if got := (&AgentManifest{PollInterval: 1}).EffectivePollInterval(15); got != 5*time.Second {
		t.Fatalf("poll interval was not clamped: %s", got)
	}
	if err := (&AgentManifest{Nodes: []int{1}}).Validate(); err == nil {
		t.Fatal("expected a manifest without the ZBoard panel type to be rejected")
	}
}

func TestAgentClientRejectsHTTP200AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"fail","message":"token is error"}`))
	}))
	defer server.Close()

	client, err := NewAgentClient(conf.AgentConfig{
		Enable: true, APIHost: server.URL, AgentID: "agent", AgentToken: "wrong",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetManifest(context.Background()); err == nil {
		t.Fatal("expected HTTP 200 auth failure to be rejected")
	}
}

func TestAgentClientFailsClosedWhenAuthorizationIsRevoked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(agentAuthorizationHeader, "revoked")
		http.Error(w, `{"status":"fail","message":"token is error"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	client, err := NewAgentClient(conf.AgentConfig{
		Enable: true, APIHost: server.URL, AgentID: "agent", AgentToken: "revoked",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := client.GetManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.AuthorizationRevoked || len(manifest.Nodes) != 0 || manifest.EffectiveRevision() == "" {
		t.Fatalf("authorization denial did not produce a revoked empty manifest: %+v", manifest)
	}
}

func TestAgentClientKeepsRuntimeForGenericCDNAuthorizationPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cloud edge temporarily unavailable", http.StatusForbidden)
	}))
	defer server.Close()

	client, err := NewAgentClient(conf.AgentConfig{
		Enable: true, APIHost: server.URL, AgentID: "agent", AgentToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest, err := client.GetManifest(context.Background()); err == nil || manifest != nil {
		t.Fatalf("generic edge denial was treated as an authoritative revocation: manifest=%+v err=%v", manifest, err)
	}
}

func TestLogicalNodeClientUsesAgentHeadersWithoutTokenQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-ZNode-Agent-ID"); got != "agent-123" {
			t.Fatalf("unexpected agent ID header: %q", got)
		}
		if got := r.Header.Get("X-ZNode-Agent-Token"); got != "top-secret" {
			t.Fatalf("unexpected agent token header: %q", got)
		}
		if got := r.Header.Get("X-ZNode-Type"); got != conf.RequiredPanelType {
			t.Fatalf("unexpected ZBoard type header: %q", got)
		}
		if got := r.URL.Query().Get("type"); got != conf.RequiredPanelType {
			t.Fatalf("unexpected ZBoard type query: %q", got)
		}
		if got := r.URL.Query().Get("token"); got != "" {
			t.Fatalf("agent token leaked into query string: %q", got)
		}
		if got := r.URL.Query().Get("agent_id"); got != "" {
			t.Fatalf("agent ID leaked into query string: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := New(&conf.NodeConfig{
		APIHost: server.URL,
		NodeID:  12,
		Key:     "top-secret",
		AgentID: "agent-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.client.R().Get("/probe")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode() != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", response.StatusCode())
	}
}

func TestLogicalNodeClientDoesNotLeakAgentTokenAcrossRedirects(t *testing.T) {
	leaked := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		leaked <- request.Header.Get("X-ZNode-Agent-Token")
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirector.Close()

	client, err := New(&conf.NodeConfig{
		APIHost: redirector.URL, NodeID: 12, Key: "top-secret", AgentID: "agent-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.client.R().Get("/redirect"); err == nil {
		t.Fatal("expected panel redirect to be rejected")
	}
	select {
	case token := <-leaked:
		t.Fatalf("redirect target received agent token %q", token)
	default:
	}
}

func TestAgentManifestRejectsResourceExhaustionPayloads(t *testing.T) {
	manifest := &AgentManifest{PanelType: conf.RequiredPanelType, Nodes: make([]int, maxAgentNodes+1)}
	if err := manifest.Validate(); err == nil {
		t.Fatal("oversized agent assignment was accepted")
	}
	manifest = &AgentManifest{PanelType: conf.RequiredPanelType, Revision: strings.Repeat("x", 257)}
	if err := manifest.Validate(); err == nil {
		t.Fatal("oversized agent revision was accepted")
	}
	if got := (&AgentManifest{PollInterval: 999999}).EffectivePollInterval(15); got != time.Hour {
		t.Fatalf("oversized polling interval was not capped: %s", got)
	}
}
