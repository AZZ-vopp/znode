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

func TestNodeConfigRequiresZBoardPanelIdentity(t *testing.T) {
	for name, panelType := range map[string]string{
		"missing": "",
		"wrong":   "v2board",
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("X-ZNode-Type") != conf.RequiredPanelType || request.URL.Query().Get("type") != conf.RequiredPanelType {
					t.Error("ZNode did not identify itself as a ZBoard client")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"panel_type":"` + panelType + `","protocol":"vmess","listen_ip":"127.0.0.1","server_port":443,"network":"tcp","tls":0}`))
			}))
			defer server.Close()

			client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "token", AgentID: "agent-a"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.GetNodeInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "panel_type") {
				t.Fatalf("expected incompatible panel identity to be rejected, got %v", err)
			}
		})
	}
}

func TestNodeClientRejectsLegacyManualTokenBeforeAnyRequest(t *testing.T) {
	if _, err := New(&conf.NodeConfig{
		APIHost: "https://panel.example",
		NodeID:  1,
		Key:     "legacy-global-token",
	}); err == nil || !strings.Contains(err.Error(), "legacy manual/global tokens are disabled") {
		t.Fatalf("expected legacy credentials to be rejected, got %v", err)
	}
}

func TestNodeConfigWithoutBaseConfigUsesSafeIntervals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"panel_type":"zboard","protocol":"vmess","listen_ip":"127.0.0.1","server_port":443,"network":"tcp","tls":0}`))
	}))
	defer server.Close()

	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "token", AgentID: "agent-a"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if node.PushInterval != time.Minute || node.PullInterval != time.Minute {
		t.Fatalf("unsafe default intervals: push=%s pull=%s", node.PushInterval, node.PullInterval)
	}
}

func TestNodeConfigRejectsExecutableDNSProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"panel_type":"zboard","protocol":"trojan","listen_ip":"127.0.0.1",
			"server_port":443,"network":"tcp","tls":1,
			"tls_settings":{"cert_mode":"dns","provider":"exec","dns_env":"EXEC_PATH=/tmp/payload"}
		}`))
	}))
	defer server.Close()

	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "token", AgentID: "agent-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetNodeInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported DNS provider") {
		t.Fatalf("expected executable DNS provider to be rejected, got %v", err)
	}
}

func TestNodeConfigRejectsUnknownOrCertificateLessTLS(t *testing.T) {
	for name, security := range map[string]string{
		"unknown security":     `"tls":99`,
		"certificate-less TLS": `"tls":1,"tls_settings":{"cert_mode":"none"}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"panel_type":"zboard","protocol":"vmess","listen_ip":"127.0.0.1","server_port":443,"network":"tcp",` + security + `}`))
			}))
			defer server.Close()
			client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 1, Key: "token", AgentID: "agent-a"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.GetNodeInfo(context.Background()); err == nil {
				t.Fatal("unsafe transport security configuration was accepted")
			}
		})
	}
}
