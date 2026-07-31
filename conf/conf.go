package conf

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15
const RequiredPanelType = "zboard"

type Conf struct {
	Type             string           `mapstructure:"type"`
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
// assigned by ZBoard. Nodes remains the manual ZNode mode when
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
// BufferSize is in KiB. Defaults keep enough headroom for UDP/QUIC video while
// remaining below Xray's amd64 default.
type ConnectionConfig struct {
	Handshake                 uint32 `mapstructure:"Handshake"`
	ConnIdle                  uint32 `mapstructure:"ConnIdle"`
	UplinkOnly                uint32 `mapstructure:"UplinkOnly"`
	DownlinkOnly              uint32 `mapstructure:"DownlinkOnly"`
	BufferSize                int32  `mapstructure:"BufferSize"`
	DisableUDPContentSniffing bool   `mapstructure:"DisableUDPContentSniffing"`
	MaxConnectionsPerUser     int    `mapstructure:"MaxConnectionsPerUser"`
	MaxConnections            int    `mapstructure:"MaxConnections"`
}

// GlobalDeviceLimitConfig enables a Redis-backed, cross-node device/IP limit.
// If Redis is unavailable, FailClosed=false keeps traffic available and falls
// back to the bounded local tracker.
type GlobalDeviceLimitConfig struct {
	Enable             bool   `mapstructure:"Enable"`
	RedisNetwork       string `mapstructure:"RedisNetwork"`
	RedisAddr          string `mapstructure:"RedisAddr"`
	RedisUsername      string `mapstructure:"RedisUsername"`
	RedisPassword      string `mapstructure:"RedisPassword"`
	RedisDB            int    `mapstructure:"RedisDB"`
	RedisTLS           bool   `mapstructure:"RedisTLS"`
	RedisTLSServerName string `mapstructure:"RedisTLSServerName"`
	RedisTLSCAFile     string `mapstructure:"RedisTLSCAFile"`
	Timeout            int    `mapstructure:"Timeout"`
	Expiry             int    `mapstructure:"Expiry"`
	RefreshInterval    int    `mapstructure:"RefreshInterval"`
	MaxIPsPerUser      int    `mapstructure:"MaxIPsPerUser"`
	KeyPrefix          string `mapstructure:"KeyPrefix"`
	FailClosed         bool   `mapstructure:"FailClosed"`
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
			Handshake:    4,
			ConnIdle:     120,
			UplinkOnly:   2,
			DownlinkOnly: 4,
			BufferSize:   128,
			// Domain routing still sniffs TCP/TLS. Do not hold QUIC datagrams while
			// attempting content inspection; mobile video apps are sensitive to the
			// added first-packet delay on high-latency China-to-Vietnam links.
			DisableUDPContentSniffing: true,
			MaxConnectionsPerUser:     128,
			MaxConnections:            32768,
		},
		AgentConfig: AgentConfig{PollInterval: 15},
	}
}

func (p *Conf) LoadFromPath(filePath string) error {
	f, err := openSecureConfigFile(filePath)
	if err != nil {
		return fmt.Errorf("open secure config file error: %s", err)
	}
	defer f.Close()
	v := viper.New()
	configType := strings.TrimPrefix(strings.ToLower(filepath.Ext(filePath)), ".")
	if configType == "yml" {
		configType = "yaml"
	}
	switch configType {
	case "json", "yaml", "toml":
		v.SetConfigType(configType)
	default:
		return fmt.Errorf("unsupported config file type %q", configType)
	}
	// Parse the already-verified descriptor. Reopening the pathname here would
	// reintroduce a symlink/swap race after the permission and owner checks.
	if err := v.ReadConfig(f); err != nil {
		return fmt.Errorf("read config file error: %s", err)
	}
	if err := v.Unmarshal(p); err != nil {
		return fmt.Errorf("unmarshal config error: %s", err)
	}
	p.Type = strings.ToLower(strings.TrimSpace(p.Type))
	if p.Type != RequiredPanelType {
		return fmt.Errorf("invalid config type %q: ZNode requires type=%s", p.Type, RequiredPanelType)
	}
	if err := p.AgentConfig.applyDefaultsAndValidate(); err != nil {
		return err
	}
	for i := range p.NodeConfigs {
		apiHost, err := NormalizePanelAPIHost(p.NodeConfigs[i].APIHost)
		if err != nil {
			return fmt.Errorf("node config %d: %w", i, err)
		}
		p.NodeConfigs[i].APIHost = apiHost
		if p.NodeConfigs[i].RetryCount == nil {
			p.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
		if p.NodeConfigs[i].GlobalDeviceLimitConfig != nil {
			p.NodeConfigs[i].GlobalDeviceLimitConfig.applyDefaults()
			if _, err := RedisTLSConfig(p.NodeConfigs[i].GlobalDeviceLimitConfig); err != nil {
				return fmt.Errorf("node config %d Redis: %w", i, err)
			}
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
		if _, err := RedisTLSConfig(c.GlobalDeviceLimitConfig); err != nil {
			return fmt.Errorf("agent Redis config: %w", err)
		}
	}
	if !c.Enable {
		return nil
	}
	if c.APIHost == "" {
		return fmt.Errorf("agent config error: ApiHost is required when Agent.Enable is true")
	}
	apiHost, err := NormalizePanelAPIHost(c.APIHost)
	if err != nil {
		return fmt.Errorf("agent config error: %w", err)
	}
	c.APIHost = apiHost
	if c.AgentID == "" {
		return fmt.Errorf("agent config error: AgentID is required when Agent.Enable is true")
	}
	if c.AgentToken == "" {
		return fmt.Errorf("agent config error: AgentToken is required when Agent.Enable is true")
	}
	return nil
}

// NormalizePanelAPIHost validates the transport used for panel credentials.
// HTTPS is mandatory for remote panels. Plain HTTP is accepted only for a
// numeric loopback address so isolated local tests and local-only development
// do not weaken production traffic.
func NormalizePanelAPIHost(raw string) (string, error) {
	host := strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(host)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("ApiHost must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("ApiHost must not contain credentials, a query, or a fragment")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "https" {
		return host, nil
	}
	if scheme == "http" {
		ip := net.ParseIP(parsed.Hostname())
		if ip != nil && ip.IsLoopback() {
			return host, nil
		}
	}
	return "", fmt.Errorf("ApiHost must use HTTPS (plain HTTP is allowed only on a numeric loopback address)")
}

func (c *ConnectionConfig) applyDefaults() {
	if c.Handshake == 0 {
		c.Handshake = 4
	}
	if c.ConnIdle == 0 {
		c.ConnIdle = 120
	}
	if c.UplinkOnly == 0 {
		c.UplinkOnly = 2
	}
	if c.DownlinkOnly == 0 {
		c.DownlinkOnly = 4
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 128
	}
	if c.MaxConnectionsPerUser <= 0 {
		c.MaxConnectionsPerUser = 128
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = 32768
	}
	if c.MaxConnectionsPerUser > 4096 {
		c.MaxConnectionsPerUser = 4096
	}
	if c.MaxConnections > 262144 {
		c.MaxConnections = 262144
	}
	if c.MaxConnections < c.MaxConnectionsPerUser {
		c.MaxConnections = c.MaxConnectionsPerUser
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
		c.Timeout = 1
	}
	if c.Expiry <= 0 {
		c.Expiry = 60
	}
	if c.Expiry < 10 {
		c.Expiry = 10
	}
	if c.RefreshInterval <= 0 || c.RefreshInterval >= c.Expiry {
		c.RefreshInterval = c.Expiry / 3
		if c.RefreshInterval < 5 {
			c.RefreshInterval = 5
		}
	}
	if c.RefreshInterval < 5 {
		c.RefreshInterval = 5
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
