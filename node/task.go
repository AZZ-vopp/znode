package node

import (
	"context"
	"errors"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/common/task"
	vCore "github.com/AZZ-vopp/znode/core"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) startTasks(node *panel.NodeInfo) {
	// fetch node info task
	c.nodeInfoMonitorPeriodic = &task.Task{
		Name:     "nodeInfoMonitor",
		Interval: node.PullInterval,
		Execute:  c.nodeInfoMonitor,
		ReloadCh: c.server.ReloadCh,
	}
	// fetch user list task
	c.userReportPeriodic = &task.Task{
		Name:     "reportUserTrafficTask",
		Interval: node.PushInterval,
		Execute:  c.reportUserTrafficTask,
		ReloadCh: c.server.ReloadCh,
	}
	log.WithField("tag", c.tag).Info("Start monitor node status")
	// delay to start nodeInfoMonitor
	_ = c.nodeInfoMonitorPeriodic.Start(false)
	log.WithField("tag", c.tag).Info("Start report node status")
	_ = c.userReportPeriodic.Start(false)
	if node.Security == panel.Tls {
		switch c.info.Common.CertInfo.CertMode {
		case "none", "", "file", "self":
		default:
			c.renewCertPeriodic = &task.Task{
				Name:     "renewCertTask",
				Interval: time.Hour * 24,
				Execute:  c.renewCertTask,
				ReloadCh: c.server.ReloadCh,
			}
			log.WithField("tag", c.tag).Info("Start renew cert")
			// delay to start renewCert
			_ = c.renewCertPeriodic.Start(true)
		}
	}
}

func (c *Controller) nodeInfoMonitor(ctx context.Context) (err error) {
	// get node info
	newN, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get node info failed")
		return nil
	}
	if newN != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
		}).Error("Got new node info, reload")
		if c.server.ReloadCh != nil {
			select {
			case c.server.ReloadCh <- struct{}{}:
			default:
			}
		} else {
			log.Panic("Reload failed")
		}
	}
	log.WithField("tag", c.tag).Debug("Node info no change")

	return c.syncUsers(ctx)
}

func (c *Controller) refreshUsersImmediately() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.syncUsers(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.WithFields(log.Fields{"tag": c.tag, "err": err}).Warn("Fast device sync failed")
	}
}

func (c *Controller) syncUsers(ctx context.Context) (err error) {
	c.userSyncMu.Lock()
	defer c.userSyncMu.Unlock()
	if c.closing {
		return nil
	}
	// get user info
	newU, err := c.apiClient.GetUserList(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get user list failed")
		return nil
	}
	// get user alive
	newA, err := c.apiClient.GetUserAlive(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get alive list failed")
		return nil
	}

	// update alive list
	if newA != nil {
		c.limiter.UpdateAliveList(newA)
	}
	// A prior deletion may already have removed runtime credentials but failed
	// to fsync its final counters. Finish that transaction before comparing the
	// latest desired list. In particular, if the panel re-adds the UUID while
	// disk persistence is recovering, the comparison below must see it as an
	// addition and restore the runtime credential.
	if len(c.quiescedUsers) > 0 {
		quiesced, quiesceErr := c.server.QuiesceUsers(c.quiescedUsers, c.tag)
		c.quiescedUsers = mergeUsersByCredential(c.quiescedUsers, quiesced)
		if quiesceErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": quiesceErr,
			}).Error("Complete prior user quiesce failed")
			return nil
		}
		if finalizeErr := c.finalizeQuiescedUsersLocked(); finalizeErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": finalizeErr,
			}).Error("Persist final traffic for quiesced users failed")
			return nil
		}
	}
	// node no changed, check users
	if newU == nil {
		log.WithField("tag", c.tag).Debug("User list no change")
		return nil
	}
	deleted, added, modified := compareUserList(c.userList, newU)
	if len(deleted) > 0 {
		// Stop new sessions and close old links, but retain UID mappings and
		// counters until the final capture is fsynced. A disk failure keeps the
		// users quiesced (fail closed) and retries the durable capture later.
		quiesced, quiesceErr := c.server.QuiesceUsers(deleted, c.tag)
		c.quiescedUsers = mergeUsersByCredential(c.quiescedUsers, quiesced)
		if quiesceErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": quiesceErr,
			}).Error("Quiesce users failed")
			return nil
		}
		c.quiescedUsers = mergeUsersByCredential(c.quiescedUsers, deleted)
		if finalizeErr := c.finalizeQuiescedUsersLocked(); finalizeErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": finalizeErr,
			}).Error("Persist final traffic for deleted users failed")
			return nil
		}
	}
	if len(added) > 0 {
		// have added users
		_, err = c.server.AddUsers(&vCore.AddUsersParams{
			Tag:      c.tag,
			NodeInfo: c.info,
			Users:    added,
		})
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Add users failed")
			return nil
		}
	}
	if len(added) > 0 || len(modified) > 0 {
		// update Limiter
		c.limiter.UpdateUser(c.tag, added, nil, modified)
	}
	c.userList = newU
	log.WithField("tag", c.tag).Infof("%d user deleted, %d user added, %d user modified", len(deleted), len(added), len(modified))
	return nil
}

// finalizeQuiescedUsersLocked requires userSyncMu. Once the final counters are
// durable, it makes the controller's active-user snapshot match the runtime
// before a later desired-state comparison can re-add any restored UUID.
func (c *Controller) finalizeQuiescedUsersLocked() error {
	if len(c.quiescedUsers) == 0 {
		return nil
	}
	if err := c.spoolOutstandingTrafficWithUsersLocked(); err != nil {
		return err
	}
	completed := append([]panel.UserInfo(nil), c.quiescedUsers...)
	c.server.ForgetUsers(completed, c.tag)
	if c.limiter != nil {
		c.limiter.UpdateUser(c.tag, nil, completed, nil)
	}
	c.userList = removeUsersByCredential(c.userList, completed)
	c.quiescedUsers = nil
	return nil
}

func removeUsersByCredential(users, removed []panel.UserInfo) []panel.UserInfo {
	if len(users) == 0 || len(removed) == 0 {
		return users
	}
	credentials := make(map[string]struct{}, len(removed))
	for _, user := range removed {
		credentials[user.Uuid] = struct{}{}
	}
	kept := make([]panel.UserInfo, 0, len(users))
	for _, user := range users {
		if _, remove := credentials[user.Uuid]; !remove {
			kept = append(kept, user)
		}
	}
	return kept
}

func mergeUsersByCredential(existing, additions []panel.UserInfo) []panel.UserInfo {
	if len(additions) == 0 {
		return existing
	}
	merged := append([]panel.UserInfo(nil), existing...)
	seen := make(map[string]struct{}, len(existing)+len(additions))
	for _, user := range existing {
		seen[user.Uuid] = struct{}{}
	}
	for _, user := range additions {
		if _, ok := seen[user.Uuid]; ok {
			continue
		}
		seen[user.Uuid] = struct{}{}
		merged = append(merged, user)
	}
	return merged
}
