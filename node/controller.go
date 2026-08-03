package node

import (
	"context"
	"fmt"
	"sync"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/common/task"
	"github.com/AZZ-vopp/znode/conf"
	"github.com/AZZ-vopp/znode/core"
	"github.com/AZZ-vopp/znode/limiter"
	log "github.com/sirupsen/logrus"
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
	userRevisionWatcher     *userRevisionWatcher
	userRevision            string
	metrics                 *nodeMetricsCollector
	userSyncMu              sync.Mutex
	trafficReportMu         sync.Mutex
	pendingTraffic          []panel.UserTraffic
	pendingTrafficReportID  string
	queuedTraffic           []panel.UserTraffic
	quiescedUsers           []panel.UserInfo
	trafficSpoolLoaded      bool
	closing                 bool
	inboundActive           bool
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

func (c *Controller) UpdateFallbackConfig(config *conf.GlobalDeviceLimitConfig) {
	c.userSyncMu.Lock()
	cloned := cloneDeviceConfig(config)
	c.conf.GlobalDeviceLimitConfig = cloned
	c.apiClient.UpdateFallbackConfig(cloned)
	c.userSyncMu.Unlock()
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
	// Read the revision before the user snapshot. If a device changes between
	// these calls, the watcher observes the newer revision and immediately
	// performs one more credential-only reconciliation after startup.
	if revision, revisionErr := c.apiClient.GetUserRevision(ctx); revisionErr == nil {
		c.userRevision = revision
	} else {
		log.WithFields(log.Fields{"tag": c.tag, "err": revisionErr}).Debug("User revision endpoint unavailable; periodic pull remains active")
	}
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
		if err := c.requestCertContext(ctx); err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}
	c.prepared = true
	return nil
}

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) error {
	c.userSyncMu.Lock()
	if c.started || c.inboundActive {
		c.userSyncMu.Unlock()
		return fmt.Errorf("node controller %s is already active", c.tag)
	}
	c.closing = false
	c.userSyncMu.Unlock()
	// Init Core
	c.server = x
	if !c.prepared {
		if err := c.Prepare(context.Background()); err != nil {
			return err
		}
	}
	if err := c.restoreTrafficSpool(); err != nil {
		return fmt.Errorf("restore durable traffic batch: %s", err)
	}
	node := c.info
	// A controller reused by rollback can retain a deletion transaction whose
	// counters became durable during Close. Do not briefly re-authorize those
	// credentials in the replacement core; syncUsers will finalize the retained
	// transaction and re-add only UUIDs that are present in the latest panel
	// list.
	runtimeUsers := removeUsersByCredential(c.userList, c.quiescedUsers)

	// add limiter
	l := limiter.AddLimiter(c.info.Type, c.tag, runtimeUsers, c.aliveMap, c.conf.GlobalDeviceLimitConfig, c.conf.APIHost)
	c.limiter = l
	c.metrics = newNodeMetricsCollector()
	// Add new tag
	err := c.server.AddNode(c.tag, node)
	if err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	c.inboundActive = true
	c.started = true
	added := 0
	if len(runtimeUsers) > 0 {
		added, err = c.server.AddUsers(&core.AddUsersParams{
			Tag:      c.tag,
			Users:    runtimeUsers,
			NodeInfo: node,
		})
		if err != nil {
			return fmt.Errorf("add users error: %s", err)
		}
	}
	// A controller can be restarted on the same core when a multi-node close or
	// reload is rolled back. Remove the tag-level rejection barrier only after
	// the listener and all runtime users are ready again.
	c.server.ReactivateNodeLinks(c.tag)
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	c.info = node
	c.startBackgroundServices()
	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	c.stopBackgroundServices()

	c.userSyncMu.Lock()
	c.closing = true
	defer c.userSyncMu.Unlock()
	if !c.started || c.server == nil {
		if c.limiter != nil {
			limiter.DeleteLimiter(c.tag)
			c.limiter = nil
		}
		return nil
	}

	// Stop accepting new connections before the last atomic counter capture.
	// If any later durability barrier fails, restore this same controller on the
	// old core before returning so a failed reload does not silently leave the
	// supposedly retained runtime offline.
	if c.inboundActive {
		if err := c.server.DelNode(c.tag); err != nil {
			c.closing = false
			c.startBackgroundServices()
			return fmt.Errorf("del node error: %s", err)
		}
		c.inboundActive = false
	}
	if err := c.server.QuiesceNodeLinks(c.tag); err != nil {
		return c.restoreAfterCloseFailureLocked(fmt.Errorf("drain node links: %w", err))
	}
	if err := c.spoolOutstandingTrafficWithUsersLocked(); err != nil {
		return c.restoreAfterCloseFailureLocked(fmt.Errorf("persist traffic: %w", err))
	}

	if c.limiter != nil {
		limiter.DeleteLimiter(c.tag)
		c.limiter = nil
	}
	c.started = false
	return nil
}

func (c *Controller) stopBackgroundServices() {
	if c.userRevisionWatcher != nil {
		c.userRevisionWatcher.Close()
		c.userRevisionWatcher = nil
	}
	if c.deviceSyncWatcher != nil {
		c.deviceSyncWatcher.Close()
		c.deviceSyncWatcher = nil
	}
	if c.nodeInfoMonitorPeriodic != nil {
		c.nodeInfoMonitorPeriodic.Close()
		c.nodeInfoMonitorPeriodic = nil
	}
	if c.userReportPeriodic != nil {
		c.userReportPeriodic.Close()
		c.userReportPeriodic = nil
	}
	if c.renewCertPeriodic != nil {
		c.renewCertPeriodic.Close()
		c.renewCertPeriodic = nil
	}
}

func (c *Controller) startBackgroundServices() {
	if c.info == nil || c.server == nil || !c.started {
		return
	}
	c.startTasks(c.info)
	c.userRevisionWatcher = newUserRevisionWatcher(c.apiClient, c.userRevision, c.syncUserCredentials)
	c.userRevisionWatcher.Start()
	// Publish the freshly generated/self-signed certificate fingerprint right
	// after a successful start instead of waiting for the traffic interval.
	c.reportNodeStatusImmediately()
	c.deviceSyncWatcher = newDeviceSyncWatcher(c.conf.GlobalDeviceLimitConfig, c.conf.APIHost)
	if c.deviceSyncWatcher != nil {
		if err := c.deviceSyncWatcher.Start(c.conf.APIHost, c.refreshUsersImmediately); err != nil {
			c.deviceSyncWatcher = nil
			log.WithError(err).WithField("tag", c.tag).Warn("Device UUID fast-sync disabled")
		} else {
			log.WithField("tag", c.tag).Info("Start device UUID fast-sync watcher")
		}
	}
}

// restoreAfterCloseFailureLocked requires userSyncMu and is the rollback half
// of Close. Live counters were not committed when the durable spool failed, so
// reinstalling the inbound on the same core preserves every byte for retry.
func (c *Controller) restoreAfterCloseFailureLocked(cause error) error {
	restoreErr := c.restoreInboundLocked()
	if restoreErr == nil {
		c.closing = false
		c.startBackgroundServices()
		return cause
	}
	return fmt.Errorf("%w; restore previous node runtime: %v", cause, restoreErr)
}

func (c *Controller) restoreInboundLocked() error {
	if c.inboundActive {
		c.server.ReactivateNodeLinks(c.tag)
		return nil
	}
	if err := c.server.AddNode(c.tag, c.info); err != nil {
		return fmt.Errorf("re-add inbound: %w", err)
	}
	c.inboundActive = true
	runtimeUsers := removeUsersByCredential(c.userList, c.quiescedUsers)
	if len(runtimeUsers) > 0 {
		if _, err := c.server.AddUsers(&core.AddUsersParams{
			Tag: c.tag, Users: runtimeUsers, NodeInfo: c.info,
		}); err != nil {
			_ = c.server.DelNode(c.tag)
			c.inboundActive = false
			return fmt.Errorf("restore users: %w", err)
		}
	}
	c.server.ReactivateNodeLinks(c.tag)
	return nil
}
