package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsAndRedisConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "Nodes": [{
    "ApiHost": "https://panel.example",
    "NodeID": 7,
    "ApiKey": "secret",
    "GlobalDeviceLimitConfig": {
      "Enable": true,
      "RedisAddr": "redis.example:6379",
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
	if c.ConnectionConfig.ConnIdle != 30 || c.ConnectionConfig.BufferSize != 16 || !c.ConnectionConfig.DisableUDPContentSniffing {
		t.Fatalf("unexpected connection defaults: %+v", c.ConnectionConfig)
	}
	device := c.NodeConfigs[0].GlobalDeviceLimitConfig
	if device == nil || device.RedisNetwork != "tcp" || device.Timeout != 2 || device.RefreshInterval != 30 || device.MaxIPsPerUser != 256 || device.SyncEnabled == nil || !*device.SyncEnabled {
		t.Fatalf("unexpected Redis defaults: %+v", device)
	}
	disabled := false
	custom := &GlobalDeviceLimitConfig{SyncEnabled: &disabled}
	custom.applyDefaults()
	if custom.SyncEnabled == nil || *custom.SyncEnabled {
		t.Fatalf("explicit SyncEnabled=false was not preserved")
	}
}

func TestLoadAgentConfigAndKeepManualModeCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	data := []byte(`{
  "Agent": {
    "Enable": true,
    "ApiHost": "https://panel.example/",
    "AgentID": "agent-123",
    "AgentToken": "secret",
    "GlobalDeviceLimitConfig": {
      "Enable": true,
      "RedisAddr": "redis.example:6379",
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
	if err := os.WriteFile(manualPath, []byte(`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":9,"ApiKey":"legacy"}]}`), 0600); err != nil {
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

func TestAgentConfigRequiresCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid-agent.json")
	if err := os.WriteFile(path, []byte(`{"Agent":{"Enable":true,"ApiHost":"https://panel.example"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.LoadFromPath(path); err == nil {
		t.Fatal("expected missing agent credentials error")
	}
}
