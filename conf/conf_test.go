package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsAndRedisConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "type": "zboard",
  "Nodes": [{
    "ApiHost": "https://panel.example",
    "NodeID": 7,
    "ApiKey": "secret",
    "GlobalDeviceLimitConfig": {
      "Enable": true,
      "RedisAddr": "redis.example:6379",
      "RedisTLS": true,
      "Expiry": 90
    }
  }]
}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.LoadFromPath(path); err != nil {
		t.Fatal(err)
	}
	if c.ConnectionConfig.ConnIdle != 120 || c.ConnectionConfig.BufferSize != 128 || !c.ConnectionConfig.DisableUDPContentSniffing {
		t.Fatalf("unexpected connection defaults: %+v", c.ConnectionConfig)
	}
	device := c.NodeConfigs[0].GlobalDeviceLimitConfig
	if device == nil || device.RedisNetwork != "tcp" || device.Timeout != 1 || device.RefreshInterval != 30 || device.MaxIPsPerUser != 256 || device.SyncEnabled == nil || !*device.SyncEnabled {
		t.Fatalf("unexpected Redis defaults: %+v", device)
	}
	disabled := false
	custom := &GlobalDeviceLimitConfig{SyncEnabled: &disabled}
	custom.applyDefaults()
	if custom.Expiry != 60 || custom.RefreshInterval != 20 || custom.Timeout != 1 {
		t.Fatalf("unexpected balanced Redis defaults: %+v", custom)
	}
	if custom.SyncEnabled == nil || *custom.SyncEnabled {
		t.Fatalf("explicit SyncEnabled=false was not preserved")
	}
}

func TestLoadAgentConfigAndKeepManualModeCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	data := []byte(`{
  "type": "zboard",
  "Agent": {
    "Enable": true,
    "ApiHost": "https://panel.example/",
    "AgentID": "agent-123",
    "AgentToken": "secret",
    "GlobalDeviceLimitConfig": {
      "Enable": true,
      "RedisAddr": "redis.example:6379",
      "RedisTLS": true,
      "Expiry": 90
    }
  },
  "Nodes": []
}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.LoadFromPath(path); err != nil {
		t.Fatal(err)
	}
	if !c.AgentConfig.Enable || c.AgentConfig.APIHost != "https://panel.example" || c.AgentConfig.PollInterval != 15 {
		t.Fatalf("unexpected agent config: %+v", c.AgentConfig)
	}
	agentDevice := c.AgentConfig.GlobalDeviceLimitConfig
	if agentDevice == nil || agentDevice.RedisNetwork != "tcp" || agentDevice.RefreshInterval != 30 || agentDevice.SyncEnabled == nil || !*agentDevice.SyncEnabled {
		t.Fatalf("unexpected agent Redis defaults: %+v", agentDevice)
	}

	manualPath := filepath.Join(t.TempDir(), "manual.json")
	if err := os.WriteFile(manualPath, []byte(`{"type":"zboard","Nodes":[{"ApiHost":"https://panel.example","NodeID":9,"ApiKey":"legacy"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	manual := New()
	if err := manual.LoadFromPath(manualPath); err != nil {
		t.Fatal(err)
	}
	if manual.AgentConfig.Enable || len(manual.NodeConfigs) != 1 || manual.NodeConfigs[0].Key != "legacy" {
		t.Fatalf("manual Nodes mode changed unexpectedly: %+v", manual)
	}
}

func TestRemoteRedisRequiresVerifiedTLS(t *testing.T) {
	plain := &GlobalDeviceLimitConfig{
		Enable: true, RedisNetwork: "tcp", RedisAddr: "redis.example:6379",
	}
	if _, err := RedisTLSConfig(plain); err == nil {
		t.Fatal("remote Redis was allowed to receive credentials without TLS")
	}

	secure := *plain
	secure.RedisTLS = true
	secure.RedisTLSServerName = "redis.example"
	tlsConfig, err := RedisTLSConfig(&secure)
	if err != nil {
		t.Fatalf("verified Redis TLS was rejected: %v", err)
	}
	if tlsConfig == nil || tlsConfig.MinVersion == 0 || tlsConfig.ServerName != "redis.example" {
		t.Fatalf("unsafe Redis TLS config: %#v", tlsConfig)
	}

	loopback := *plain
	loopback.RedisAddr = "127.0.0.1:6379"
	if tlsConfig, err := RedisTLSConfig(&loopback); err != nil || tlsConfig != nil {
		t.Fatalf("numeric loopback Redis should remain usable without TLS: config=%#v err=%v", tlsConfig, err)
	}
}

func TestAgentConfigRequiresCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid-agent.json")
	if err := os.WriteFile(path, []byte(`{"type":"zboard","Agent":{"Enable":true,"ApiHost":"https://panel.example"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.LoadFromPath(path); err == nil {
		t.Fatal("expected missing agent credentials error")
	}
}

func TestConfigRequiresZBoardType(t *testing.T) {
	for name, payload := range map[string]string{
		"missing": `{"Nodes":[]}`,
		"wrong":   `{"type":"v2board","Nodes":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
				t.Fatal(err)
			}
			if err := New().LoadFromPath(path); err == nil {
				t.Fatalf("expected config type %s to be rejected", name)
			}
		})
	}
}

func TestPanelAPIHostRequiresHTTPSOutsideNumericLoopback(t *testing.T) {
	for name, raw := range map[string]string{
		"remote http":        "http://panel.example",
		"userinfo":           "https://user:pass@panel.example",
		"query":              "https://panel.example?token=value",
		"fragment":           "https://panel.example/#fragment",
		"localhost hostname": "http://localhost:8080",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizePanelAPIHost(raw); err == nil {
				t.Fatalf("expected insecure ApiHost %q to be rejected", raw)
			}
		})
	}

	for _, raw := range []string{
		"https://panel.example/",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	} {
		if _, err := NormalizePanelAPIHost(raw); err != nil {
			t.Fatalf("expected ApiHost %q to be accepted: %v", raw, err)
		}
	}
}

func TestLoadRejectsInsecureAgentAndManualPanelHosts(t *testing.T) {
	for name, payload := range map[string]string{
		"agent":  `{"type":"zboard","Agent":{"Enable":true,"ApiHost":"http://panel.example","AgentID":"agent","AgentToken":"secret"}}`,
		"manual": `{"type":"zboard","Nodes":[{"ApiHost":"http://panel.example","NodeID":9,"ApiKey":"legacy"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
				t.Fatal(err)
			}
			if err := New().LoadFromPath(path); err == nil {
				t.Fatal("expected insecure panel ApiHost to be rejected")
			}
		})
	}
}
