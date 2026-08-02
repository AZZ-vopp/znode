package panel

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/AZZ-vopp/znode/conf"
	"github.com/go-resty/resty/v2"
)

const agentManifestPath = "/api/v2/server/agent/config"
const agentMaintenanceReportPath = "/api/v2/server/agent/maintenance/report"
const agentCertificateReportPath = "/api/v2/server/agent/certificate/report"
const agentAuthorizationHeader = "X-ZBoard-Agent-Authorization"

const maxAgentNodes = 10000

type AgentMaintenance struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	RequestedAt int64  `json:"requested_at"`
}

type AgentCertificateRequest struct {
	ID          string `json:"id"`
	NodeID      int    `json:"node_id"`
	CertFile    string `json:"cert_file"`
	RequestedAt int64  `json:"requested_at"`
}

// AgentManifest is the desired logical-node assignment returned by V2Board.
// Revision should change whenever Nodes changes. If an older panel omits it,
// EffectiveRevision derives a stable value from the sorted node IDs.
type AgentManifest struct {
	PanelType               string                        `json:"panel_type"`
	Revision                string                        `json:"revision"`
	Nodes                   []int                         `json:"nodes"`
	PollInterval            int                           `json:"poll_interval"`
	Maintenance             *AgentMaintenance             `json:"maintenance,omitempty"`
	CertificateRequest      *AgentCertificateRequest      `json:"certificate_request,omitempty"`
	GlobalDeviceLimitConfig *conf.GlobalDeviceLimitConfig `json:"global_device_limit_config,omitempty"`
	AuthorizationRevoked    bool                          `json:"-"`
}

// AgentClient is deliberately separate from Client: it authenticates a VPS
// agent, while Client authenticates requests for one assigned logical node.
type AgentClient struct {
	client *resty.Client
	config conf.AgentConfig
}

func NewAgentClient(c conf.AgentConfig) (*AgentClient, error) {
	if !c.Enable {
		return nil, fmt.Errorf("agent client requires Agent.Enable=true")
	}
	if strings.TrimSpace(c.APIHost) == "" || strings.TrimSpace(c.AgentID) == "" || strings.TrimSpace(c.AgentToken) == "" {
		return nil, fmt.Errorf("agent client requires ApiHost, AgentID and AgentToken")
	}
	apiHost, err := conf.NormalizePanelAPIHost(c.APIHost)
	if err != nil {
		return nil, fmt.Errorf("agent client: %w", err)
	}
	c.APIHost = apiHost

	timeout := conf.DefaultNodeTimeout
	client := resty.New().
		SetBaseURL(c.APIHost).
		SetTimeout(time.Duration(timeout)*time.Second).
		SetResponseBodyLimit(maxBufferedPanelResponseBytes).
		SetRetryCount(conf.DefaultNodeRetryCount).
		SetRedirectPolicy(resty.NoRedirectPolicy()).
		SetTLSClientConfig(&tls.Config{MinVersion: tls.VersionTLS12}).
		SetHeader("User-Agent", fmt.Sprintf("znode-agent go-resty/%s (https://github.com/go-resty/resty)", resty.Version)).
		SetHeader("X-ZNode-Agent-ID", c.AgentID).
		SetHeader("X-ZNode-Instance-ID", effectiveInstanceID(c.AgentInstanceID)).
		SetHeader("X-ZNode-Agent-Token", c.AgentToken).
		SetHeader("X-ZNode-Type", conf.RequiredPanelType).
		SetHeader("X-ZNode-Version", ClientVersion()).
		SetAuthToken(c.AgentToken)
	setAddressHeaders(client)

	return &AgentClient{client: client, config: c}, nil
}

func (c *AgentClient) GetManifest(ctx context.Context) (*AgentManifest, error) {
	response, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(agentManifestPath)
	if err != nil {
		return nil, fmt.Errorf("get agent manifest: %w", err)
	}
	if response == nil {
		return nil, fmt.Errorf("get agent manifest: received nil response")
	}
	if response.StatusCode() == http.StatusUnauthorized || response.StatusCode() == http.StatusForbidden {
		// A CDN, WAF or maintenance proxy may generate a generic 401/403 while
		// the panel is unavailable. Only an authenticated ZBoard response carries
		// this explicit marker; generic denials are transient control-plane errors
		// and must never tear down healthy VPN inbounds.
		if !strings.EqualFold(strings.TrimSpace(response.Header().Get(agentAuthorizationHeader)), "revoked") {
			return nil, fmt.Errorf("get agent manifest: unconfirmed authorization response HTTP %d", response.StatusCode())
		}
		return &AgentManifest{
			Revision:             fmt.Sprintf("authorization-revoked:%d", response.StatusCode()),
			Nodes:                make([]int, 0),
			PollInterval:         c.config.PollInterval,
			AuthorizationRevoked: true,
		}, nil
	}
	if response.IsError() {
		return nil, fmt.Errorf("get agent manifest: panel returned HTTP %d", response.StatusCode())
	}

	body := response.Body()
	var envelope struct {
		Status  string          `json:"status"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode agent manifest: %w", err)
	}
	if envelope.Status != "" && !strings.EqualFold(envelope.Status, "success") {
		return nil, fmt.Errorf("get agent manifest: panel rejected credentials: %s", envelope.Message)
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		body = envelope.Data
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("decode agent manifest: %w", err)
	}
	if _, ok := fields["nodes"]; !ok {
		return nil, fmt.Errorf("decode agent manifest: missing nodes field")
	}
	manifest := &AgentManifest{}
	if err := json.Unmarshal(body, manifest); err != nil {
		return nil, fmt.Errorf("decode agent manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return manifest, nil
}

func (c *AgentClient) ReportMaintenance(ctx context.Context, commandID, status, message string) error {
	response, err := c.client.R().SetContext(ctx).SetBody(map[string]string{
		"id": commandID, "status": status, "message": message, "version": ClientVersion(),
	}).ForceContentType("application/json").Post(agentMaintenanceReportPath)
	if err != nil {
		return fmt.Errorf("report agent maintenance: %w", err)
	}
	if response.IsError() {
		return fmt.Errorf("report agent maintenance: panel returned HTTP %d", response.StatusCode())
	}
	return nil
}

func (c *AgentClient) ReportCertificate(ctx context.Context, requestID string, report CertificateReport) error {
	body := map[string]any{"id": requestID, "node_id": report.NodeID, "status": report.Status, "message": report.Message}
	if report.SHA256 != "" {
		body["sha256"] = report.SHA256
	}
	if report.PublicKeySHA256 != "" {
		body["public_key_sha256"] = report.PublicKeySHA256
	}
	if report.NotAfter > 0 {
		body["not_after"] = report.NotAfter
	}
	if report.Issuer != "" {
		body["issuer"] = report.Issuer
	}
	response, err := c.client.R().SetContext(ctx).SetBody(body).ForceContentType("application/json").Post(agentCertificateReportPath)
	if err != nil {
		return fmt.Errorf("report agent certificate: %w", err)
	}
	if response.IsError() {
		return fmt.Errorf("report agent certificate: panel returned HTTP %d", response.StatusCode())
	}
	return nil
}

type CertificateReport struct {
	NodeID          int
	Status          string
	SHA256          string
	PublicKeySHA256 string
	NotAfter        int64
	Issuer          string
	Message         string
}

func (m *AgentManifest) Validate() error {
	if !strings.EqualFold(strings.TrimSpace(m.PanelType), conf.RequiredPanelType) {
		return fmt.Errorf("invalid agent manifest panel_type %q: ZNode requires %s", m.PanelType, conf.RequiredPanelType)
	}
	if len(m.Nodes) > maxAgentNodes {
		return fmt.Errorf("invalid agent manifest: too many assigned nodes")
	}
	if len(m.Revision) > 256 {
		return fmt.Errorf("invalid agent manifest: revision is too long")
	}
	if m.GlobalDeviceLimitConfig != nil {
		if len(m.GlobalDeviceLimitConfig.RedisSentinelAddrs) > 64 {
			return fmt.Errorf("invalid agent manifest: too many Redis sentinels")
		}
		if _, err := conf.RedisTLSConfig(m.GlobalDeviceLimitConfig); err != nil {
			return fmt.Errorf("invalid agent manifest Redis config: %w", err)
		}
	}
	seen := make(map[int]struct{}, len(m.Nodes))
	for _, nodeID := range m.Nodes {
		if nodeID <= 0 {
			return fmt.Errorf("invalid agent manifest: node ID must be positive, got %d", nodeID)
		}
		if _, ok := seen[nodeID]; ok {
			return fmt.Errorf("invalid agent manifest: duplicate node ID %d", nodeID)
		}
		seen[nodeID] = struct{}{}
	}
	if m.Maintenance != nil {
		if len(m.Maintenance.ID) != 32 {
			return fmt.Errorf("invalid agent maintenance command ID")
		}
		if _, err := hex.DecodeString(m.Maintenance.ID); err != nil {
			return fmt.Errorf("invalid agent maintenance command ID: %w", err)
		}
		if m.Maintenance.Action != "update_latest" && m.Maintenance.Action != "rollback" {
			return fmt.Errorf("invalid agent maintenance action %q", m.Maintenance.Action)
		}
	}
	if m.CertificateRequest != nil {
		if len(m.CertificateRequest.ID) != 32 {
			return fmt.Errorf("invalid agent certificate request ID")
		}
		if _, err := hex.DecodeString(m.CertificateRequest.ID); err != nil {
			return fmt.Errorf("invalid agent certificate request ID: %w", err)
		}
		if m.CertificateRequest.NodeID <= 0 {
			return fmt.Errorf("invalid agent certificate request node ID")
		}
		if len(m.CertificateRequest.CertFile) == 0 || len(m.CertificateRequest.CertFile) > 4096 {
			return fmt.Errorf("invalid agent certificate request path")
		}
	}
	return nil
}

func (m *AgentManifest) EffectiveRevision() string {
	if revision := strings.TrimSpace(m.Revision); revision != "" {
		return revision
	}
	ids := append([]int(nil), m.Nodes...)
	sort.Ints(ids)
	payload, _ := json.Marshal(ids)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (m *AgentManifest) EffectivePollInterval(fallback int) time.Duration {
	seconds := m.PollInterval
	if seconds <= 0 {
		seconds = fallback
	}
	if seconds <= 0 {
		seconds = 15
	}
	// A broken or malicious panel must not create a tight polling loop.
	if seconds < 5 {
		seconds = 5
	}
	if seconds > 3600 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}

// NodeConfigs turns the authoritative manifest into the per-node clients used
// by the existing runtime. Manual Nodes are untouched when agent mode is off.
func (m *AgentManifest) NodeConfigs(agent conf.AgentConfig) []conf.NodeConfig {
	deviceConfig := agent.GlobalDeviceLimitConfig
	if m.GlobalDeviceLimitConfig != nil {
		deviceConfig = m.GlobalDeviceLimitConfig
	}
	nodes := make([]conf.NodeConfig, 0, len(m.Nodes))
	for _, nodeID := range m.Nodes {
		nodes = append(nodes, conf.NodeConfig{
			APIHost:                 agent.APIHost,
			NodeID:                  nodeID,
			Key:                     agent.AgentToken,
			AgentID:                 agent.AgentID,
			AgentInstanceID:         agent.AgentInstanceID,
			Timeout:                 conf.DefaultNodeTimeout,
			GlobalDeviceLimitConfig: cloneGlobalDeviceLimitConfig(deviceConfig),
		})
	}
	return nodes
}

func cloneGlobalDeviceLimitConfig(source *conf.GlobalDeviceLimitConfig) *conf.GlobalDeviceLimitConfig {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.RedisSentinelAddrs = append([]string(nil), source.RedisSentinelAddrs...)
	if source.SyncEnabled != nil {
		syncEnabled := *source.SyncEnabled
		cloned.SyncEnabled = &syncEnabled
	}
	return &cloned
}
