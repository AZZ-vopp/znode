package node

import (
	"context"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/znode/api/v2board"
	"github.com/wyx2685/znode/common/task"
	"github.com/wyx2685/znode/conf"
	"github.com/wyx2685/znode/core"
	"github.com/wyx2685/znode/limiter"
)

type Controller struct {
	server                  *core.V2Core
	apiClient               *panel.Client
	tag                     string
	limiter                 *limiter.Limiter
	userList                []panel.UserInfo
	aliveMap                map[int]int
	conf                    *conf.NodeConfig
	info                    *panel.NodeInfo
	nodeInfoMonitorPeriodic *task.Task
	userReportPeriodic      *task.Task
	renewCertPeriodic       *task.Task
	deviceSyncWatcher       *deviceSyncWatcher
	metrics                 *nodeMetricsCollector
	userSyncMu              sync.Mutex
	prepared                bool
	started                 bool
}

// NewController return a Node controller with default parameters.
func NewController(api *panel.Client, conf *conf.NodeConfig, info *panel.NodeInfo) *Controller {
	controller := &Controller{
		apiClient: api,
		info:      info,
		conf:      conf,
	}
	return controller
}

// Prepare fetches all panel-side state and certificates without binding an
// inbound. Reload uses this while the previous runtime is still healthy.
func (c *Controller) Prepare(ctx context.Context) error {
	var err error
	if c.info == nil {
		c.info, err = c.apiClient.GetNodeInfo(ctx)
		if err != nil {
			return fmt.Errorf("get node info error: %s", err)
		}
		if c.info == nil || c.info.Common == nil {
			return fmt.Errorf("get node info error: panel returned an empty node configuration")
		}
	}
	c.tag = c.info.Tag
	c.userList, err = c.apiClient.GetUserList(ctx)
	if err != nil {
		return fmt.Errorf("get user list error: %s", err)
	}
	if c.userList == nil {
		return fmt.Errorf("get user list error: panel returned not-modified before an initial user list")
	}
	c.aliveMap, err = c.apiClient.GetUserAlive(ctx)
	if err != nil {
		return fmt.Errorf("failed to get user alive list: %s", err)
	}
	if c.info.Security == panel.Tls {
		if err := c.requestCert(); err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}
	c.prepared = true
	return nil
}

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) error {
	// Init Core
	c.server = x
	if !c.prepared {
		if err := c.Prepare(context.Background()); err != nil {
			return err
		}
	}
	node := c.info

	// add limiter
	l := limiter.AddLimiter(c.info.Type, c.tag, c.userList, c.aliveMap, c.conf.GlobalDeviceLimitConfig, c.conf.APIHost)
	c.limiter = l
	c.metrics = newNodeMetricsCollector()
	// Add new tag
	err := c.server.AddNode(c.tag, node)
	if err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	c.started = true
	added := 0
	if len(c.userList) > 0 {
		added, err = c.server.AddUsers(&core.AddUsersParams{
			Tag:      c.tag,
			Users:    c.userList,
			NodeInfo: node,
		})
		if err != nil {
			return fmt.Errorf("add users error: %s", err)
		}
	}
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	c.info = node
	c.startTasks(node)
	// Publish the freshly generated/self-signed certificate fingerprint right
	// after a successful start instead of waiting for the traffic interval.
	go c.reportNodeStatusImmediately()
	c.deviceSyncWatcher = newDeviceSyncWatcher(c.conf.GlobalDeviceLimitConfig, c.conf.APIHost)
	if c.deviceSyncWatcher != nil {
		c.deviceSyncWatcher.Start(c.conf.APIHost, c.refreshUsersImmediately)
		log.WithField("tag", c.tag).Info("Start device UUID fast-sync watcher")
	}
	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	if c.deviceSyncWatcher != nil {
		c.deviceSyncWatcher.Close()
		c.deviceSyncWatcher = nil
	}
	if c.nodeInfoMonitorPeriodic != nil {
		c.nodeInfoMonitorPeriodic.Close()
	}
	if c.userReportPeriodic != nil {
		c.userReportPeriodic.Close()
	}
	if c.renewCertPeriodic != nil {
		c.renewCertPeriodic.Close()
	}
	if c.limiter != nil {
		limiter.DeleteLimiter(c.tag)
		c.limiter = nil
	}
	if !c.started || c.server == nil {
		return nil
	}
	err := c.server.DelNode(c.tag)
	if err != nil {
		return fmt.Errorf("del node error: %s", err)
	}
	c.started = false
	return nil
}
