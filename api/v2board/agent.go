package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wyx2685/znode/conf"
)

const agentManifestPath = "/api/v2/server/agent/config"

// AgentManifest is the desired logical-node assignment returned by V2Board.
// Revision should change whenever Nodes changes. If an older panel omits it,
// EffectiveRevision derives a stable value from the sorted node IDs.
type AgentManifest struct {
	Revision             string `json:"revision"`
	Nodes                []int  `json:"nodes"`
	PollInterval         int    `json:"poll_interval"`
	AuthorizationRevoked bool   `json:"-"`
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

	timeout := conf.DefaultNodeTimeout
	client := resty.New().
		SetBaseURL(strings.TrimRight(c.APIHost, "/")).
		SetTimeout(time.Duration(timeout)*time.Second).
		SetRetryCount(conf.DefaultNodeRetryCount).
		SetHeader("User-Agent", fmt.Sprintf("znode-agent go-resty/%s (https://github.com/go-resty/resty)", resty.Version)).
		SetHeader("X-ZNode-Agent-ID", c.AgentID).
		SetHeader("X-ZNode-Instance-ID", effectiveInstanceID(c.AgentInstanceID)).
		SetHeader("X-ZNode-Agent-Token", c.AgentToken).
		SetAuthToken(c.AgentToken)
	setAddressHeaders(client)

	return &AgentClient{client: client, config: c}, nil
}

func (c *AgentClient) GetManifest(ctx context.Context) (*AgentManifest, error) {
	response, err := c.client.R().
		SetContext(ctx).
		SetQueryParam("agent_id", c.config.AgentID).
		ForceContentType("application/json").
		Get(agentManifestPath)
	if err != nil {
		return nil, fmt.Errorf("get agent manifest: %w", err)
	}
	if response == nil {
		return nil, fmt.Errorf("get agent manifest: received nil response")
	}
	if response.StatusCode() == http.StatusUnauthorized || response.StatusCode() == http.StatusForbidden {
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

func (m *AgentManifest) Validate() error {
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
	return time.Duration(seconds) * time.Second
}

// NodeConfigs turns the authoritative manifest into the per-node clients used
// by the existing runtime. Manual Nodes are untouched when agent mode is off.
func (m *AgentManifest) NodeConfigs(agent conf.AgentConfig) []conf.NodeConfig {
	nodes := make([]conf.NodeConfig, 0, len(m.Nodes))
	for _, nodeID := range m.Nodes {
		nodes = append(nodes, conf.NodeConfig{
			APIHost:                 agent.APIHost,
			NodeID:                  nodeID,
			Key:                     agent.AgentToken,
			AgentID:                 agent.AgentID,
			AgentInstanceID:         agent.AgentInstanceID,
			Timeout:                 conf.DefaultNodeTimeout,
			GlobalDeviceLimitConfig: cloneGlobalDeviceLimitConfig(agent.GlobalDeviceLimitConfig),
		})
	}
	return nodes
}

func cloneGlobalDeviceLimitConfig(source *conf.GlobalDeviceLimitConfig) *conf.GlobalDeviceLimitConfig {
	if source == nil {
		return nil
	}
	cloned := *source
	if source.SyncEnabled != nil {
		syncEnabled := *source.SyncEnabled
		cloned.SyncEnabled = &syncEnabled
	}
	return &cloned
}
