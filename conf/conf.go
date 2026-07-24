package conf

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

type Conf struct {
	LogConfig        LogConfig        `mapstructure:"Log"`
	NodeConfigs      []NodeConfig     `mapstructure:"Nodes"`
	AgentConfig      AgentConfig      `mapstructure:"Agent"`
	PprofPort        int              `mapstructure:"PprofPort"`
	ConnectionConfig ConnectionConfig `mapstructure:"ConnectionConfig"`
}

type LogConfig struct {
	Level  string `mapstructure:"Level"`
	Output string `mapstructure:"Output"`
	Access string `mapstructure:"Access"`
}

type NodeConfig struct {
	APIHost                 string                   `mapstructure:"ApiHost"`
	NodeID                  int                      `mapstructure:"NodeID"`
	Key                     string                   `mapstructure:"ApiKey"`
	AgentID                 string                   `mapstructure:"AgentID"`
	AgentInstanceID         string                   `mapstructure:"AgentInstanceID"`
	Timeout                 int                      `mapstructure:"Timeout"`
	RetryCount              *int                     `mapstructure:"RetryCount"`
	GlobalDeviceLimitConfig *GlobalDeviceLimitConfig `mapstructure:"GlobalDeviceLimitConfig"`
}

// AgentConfig lets one znode process receive and run multiple logical nodes
// assigned by V2Board. Nodes remains the backward-compatible manual mode when
// Agent.Enable is false.
type AgentConfig struct {
	Enable                  bool                     `mapstructure:"Enable"`
	APIHost                 string                   `mapstructure:"ApiHost"`
	AgentID                 string                   `mapstructure:"AgentID"`
	AgentInstanceID         string                   `mapstructure:"AgentInstanceID"`
	AgentToken              string                   `mapstructure:"AgentToken"`
	PollInterval            int                      `mapstructure:"PollInterval"`
	GlobalDeviceLimitConfig *GlobalDeviceLimitConfig `mapstructure:"GlobalDeviceLimitConfig"`
}

// ConnectionConfig controls the Xray policy applied to every inbound session.
// BufferSize is in KiB. The defaults intentionally match the lower-memory
// profile used by XrayR instead of the old 128 KiB per-connection setting.
type ConnectionConfig struct {
	Handshake                 uint32 `mapstructure:"Handshake"`
	ConnIdle                  uint32 `mapstructure:"ConnIdle"`
	UplinkOnly                uint32 `mapstructure:"UplinkOnly"`
	DownlinkOnly              uint32 `mapstructure:"DownlinkOnly"`
	BufferSize                int32  `mapstructure:"BufferSize"`
	DisableUDPContentSniffing bool   `mapstructure:"DisableUDPContentSniffing"`
}

// GlobalDeviceLimitConfig enables a Redis-backed, cross-node device/IP limit.
// If Redis is unavailable, FailClosed=false keeps traffic available and falls
// back to the bounded local tracker.
type GlobalDeviceLimitConfig struct {
	Enable          bool   `mapstructure:"Enable"`
	RedisNetwork    string `mapstructure:"RedisNetwork"`
	RedisAddr       string `mapstructure:"RedisAddr"`
	RedisUsername   string `mapstructure:"RedisUsername"`
	RedisPassword   string `mapstructure:"RedisPassword"`
	RedisDB         int    `mapstructure:"RedisDB"`
	Timeout         int    `mapstructure:"Timeout"`
	Expiry          int    `mapstructure:"Expiry"`
	RefreshInterval int    `mapstructure:"RefreshInterval"`
	MaxIPsPerUser   int    `mapstructure:"MaxIPsPerUser"`
	KeyPrefix       string `mapstructure:"KeyPrefix"`
	FailClosed      bool   `mapstructure:"FailClosed"`
	// Pointer allows omitted SyncEnabled to default to true while still
	// honoring an explicit false in a node config.
	SyncEnabled *bool  `mapstructure:"SyncEnabled"`
	SyncChannel string `mapstructure:"SyncChannel"`
}

func New() *Conf {
	return &Conf{
		LogConfig: LogConfig{
			Level:  "info",
			Output: "",
			Access: "none",
		},
		ConnectionConfig: ConnectionConfig{
			Handshake:                 4,
			ConnIdle:                  30,
			UplinkOnly:                2,
			DownlinkOnly:              4,
			BufferSize:                16,
			DisableUDPContentSniffing: true,
		},
		AgentConfig: AgentConfig{PollInterval: 15},
	}
}

func (p *Conf) LoadFromPath(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open config file error: %s", err)
	}
	defer f.Close()
	v := viper.New()
	v.SetConfigFile(filePath)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config file error: %s", err)
	}
	if err := v.Unmarshal(p); err != nil {
		return fmt.Errorf("unmarshal config error: %s", err)
	}
	if err := p.AgentConfig.applyDefaultsAndValidate(); err != nil {
		return err
	}
	for i := range p.NodeConfigs {
		if p.NodeConfigs[i].RetryCount == nil {
			p.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
		if p.NodeConfigs[i].GlobalDeviceLimitConfig != nil {
			p.NodeConfigs[i].GlobalDeviceLimitConfig.applyDefaults()
		}
	}
	p.ConnectionConfig.applyDefaults()
	return nil
}

func (c *AgentConfig) applyDefaultsAndValidate() error {
	c.APIHost = strings.TrimRight(strings.TrimSpace(c.APIHost), "/")
	c.AgentID = strings.TrimSpace(c.AgentID)
	c.AgentInstanceID = strings.TrimSpace(c.AgentInstanceID)
	c.AgentToken = strings.TrimSpace(c.AgentToken)
	if c.PollInterval <= 0 {
		c.PollInterval = 15
	}
	if c.GlobalDeviceLimitConfig != nil {
		c.GlobalDeviceLimitConfig.applyDefaults()
	}
	if !c.Enable {
		return nil
	}
	if c.APIHost == "" {
		return fmt.Errorf("agent config error: ApiHost is required when Agent.Enable is true")
	}
	if c.AgentID == "" {
		return fmt.Errorf("agent config error: AgentID is required when Agent.Enable is true")
	}
	if c.AgentToken == "" {
		return fmt.Errorf("agent config error: AgentToken is required when Agent.Enable is true")
	}
	return nil
}

func (c *ConnectionConfig) applyDefaults() {
	if c.Handshake == 0 {
		c.Handshake = 4
	}
	if c.ConnIdle == 0 {
		c.ConnIdle = 30
	}
	if c.UplinkOnly == 0 {
		c.UplinkOnly = 2
	}
	if c.DownlinkOnly == 0 {
		c.DownlinkOnly = 4
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 16
	}
}

func (c *GlobalDeviceLimitConfig) applyDefaults() {
	if c.RedisNetwork == "" {
		c.RedisNetwork = "tcp"
	}
	if c.RedisAddr == "" {
		c.RedisAddr = "127.0.0.1:6379"
	}
	if c.Timeout <= 0 {
		c.Timeout = 2
	}
	if c.Expiry <= 0 {
		c.Expiry = 120
	}
	if c.Expiry < 10 {
		c.Expiry = 10
	}
	if c.RefreshInterval <= 0 || c.RefreshInterval >= c.Expiry {
		c.RefreshInterval = c.Expiry / 3
		if c.RefreshInterval < 1 {
			c.RefreshInterval = 1
		}
	}
	if c.MaxIPsPerUser <= 0 {
		c.MaxIPsPerUser = 256
	}
	if c.KeyPrefix == "" {
		c.KeyPrefix = "znode:device"
	}
	if c.SyncChannel == "" {
		c.SyncChannel = "v2board:device-sync"
	}
	// Sync is enabled by default whenever the Redis device limiter is enabled;
	// this removes the panel pull-interval delay for new/deleted device UUIDs.
	if c.SyncEnabled == nil {
		enabled := true
		c.SyncEnabled = &enabled
	}
}

func intPtr(v int) *int {
	return &v
}
