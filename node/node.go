package node

import (
	"context"
	"fmt"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
	"github.com/AZZ-vopp/znode/core"
	log "github.com/sirupsen/logrus"
)

type Node struct {
	controllers []*Controller
	NodeInfos   []*panel.NodeInfo
}

func New(nodes []conf.NodeConfig) (*Node, error) {
	return NewContext(context.Background(), nodes)
}

func NewContext(ctx context.Context, nodes []conf.NodeConfig) (*Node, error) {
	n := &Node{
		controllers: make([]*Controller, len(nodes)),
		NodeInfos:   make([]*panel.NodeInfo, len(nodes)),
	}
	for i, node := range nodes {
		p, err := panel.New(&node)
		if err != nil {
			return nil, err
		}
		info, err := p.GetNodeInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("get node info for node %d: %w", node.NodeID, err)
		}
		if info == nil || info.Common == nil {
			return nil, fmt.Errorf("get node info for node %d: panel returned an empty node configuration", node.NodeID)
		}
		n.controllers[i] = NewController(p, &node, info)
		n.NodeInfos[i] = info
	}
	if err := ValidateUniqueServerPorts(n.NodeInfos); err != nil {
		return nil, err
	}
	return n, nil
}

// ValidateUniqueServerPorts performs an agent-side preflight before any
// inbound is opened. V2Board also validates assignments, but this second check
// keeps a stale or malformed manifest from tearing down a working core.
func ValidateUniqueServerPorts(infos []*panel.NodeInfo) error {
	type owner struct {
		nodeID int
		tag    string
	}
	ports := make(map[int]owner, len(infos))
	for _, info := range infos {
		if info == nil || info.Common == nil {
			return fmt.Errorf("validate node ports: received empty node configuration")
		}
		port := info.Common.ServerPort
		if port <= 0 || port > 65535 {
			return fmt.Errorf("node %d has invalid server_port %d", info.Id, port)
		}
		if previous, exists := ports[port]; exists {
			return fmt.Errorf(
				"duplicate server_port %d on this VPS agent: node %d (%s) conflicts with existing node %d (%s)",
				port,
				info.Id,
				info.Tag,
				previous.nodeID,
				previous.tag,
			)
		}
		ports[port] = owner{nodeID: info.Id, tag: info.Tag}
	}
	return nil
}

func (n *Node) Prepare(ctx context.Context, nodes []conf.NodeConfig) error {
	for i, nodeConfig := range nodes {
		if err := n.controllers[i].Prepare(ctx); err != nil {
			return fmt.Errorf("prepare node controller [%s-%d] error: %w", nodeConfig.APIHost, nodeConfig.NodeID, err)
		}
	}
	return nil
}

func (n *Node) Start(nodes []conf.NodeConfig, core *core.V2Core) error {
	for i, node := range nodes {
		err := n.controllers[i].Start(core)
		if err != nil {
			// Close the failed controller too: AddNode may already have bound its
			// port before a later initialization step failed.
			for j := i; j >= 0; j-- {
				if closeErr := n.controllers[j].Close(); closeErr != nil {
					log.Errorf("rollback controller failed: %v", closeErr)
				}
			}
			return fmt.Errorf("start node controller [%s-%d] error: %s",
				node.APIHost,
				node.NodeID,
				err)
		}
	}
	return nil
}

func (n *Node) Close() error {
	closed := make([]*Controller, 0, len(n.controllers))
	for _, c := range n.controllers {
		if c == nil {
			continue
		}
		wasActive := c.started
		if err := c.Close(); err != nil {
			log.Errorf("close controller failed: %v", err)
			// Closing a multi-node Agent is transactional from the caller's
			// perspective. Controllers closed earlier in this pass must be brought
			// back before reload reports that it retained the previous runtime.
			var restoreErr error
			for index := len(closed) - 1; index >= 0; index-- {
				controller := closed[index]
				if startErr := controller.Start(controller.server); startErr != nil && restoreErr == nil {
					restoreErr = startErr
				}
			}
			if restoreErr != nil {
				return fmt.Errorf("close node controllers: %w; restore previous controllers: %v", err, restoreErr)
			}
			return fmt.Errorf("close node controllers: %w; previous controllers restored", err)
		}
		if wasActive {
			closed = append(closed, c)
		}
	}
	return nil
}
