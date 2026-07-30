package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AZZ-vopp/znode/conf"

	"encoding/json"
)

// FlexibleUint64 accepts both numeric JSON and the legacy quoted xver value.
type FlexibleUint64 uint64

func effectiveCertMode(security int, mode string) string {
	mode = strings.TrimSpace(mode)
	if security == Tls && mode == "" {
		return "self"
	}
	return mode
}

func (v *FlexibleUint64) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(string(data))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid uint64 value %q: %w", value, err)
	}
	*v = FlexibleUint64(parsed)
	return nil
}

// Security type
const (
	None    = 0
	Tls     = 1
	Reality = 2
)

type NodeInfo struct {
	Id           int
	Type         string
	Security     int
	PushInterval time.Duration
	PullInterval time.Duration
	Tag          string
	Common       *CommonNode
}

type CommonNode struct {
	PanelType  string      `json:"panel_type"`
	Protocol   string      `json:"protocol"`
	ListenIP   string      `json:"listen_ip"`
	ServerPort int         `json:"server_port"`
	Routes     []Route     `json:"routes"`
	BaseConfig *BaseConfig `json:"base_config"`
	//vless vmess trojan
	Tls                  int         `json:"tls"`
	TlsSettings          TlsSettings `json:"tls_settings"`
	CertInfo             *CertInfo
	Network              string          `json:"network"`
	NetworkSettings      json.RawMessage `json:"network_settings"`
	TrustedXForwardedFor []string        `json:"trusted_x_forwarded_for"`
	Encryption           string          `json:"encryption"`
	EncryptionSettings   EncSettings     `json:"encryption_settings"`
	ServerName           string          `json:"server_name"`
	Flow                 string          `json:"flow"`
	//shadowsocks
	Cipher    string `json:"cipher"`
	ServerKey string `json:"server_key"`
	//tuic
	CongestionControl string `json:"congestion_control"`
	ZeroRTTHandshake  bool   `json:"zero_rtt_handshake"`
	//anytls
	PaddingScheme []string `json:"padding_scheme,omitempty"`
	//hysteria hysteria2
	UpMbps                  int    `json:"up_mbps"`
	DownMbps                int    `json:"down_mbps"`
	Obfs                    string `json:"obfs"`
	ObfsPassword            string `json:"obfs_password"`
	Ignore_Client_Bandwidth bool   `json:"ignore_client_bandwidth"`
}

type Route struct {
	Id          int      `json:"id"`
	Match       []string `json:"match"`
	Action      string   `json:"action"`
	ActionValue *string  `json:"action_value"`
}

type BaseConfig struct {
	PushInterval           any `json:"push_interval"`
	PullInterval           any `json:"pull_interval"`
	DeviceOnlineMinTraffic int `json:"device_online_min_traffic"`
	NodeReportMinTraffic   int `json:"node_report_min_traffic"`
}

type TlsSettings struct {
	ServerName         string         `json:"server_name"`
	ServerNames        []string       `json:"server_names"`
	Dest               string         `json:"dest"`
	ServerPort         string         `json:"server_port"`
	ShortId            string         `json:"short_id"`
	ShortIds           []string       `json:"short_ids"`
	PrivateKey         string         `json:"private_key"`
	Mldsa65Seed        string         `json:"mldsa65Seed"`
	Xver               FlexibleUint64 `json:"xver"`
	CertMode           string         `json:"cert_mode"`
	CertFile           string         `json:"cert_file"`
	KeyFile            string         `json:"key_file"`
	Provider           string         `json:"provider"`
	DNSEnv             string         `json:"dns_env"`
	FallbackSelfSigned bool           `json:"fallback_self_signed"`
	RejectUnknownSni   string         `json:"reject_unknown_sni"`
}

type CertInfo struct {
	CertMode           string
	CertFile           string
	KeyFile            string
	Email              string
	CertDomain         string
	DNSEnv             map[string]string
	FallbackSelfSigned bool
	Provider           string
	RejectUnknownSni   bool
}

type EncSettings struct {
	Mode          string `json:"mode"`
	Ticket        string `json:"ticket"`
	ServerPadding string `json:"server_padding"`
	PrivateKey    string `json:"private_key"`
}

func (c *Client) GetNodeInfo(ctx context.Context) (node *NodeInfo, err error) {
	const path = "/api/v2/server/config"
	r, err := c.client.
		R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.nodeEtag).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("received nil response")
	}

	if r.StatusCode() == 304 {
		return nil, nil
	}
	if r.IsError() {
		return nil, fmt.Errorf("get node config: panel returned HTTP %d", r.StatusCode())
	}
	hash := sha256.Sum256(r.Body())
	newBodyHash := hex.EncodeToString(hash[:])
	if c.responseBodyHash == newBodyHash {
		return nil, nil
	}
	c.responseBodyHash = newBodyHash
	c.nodeEtag = r.Header().Get("ETag")

	if r != nil {
		defer func() {
			if r.RawBody() != nil {
				r.RawBody().Close()
			}
		}()
	} else {
		return nil, fmt.Errorf("received nil response")
	}
	node = &NodeInfo{
		Id: c.NodeId,
	}
	// parse protocol params
	cm := &CommonNode{}
	err = json.Unmarshal(r.Body(), cm)
	if err != nil {
		return nil, fmt.Errorf("decode node params error: %s", err)
	}
	if !strings.EqualFold(strings.TrimSpace(cm.PanelType), conf.RequiredPanelType) {
		return nil, fmt.Errorf("invalid node config panel_type %q: ZNode requires %s", cm.PanelType, conf.RequiredPanelType)
	}
	switch cm.Protocol {
	case "vmess", "trojan", "hysteria2", "tuic", "anytls", "vless":
		node.Type = cm.Protocol
		node.Security = cm.Tls
	case "shadowsocks":
		node.Type = cm.Protocol
		node.Security = 0
	default:
		return nil, fmt.Errorf("unsupport protocol: %s", cm.Protocol)
	}
	if node.Security != None && node.Security != Tls && node.Security != Reality {
		return nil, fmt.Errorf("unsupported transport security mode %d", node.Security)
	}
	node.Tag = fmt.Sprintf("[%s]-%s:%d", c.APIHost, node.Type, node.Id)
	cf := cm.TlsSettings.CertFile
	kf := cm.TlsSettings.KeyFile
	if cf == "" {
		cf = filepath.Join("/etc/znode/", cm.Protocol+strconv.Itoa(c.NodeId)+".cer")
	}
	if kf == "" {
		kf = filepath.Join("/etc/znode/", cm.Protocol+strconv.Itoa(c.NodeId)+".key")
	}
	// Older panel rows enabled TLS without persisting cert_mode. Treat those
	// rows as self-signed TLS so the runtime creates the certificate instead
	// of silently starting without TLS and leaving fingerprint sync waiting.
	certMode := effectiveCertMode(node.Security, cm.TlsSettings.CertMode)
	if node.Security == Tls && certMode == "none" {
		return nil, fmt.Errorf("TLS transport cannot use cert_mode=none")
	}
	cm.CertInfo = &CertInfo{
		CertMode:           certMode,
		CertFile:           cf,
		KeyFile:            kf,
		Email:              "node@v2board.com",
		CertDomain:         cm.TlsSettings.PrimaryServerName(),
		DNSEnv:             make(map[string]string),
		Provider:           cm.TlsSettings.Provider,
		RejectUnknownSni:   cm.TlsSettings.RejectUnknownSni == "1",
		FallbackSelfSigned: cm.TlsSettings.FallbackSelfSigned,
	}
	if cm.CertInfo.CertMode == "dns" || cm.CertInfo.CertMode == "auto" {
		provider := strings.ToLower(strings.TrimSpace(cm.TlsSettings.Provider))
		// ZBoard exposes only Cloudflare Auto-TLS. Refusing every other Lego
		// provider is important because some generic providers can execute an
		// operator-supplied helper process and would turn panel compromise into
		// root command execution on every Agent VPS.
		if provider != "" && provider != "cloudflare" {
			return nil, fmt.Errorf("invalid DNS certificate settings: unsupported DNS provider %q", provider)
		}
		if cm.TlsSettings.DNSEnv != "" {
			provider, credentials, err := parseDNSCredentials(provider, cm.TlsSettings.DNSEnv)
			if err != nil {
				return nil, fmt.Errorf("invalid DNS certificate settings: %w", err)
			}
			cm.CertInfo.Provider = provider
			cm.CertInfo.DNSEnv = credentials
		} else if cm.CertInfo.CertMode == "dns" {
			return nil, fmt.Errorf("invalid DNS certificate settings: Cloudflare credential is required")
		}
	}

	// A partial panel response must not dereference a nil base_config or create
	// a zero-duration task loop. intervalToTime supplies bounded defaults for
	// missing and malformed values.
	if cm.BaseConfig == nil {
		cm.BaseConfig = &BaseConfig{}
	}
	// set interval
	node.PushInterval = intervalToTime(cm.BaseConfig.PushInterval)
	node.PullInterval = intervalToTime(cm.BaseConfig.PullInterval)

	node.Common = cm

	return node, nil
}

func parseDNSCredentials(provider, raw string) (string, map[string]string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "cloudflare" {
		return "", nil, fmt.Errorf("unsupported DNS provider %q", provider)
	}
	entries := strings.Split(raw, ",")
	if len(entries) > 4 {
		return "", nil, fmt.Errorf("too many DNS credential entries")
	}
	allowed := map[string]struct{}{
		"CF_DNS_API_TOKEN":         {},
		"CLOUDFLARE_DNS_API_TOKEN": {},
	}
	credentials := make(map[string]string, len(entries))
	for _, entry := range entries {
		keyValue := strings.SplitN(entry, "=", 2)
		if len(keyValue) != 2 {
			return "", nil, fmt.Errorf("malformed DNS credential entry")
		}
		key := strings.TrimSpace(keyValue[0])
		value := strings.TrimSpace(keyValue[1])
		if _, ok := allowed[key]; !ok {
			return "", nil, fmt.Errorf("unsupported DNS credential variable %q", key)
		}
		if value == "" || len(value) > 512 || strings.IndexFunc(value, func(r rune) bool {
			return r < 0x20 || r == 0x7f
		}) >= 0 {
			return "", nil, fmt.Errorf("invalid DNS credential value")
		}
		if _, duplicate := credentials[key]; duplicate {
			return "", nil, fmt.Errorf("duplicate DNS credential variable %q", key)
		}
		credentials[key] = value
	}
	return provider, credentials, nil
}

func intervalToTime(i interface{}) time.Duration {
	var seconds int64
	switch value := i.(type) {
	case int:
		seconds = int64(value)
	case int8:
		seconds = int64(value)
	case int16:
		seconds = int64(value)
	case int32:
		seconds = int64(value)
	case int64:
		seconds = value
	case uint:
		seconds = int64(value)
	case uint8:
		seconds = int64(value)
	case uint16:
		seconds = int64(value)
	case uint32:
		seconds = int64(value)
	case uint64:
		if value > uint64(^uint64(0)>>1) {
			seconds = 3600
		} else {
			seconds = int64(value)
		}
	case float32:
		seconds = int64(value)
	case float64:
		seconds = int64(value)
	case string:
		seconds, _ = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	default:
		// A missing/malformed interval must not panic the runtime or create a
		// zero-duration busy loop when a panel sends an incomplete config.
		seconds = 60
	}
	if seconds < 5 {
		seconds = 5
	}
	if seconds > 3600 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}

func (t TlsSettings) EffectiveServerNames() []string {
	if len(t.ServerNames) > 0 {
		return t.ServerNames
	}
	if t.ServerName == "" {
		return nil
	}
	return []string{t.ServerName}
}

func (t TlsSettings) EffectiveShortIds() []string {
	if len(t.ShortIds) > 0 {
		return t.ShortIds
	}
	if t.ShortId == "" {
		return nil
	}
	return []string{t.ShortId}
}

func (t TlsSettings) PrimaryServerName() string {
	serverNames := t.EffectiveServerNames()
	if len(serverNames) == 0 {
		return ""
	}
	return serverNames[0]
}
