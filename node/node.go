package node

import (
	"context"
	"errors"
	"fmt"
	"sync"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
	"github.com/AZZ-vopp/znode/core"
	log "github.com/sirupsen/logrus"
)

type Node struct {
	controllers []*Controller
	NodeInfos   []*panel.NodeInfo
}

type controllerCloseResult struct {
	controller *Controller
	wasActive  bool
	err        error
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
	results := closeControllersConcurrently(n.controllers, func(controller *Controller) error {
		return controller.Close()
	})

	var closeErr error
	for _, result := range results {
		if result.err != nil {
			log.Errorf("close controller failed: %v", result.err)
			closeErr = errors.Join(closeErr, result.err)
		}
	}
	if closeErr == nil {
		return nil
	}

	// Closing a multi-node Agent is transactional from the caller's
	// perspective. A controller whose Close failed restores itself. Bring every
	// other controller that closed successfully back before reload reports that
	// it retained the previous runtime. Restore in reverse configuration order,
	// matching the previous sequential rollback behavior.
	var restoreErr error
	for index := len(results) - 1; index >= 0; index-- {
		result := results[index]
		if result.controller == nil || !result.wasActive || result.err != nil {
			continue
		}
		if err := result.controller.Start(result.controller.server); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	if restoreErr != nil {
		return fmt.Errorf("close node controllers: %w; restore previous controllers: %v", closeErr, restoreErr)
	}
	return fmt.Errorf("close node controllers: %w; previous controllers restored", closeErr)
}

func closeControllersConcurrently(controllers []*Controller, closeController func(*Controller) error) []controllerCloseResult {
	results := make([]controllerCloseResult, len(controllers))
	var wait sync.WaitGroup
	for index, c := range controllers {
		if c == nil {
			continue
		}

		// Each controller owns a distinct inbound/tag. Closing them concurrently
		// makes the traffic-drain deadline apply once per Agent instead of once
		// per node (N nodes previously took up to N * 5 seconds).
		c.userSyncMu.Lock()
		wasActive := c.started
		c.userSyncMu.Unlock()
		results[index] = controllerCloseResult{controller: c, wasActive: wasActive}
		wait.Add(1)
		go func(index int, controller *Controller) {
			defer wait.Done()
			results[index].err = closeController(controller)
		}(index, c)
	}
	wait.Wait()
	return results
}
