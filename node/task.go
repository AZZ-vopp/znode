package node

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/common/task"
	"github.com/AZZ-vopp/znode/conf"
	vCore "github.com/AZZ-vopp/znode/core"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) startTasks(node *panel.NodeInfo) {
	infoDelay := controllerTaskJitter(c.tag+":info", node.PullInterval)
	reportDelay := controllerTaskJitter(c.tag+":report", node.PushInterval)
	reloadCh, err := c.coreExecutor().reloadChannel(context.Background())
	if err != nil {
		log.WithError(err).WithField("tag", c.tag).Warn("Skip background tasks without an active core")
		return
	}
	// fetch node info task
	c.nodeInfoMonitorPeriodic = &task.Task{
		Name:     "nodeInfoMonitor",
		Interval: node.PullInterval,
		Execute:  c.nodeInfoMonitor,
		ReloadCh: reloadCh,
	}
	// fetch user list task
	c.userReportPeriodic = &task.Task{
		Name:     "reportUserTrafficTask",
		Interval: node.PushInterval,
		Execute:  c.reportUserTrafficTask,
		ReloadCh: reloadCh,
	}
	log.WithField("tag", c.tag).Info("Start monitor node status")
	// delay to start nodeInfoMonitor
	_ = c.nodeInfoMonitorPeriodic.StartAfter(false, infoDelay)
	log.WithField("tag", c.tag).Info("Start report node status")
	_ = c.userReportPeriodic.StartAfter(false, reportDelay)
	if node.Security == panel.Tls {
		switch c.info.Common.CertInfo.CertMode {
		case "none", "", "file", "self", "remote":
		default:
			c.renewCertPeriodic = &task.Task{
				Name:     "renewCertTask",
				Interval: time.Hour * 24,
				Execute:  c.renewCertTask,
				ReloadCh: reloadCh,
			}
			log.WithField("tag", c.tag).Info("Start renew cert")
			// delay to start renewCert
			_ = c.renewCertPeriodic.Start(true)
		}
	}
}

func controllerTaskJitter(seed string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	window := interval / 5
	windowMillis := uint64(window / time.Millisecond)
	if windowMillis == 0 {
		return 0
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(seed))
	return time.Duration(uint64(hash.Sum32())%(windowMillis+1)) * time.Millisecond
}

func (c *Controller) nodeInfoMonitor(ctx context.Context) (err error) {
	// get node info
	newN, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return err
		}
		c.recordNodeConfigFailure(err)
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get node info failed")
		// Keep the last-known-good inbound configuration, but still reconcile
		// credentials. GetUserList tries ZBoard first and switches to the signed
		// Redis snapshot only when that live request also fails.
		if fallbackErr := c.syncUserCredentials(ctx); fallbackErr != nil &&
			!errors.Is(fallbackErr, context.Canceled) && !errors.Is(fallbackErr, context.DeadlineExceeded) {
			log.WithFields(log.Fields{"tag": c.tag, "err": fallbackErr}).Warn("Offline user snapshot reconciliation failed")
		}
		return nil
	}
	// A successful 200 or 304/no-change response means the panel is reachable.
	// Restore strict device/IP admission immediately, without an Xray reload.
	c.nodeConfigFailures.Store(0)
	c.SetDeviceLimitBypass(false)
	if newN != nil {
		if reportingThresholdOnlyChange(c.info, newN) {
			c.applyReportingThresholds(newN)
			if err := c.coreExecutor().requestSnapshot(ctx); err != nil {
				return err
			}
			log.WithFields(log.Fields{
				"tag":                       c.tag,
				"node_report_min_traffic":   newN.Common.BaseConfig.NodeReportMinTraffic,
				"device_online_min_traffic": newN.Common.BaseConfig.DeviceOnlineMinTraffic,
			}).Info("Applied reporting thresholds without reloading Xray")
			return c.syncUsers(ctx)
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
		}).Error("Got new node info, reload")
		reloadCh, err := c.coreExecutor().reloadChannel(ctx)
		if err != nil {
			return err
		}
		if reloadCh != nil {
			select {
			case reloadCh <- struct{}{}:
			default:
			}
		} else {
			log.Panic("Reload failed")
		}
	}
	log.WithField("tag", c.tag).Debug("Node info no change")

	return c.syncUsers(ctx)
}

func (c *Controller) recordNodeConfigFailure(err error) {
	if panel.IsControlPlaneAvailabilityError(err) {
		if c.nodeConfigFailures.Add(1) >= 3 {
			c.SetDeviceLimitBypass(true)
		}
		return
	}
	// An authorization or malformed-config failure is not a valid recovery.
	// Reset the consecutive-outage counter but preserve any active bypass;
	// only a valid GetNodeInfo response may restore strict admission.
	c.nodeConfigFailures.Store(0)
}

// reportingThresholdOnlyChange identifies panel changes that affect only
// traffic/online report filtering. Those values are consumed by the periodic
// reporter and do not alter an inbound, route, certificate or Xray policy, so
// draining every live connection for them is both unnecessary and disruptive.
func reportingThresholdOnlyChange(current, next *panel.NodeInfo) bool {
	if current == nil || next == nil || current.Common == nil || next.Common == nil ||
		current.Common.BaseConfig == nil || next.Common.BaseConfig == nil {
		return false
	}

	currentCopy := *current
	nextCopy := *next
	currentCommon := *current.Common
	nextCommon := *next.Common
	currentCommon.BaseConfig = nil
	nextCommon.BaseConfig = nil
	currentCopy.Common = &currentCommon
	nextCopy.Common = &nextCommon

	return reflect.DeepEqual(currentCopy, nextCopy)
}

func (c *Controller) applyReportingThresholds(next *panel.NodeInfo) {
	if next == nil || next.Common == nil || next.Common.BaseConfig == nil {
		return
	}
	c.userSyncMu.Lock()
	defer c.userSyncMu.Unlock()
	if c.info == nil || c.info.Common == nil {
		return
	}
	baseConfig := *next.Common.BaseConfig
	c.info.Common.BaseConfig = &baseConfig
}

func (c *Controller) refreshUsersImmediately() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.syncUserCredentials(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.WithFields(log.Fields{"tag": c.tag, "err": err}).Warn("Fast device sync failed")
	}
}

func (c *Controller) handleDeviceSyncEvent(event deviceSyncEvent) {
	if event.Action == "device.connection_ips_cleared" && validDeviceIPClearEventScope(event) {
		c.userSyncMu.Lock()
		c.applyDeviceIPClearLocked(event.Generation, event.ClearAll, event.CredentialHashes)
		c.userSyncMu.Unlock()
	}
	c.refreshUsersImmediately()
}

func validDeviceIPClearEventScope(event deviceSyncEvent) bool {
	return event.Version == 1 &&
		event.Action == "device.connection_ips_cleared" &&
		normalizeDeviceSyncAPIHost(event.APIHost) != "" &&
		validDeviceIPClearGeneration(event.Generation) != "" &&
		validDeviceIPClearCredentialHashes(event.ClearAll, event.CredentialHashes)
}

// applyDeviceIPClearCommandsLocked requires userSyncMu. Commands are
// idempotent and retained by ZBoard, so an Agent that missed Pub/Sub catches up
// on its next manifest poll without repeatedly clearing active clients.
func (c *Controller) applyDeviceIPClearCommandsLocked(config *conf.GlobalDeviceLimitConfig) {
	if config == nil || c.conf == nil {
		return
	}
	for _, command := range config.ClearCommands {
		if !validDurableDeviceIPClearCommand(command, c.conf.APIHost, config.SyncSigningKey, time.Now()) {
			continue
		}
		c.applyDeviceIPClearLocked(command.Generation, command.ClearAll, command.CredentialHashes)
	}
}

func validDurableDeviceIPClearCommand(command conf.DeviceIPClearCommand, controllerAPIHost, signingKey string, now time.Time) bool {
	if command.Version != 1 || command.Action != "device.connection_ips_cleared" ||
		normalizeDeviceSyncAPIHost(command.APIHost) == "" ||
		normalizeDeviceSyncAPIHost(command.APIHost) != normalizeDeviceSyncAPIHost(controllerAPIHost) ||
		validDeviceIPClearGeneration(command.Generation) == "" ||
		!validDeviceIPClearCredentialHashes(command.ClearAll, command.CredentialHashes) {
		return false
	}
	return validDeviceIPClearEventSignature(deviceSyncEvent{
		Version:          command.Version,
		APIHost:          command.APIHost,
		Action:           command.Action,
		CredentialHashes: command.CredentialHashes,
		ClearAll:         command.ClearAll,
		Generation:       command.Generation,
		At:               command.At,
		Signature:        command.Signature,
	}, signingKey, now)
}

func validDeviceIPClearCredentialHashes(clearAll bool, hashes []string) bool {
	if len(hashes) > conf.MaximumIPsPerUser || (clearAll && len(hashes) != 0) || (!clearAll && len(hashes) == 0) {
		return false
	}
	seen := make(map[string]struct{}, len(hashes))
	for _, value := range hashes {
		if len(value) != sha256.Size*2 {
			return false
		}
		for _, char := range value {
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				return false
			}
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func (c *Controller) applyDeviceIPClearLocked(generation string, clearAll bool, hashes []string) {
	generation = validDeviceIPClearGeneration(generation)
	if generation == "" {
		return
	}
	deviceIPClearJournalMu.Lock()
	defer deviceIPClearJournalMu.Unlock()
	if err := c.loadDeviceIPClearJournalLocked(); err != nil {
		log.WithError(err).WithField("tag", c.tag).Warn("Unable to restore device IP clear journal; applying command defensively")
	}
	if _, applied := c.appliedDeviceIPClears[generation]; applied {
		return
	}
	clearer, ok := c.limiter.(interface{ ClearDeviceIPs(string, []string) })
	if !ok || (!clearAll && len(hashes) == 0) {
		return
	}
	if clearAll {
		clearer.ClearDeviceIPs(c.tag, nil)
	} else {
		clearer.ClearDeviceIPs(c.tag, hashes)
	}
	if err := c.recordDeviceIPClearGenerationLocked(generation); err != nil {
		log.WithError(err).WithField("tag", c.tag).Warn("Unable to persist applied device IP clear generation")
	}
}

func validDeviceIPClearGeneration(value string) string {
	if len(value) != 32 {
		return ""
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return ""
		}
	}
	return value
}

func (c *Controller) syncUsers(ctx context.Context) (err error) {
	return c.syncUsersState(ctx, true)
}

// syncUserCredentials applies additions/removals without waiting for the
// heavier online-device snapshot. The periodic task refreshes that fallback
// map independently, so a slow statistics endpoint cannot delay a new UUID.
func (c *Controller) syncUserCredentials(ctx context.Context) (err error) {
	return c.syncUsersState(ctx, false)
}

func (c *Controller) syncUsersState(ctx context.Context, includeAlive bool) (err error) {
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
		if !includeAlive {
			return fmt.Errorf("get user list: %w", err)
		}
		return nil
	}
	var newA map[int]int
	if includeAlive {
		// Online state is useful for conservative device limiting, but it must
		// not sit in the credential activation critical path.
		var aliveErr error
		newA, aliveErr = c.apiClient.GetUserAlive(ctx)
		if aliveErr != nil {
			if errors.Is(aliveErr, context.Canceled) || errors.Is(aliveErr, context.DeadlineExceeded) {
				return aliveErr
			}
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": aliveErr,
			}).Error("Get alive list failed")
			newA = nil
		}
	}
	// A prior deletion may already have removed runtime credentials but failed
	// to fsync its final counters. Finish that transaction before comparing the
	// latest desired list. In particular, if the panel re-adds the UUID while
	// disk persistence is recovering, the comparison below must see it as an
	// addition and restore the runtime credential.
	if len(c.quiescedUsers) > 0 {
		quiesced, quiesceErr := c.coreExecutor().quiesceUsers(ctx, c.quiescedUsers, c.tag)
		c.quiescedUsers = mergeUsersByCredential(c.quiescedUsers, quiesced)
		if quiesceErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": quiesceErr,
			}).Error("Complete prior user quiesce failed")
			if !includeAlive {
				return fmt.Errorf("complete prior user quiesce: %w", quiesceErr)
			}
			return nil
		}
		if finalizeErr := c.finalizeQuiescedUsersLocked(); finalizeErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": finalizeErr,
			}).Error("Persist final traffic for quiesced users failed")
			if !includeAlive {
				return fmt.Errorf("persist final traffic for quiesced users: %w", finalizeErr)
			}
			return nil
		}
	}
	// node no changed, check users
	if newU == nil {
		if newA != nil {
			c.limiter.UpdateAliveList(newA)
		}
		log.WithField("tag", c.tag).Debug("User list no change")
		return nil
	}
	deleted, added, modified := compareUserList(c.userList, newU)
	if len(deleted) > 0 {
		// Stop new sessions and close old links, but retain UID mappings and
		// counters until the final capture is fsynced. A disk failure keeps the
		// users quiesced (fail closed) and retries the durable capture later.
		quiesced, quiesceErr := c.coreExecutor().quiesceUsers(ctx, deleted, c.tag)
		c.quiescedUsers = mergeUsersByCredential(c.quiescedUsers, quiesced)
		if quiesceErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": quiesceErr,
			}).Error("Quiesce users failed")
			if !includeAlive {
				return fmt.Errorf("quiesce deleted users: %w", quiesceErr)
			}
			return nil
		}
		c.quiescedUsers = mergeUsersByCredential(c.quiescedUsers, deleted)
		if finalizeErr := c.finalizeQuiescedUsersLocked(); finalizeErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": finalizeErr,
			}).Error("Persist final traffic for deleted users failed")
			if !includeAlive {
				return fmt.Errorf("persist final traffic for deleted users: %w", finalizeErr)
			}
			return nil
		}
	}
	if len(added) > 0 {
		// have added users
		_, err = c.coreExecutor().addUsers(ctx, false, &vCore.AddUsersParams{Tag: c.tag, NodeInfo: c.info, Users: added})
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Add users failed")
			if !includeAlive {
				return fmt.Errorf("add users: %w", err)
			}
			return nil
		}
	}
	if len(added) > 0 || len(modified) > 0 {
		// update Limiter
		c.limiter.UpdateUser(c.tag, added, nil, modified)
	}
	c.userList = newU
	if newA != nil {
		c.limiter.UpdateAliveList(newA)
	}
	log.WithField("tag", c.tag).Infof("%d user deleted, %d user added, %d user modified", len(deleted), len(added), len(modified))
	if len(deleted) > 0 || len(added) > 0 || len(modified) > 0 {
		if err := c.coreExecutor().requestSnapshot(ctx); err != nil {
			return err
		}
	}
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
	if err := c.coreExecutor().forgetUsers(context.Background(), completed, c.tag); err != nil {
		return err
	}
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
