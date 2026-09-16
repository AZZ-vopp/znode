package node

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/common/format"
	"github.com/AZZ-vopp/znode/conf"
	"github.com/AZZ-vopp/znode/limiter"
)

func TestDeviceSyncEventDecodesPanelPayload(t *testing.T) {
	var event deviceSyncEvent
	if err := json.Unmarshal([]byte(`{"version":1,"action":"device.connection_ips_cleared","api_host":"https://panel.example","credential_hashes":["c92ef31a9cc3ef5cb3dd4030ebc78cfae74510e22e49a942e3f46c48c6b45f57"],"clear_all":false,"generation":"0123456789abcdef0123456789abcdef"}`), &event); err != nil {
		t.Fatalf("decode sync event: %v", err)
	}
	if event.Version != 1 || event.Action != "device.connection_ips_cleared" || event.APIHost != "https://panel.example" || len(event.CredentialHashes) != 1 || event.Generation == "" {
		t.Fatalf("unexpected event: %+v", event)
	}
}

func TestDurableDeviceIPClearIsAppliedOnce(t *testing.T) {
	previousJournalDirectory := deviceIPClearJournalDirectory
	deviceIPClearJournalDirectory = filepath.Join(t.TempDir(), "device-ip-clears")
	t.Cleanup(func() { deviceIPClearJournalDirectory = previousJournalDirectory })
	limiter.Init()
	const tag = "durable-clear"
	const uuid = "11111111-1111-4111-8111-111111111111"
	zeroGrace := 0
	l := limiter.AddLimiter("vless", tag, []panel.UserInfo{{
		Id: 1, Uuid: uuid, DeviceLimit: 1,
	}}, nil, &conf.GlobalDeviceLimitConfig{MaxIPsPerCredential: 1, HandoverGrace: &zeroGrace}, "https://panel.example")
	defer limiter.DeleteLimiter(tag)
	key := format.UserTag(tag, uuid)
	if _, rejected := l.CheckLimit(context.Background(), key, "198.51.100.1"); rejected {
		t.Fatal("initial device was rejected")
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "198.51.100.2"); !rejected {
		t.Fatal("second IP should be rejected before the clear")
	}
	digest := format.UserCredentialDigest(uuid)
	nodeConfig := &conf.NodeConfig{APIHost: "https://panel.example", NodeID: 42}
	c := &Controller{tag: tag, limiter: l, conf: nodeConfig}
	const generation = "0123456789abcdef0123456789abcdef"
	const signingKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	command := conf.DeviceIPClearCommand{
		Version:          1,
		APIHost:          "https://panel.example",
		Action:           "device.connection_ips_cleared",
		Generation:       generation,
		CredentialHashes: []string{fmt.Sprintf("%x", digest)},
		At:               time.Now().Unix(),
	}
	signDurableDeviceIPClearCommand(&command, signingKey)
	commandConfig := &conf.GlobalDeviceLimitConfig{SyncSigningKey: signingKey, ClearCommands: []conf.DeviceIPClearCommand{command}}
	c.applyDeviceIPClearCommandsLocked(commandConfig)
	if _, rejected := l.CheckLimit(context.Background(), key, "198.51.100.2"); rejected {
		t.Fatal("durable clear did not release the credential IP slot")
	}
	// A replacement controller starts with an empty in-memory generation map.
	// The local journal must stop a retained Agent-manifest command from
	// clearing the newly occupied slot again after a reload or process restart.
	restarted := &Controller{tag: tag, limiter: l, conf: nodeConfig}
	restarted.applyDeviceIPClearCommandsLocked(commandConfig)
	if _, rejected := l.CheckLimit(context.Background(), key, "198.51.100.3"); !rejected {
		t.Fatal("the same clear generation was applied again after restart")
	}
}

func TestDurableDeviceIPClearRejectsUntrustedCommands(t *testing.T) {
	previousJournalDirectory := deviceIPClearJournalDirectory
	deviceIPClearJournalDirectory = filepath.Join(t.TempDir(), "device-ip-clears")
	t.Cleanup(func() { deviceIPClearJournalDirectory = previousJournalDirectory })

	const signingKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newCommand := func() conf.DeviceIPClearCommand {
		command := conf.DeviceIPClearCommand{
			Version:          1,
			APIHost:          "https://panel.example/",
			Action:           "device.connection_ips_cleared",
			Generation:       "0123456789abcdef0123456789abcdef",
			CredentialHashes: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			At:               time.Now().Unix(),
		}
		signDurableDeviceIPClearCommand(&command, signingKey)
		return command
	}

	for _, test := range []struct {
		name   string
		mutate func(*conf.DeviceIPClearCommand)
	}{
		{name: "legacy missing authentication fields", mutate: func(command *conf.DeviceIPClearCommand) {
			command.Version = 0
			command.Action = ""
			command.APIHost = ""
			command.At = 0
			command.Signature = ""
		}},
		{name: "cross panel", mutate: func(command *conf.DeviceIPClearCommand) { command.APIHost = "https://other.example" }},
		{name: "tampered target", mutate: func(command *conf.DeviceIPClearCommand) {
			command.CredentialHashes[0] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{name: "stale", mutate: func(command *conf.DeviceIPClearCommand) { command.At = time.Now().Add(-11 * time.Minute).Unix() }},
		{name: "future", mutate: func(command *conf.DeviceIPClearCommand) { command.At = time.Now().Add(6 * time.Minute).Unix() }},
		{name: "invalid credential hash", mutate: func(command *conf.DeviceIPClearCommand) { command.CredentialHashes[0] = "not-a-sha256" }},
		{name: "ambiguous clear all target", mutate: func(command *conf.DeviceIPClearCommand) { command.ClearAll = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := newCommand()
			test.mutate(&command)
			capture := &captureDeviceIPClearLimiter{}
			controller := &Controller{
				tag:     "durable-clear-validation",
				limiter: capture,
				conf:    &conf.NodeConfig{APIHost: "https://panel.example"},
			}
			controller.applyDeviceIPClearCommandsLocked(&conf.GlobalDeviceLimitConfig{
				SyncSigningKey: signingKey,
				ClearCommands:  []conf.DeviceIPClearCommand{command},
			})
			if len(capture.commands) != 0 {
				t.Fatalf("untrusted durable command reached limiter: %+v", capture.commands)
			}
		})
	}

	t.Run("cross panel signed with another key", func(t *testing.T) {
		command := newCommand()
		command.APIHost = "https://other.example"
		signDurableDeviceIPClearCommand(&command, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		capture := &captureDeviceIPClearLimiter{}
		controller := &Controller{tag: "durable-clear-cross-panel", limiter: capture, conf: &conf.NodeConfig{APIHost: "https://panel.example"}}
		controller.applyDeviceIPClearCommandsLocked(&conf.GlobalDeviceLimitConfig{SyncSigningKey: signingKey, ClearCommands: []conf.DeviceIPClearCommand{command}})
		if len(capture.commands) != 0 {
			t.Fatal("command signed by another panel reached limiter")
		}
	})

	command := newCommand()
	capture := &captureDeviceIPClearLimiter{}
	controller := &Controller{
		tag:     "durable-clear-validation-valid",
		limiter: capture,
		conf:    &conf.NodeConfig{APIHost: "https://panel.example"},
	}
	controller.applyDeviceIPClearCommandsLocked(&conf.GlobalDeviceLimitConfig{
		SyncSigningKey: signingKey,
		ClearCommands:  []conf.DeviceIPClearCommand{command},
	})
	if len(capture.commands) != 1 || capture.commands[0].tag != controller.tag || len(capture.commands[0].hashes) != 1 {
		t.Fatalf("valid durable command was not applied: %+v", capture.commands)
	}

	clearAll := newCommand()
	clearAll.ClearAll = true
	clearAll.CredentialHashes = nil
	signDurableDeviceIPClearCommand(&clearAll, signingKey)
	clearAll.Generation = "fedcba9876543210fedcba9876543210"
	// Generation is part of the canonical input, so sign again after changing it.
	signDurableDeviceIPClearCommand(&clearAll, signingKey)
	clearAllCapture := &captureDeviceIPClearLimiter{}
	clearAllController := &Controller{tag: "durable-clear-validation-all", limiter: clearAllCapture, conf: &conf.NodeConfig{APIHost: "https://panel.example"}}
	clearAllController.applyDeviceIPClearCommandsLocked(&conf.GlobalDeviceLimitConfig{SyncSigningKey: signingKey, ClearCommands: []conf.DeviceIPClearCommand{clearAll}})
	if len(clearAllCapture.commands) != 1 || clearAllCapture.commands[0].hashes != nil {
		t.Fatalf("valid clear-all durable command was not applied: %+v", clearAllCapture.commands)
	}
}

func signDurableDeviceIPClearCommand(command *conf.DeviceIPClearCommand, signingKey string) {
	event := deviceSyncEvent{
		Version:          command.Version,
		APIHost:          command.APIHost,
		Action:           command.Action,
		CredentialHashes: command.CredentialHashes,
		ClearAll:         command.ClearAll,
		Generation:       command.Generation,
		At:               command.At,
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	_, _ = mac.Write([]byte(deviceIPClearSignatureInput(event)))
	command.Signature = hex.EncodeToString(mac.Sum(nil))
}

type captureDeviceIPClearCommand struct {
	tag    string
	hashes []string
}

type captureDeviceIPClearLimiter struct {
	commands []captureDeviceIPClearCommand
}

func (l *captureDeviceIPClearLimiter) UpdateAliveList(map[int]int) {}
func (l *captureDeviceIPClearLimiter) UpdateUser(string, []panel.UserInfo, []panel.UserInfo, []panel.UserInfo) {
}
func (l *captureDeviceIPClearLimiter) GetOnlineDevice() (*[]panel.OnlineUser, error) { return nil, nil }
func (l *captureDeviceIPClearLimiter) SetDeviceLimitBypass(bool) bool                { return false }
func (l *captureDeviceIPClearLimiter) ClearDeviceIPs(tag string, hashes []string) {
	l.commands = append(l.commands, captureDeviceIPClearCommand{tag: tag, hashes: append([]string(nil), hashes...)})
}

func TestDeviceSyncHubKeyIsSharedForIdenticalLogicalNodes(t *testing.T) {
	config := &conf.GlobalDeviceLimitConfig{
		Enable:       true,
		RedisNetwork: "tcp",
		RedisAddr:    "127.0.0.1:6379",
		RedisDB:      3,
		Timeout:      2,
		SyncChannel:  "v2board:device-sync",
	}
	first := deviceSyncKey(config)
	second := deviceSyncKey(config)
	if first != second {
		t.Fatal("identical logical nodes did not resolve to one shared Pub/Sub hub")
	}
	config.SyncChannel = "another-channel"
	if first == deviceSyncKey(config) {
		t.Fatal("different Pub/Sub channels unexpectedly shared one hub")
	}
}

func TestDeviceSyncHubFansOutOnlyToMatchingPanel(t *testing.T) {
	hub := &deviceSyncHub{subscribers: make(map[uint64]*deviceSyncSubscriber)}
	firstCalled := make(chan struct{}, 1)
	secondCalled := make(chan struct{}, 1)
	firstID := hub.addSubscriber("https://panel.example/", "", func(deviceSyncEvent) { firstCalled <- struct{}{} })
	secondID := hub.addSubscriber("https://other.example", "", func(deviceSyncEvent) { secondCalled <- struct{}{} })
	defer hub.removeSubscriber(firstID)
	defer hub.removeSubscriber(secondID)

	hub.dispatch(`{"version":1,"action":"device.unbound","api_host":"https://panel.example"}`)
	select {
	case <-firstCalled:
	case <-time.After(time.Second):
		t.Fatal("matching logical node did not receive the device sync event")
	}
	select {
	case <-secondCalled:
		t.Fatal("device sync event leaked to a logical node on another panel")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestDeviceSyncHubRejectsUnscopedOrInvalidConnectionIPClear(t *testing.T) {
	hub := &deviceSyncHub{subscribers: make(map[uint64]*deviceSyncSubscriber)}
	called := make(chan struct{}, 1)
	const signingKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	id := hub.addSubscriber("https://panel.example", signingKey, func(deviceSyncEvent) { called <- struct{}{} })
	defer hub.removeSubscriber(id)
	now := time.Now().Unix()

	for _, payload := range []string{
		`{"version":1,"action":"device.connection_ips_cleared","clear_all":true,"generation":"0123456789abcdef0123456789abcdef"}`,
		`{"version":2,"action":"device.connection_ips_cleared","api_host":"https://panel.example","clear_all":true,"generation":"0123456789abcdef0123456789abcdef"}`,
		`{"version":1,"action":"device.connection_ips_cleared","api_host":"https://panel.example","clear_all":true,"generation":"invalid"}`,
		`{"version":1,"action":"device.connection_ips_cleared","api_host":"https://other.example","clear_all":true,"generation":"0123456789abcdef0123456789abcdef"}`,
		fmt.Sprintf(`{"version":1,"action":"device.connection_ips_cleared","api_host":"https://panel.example","clear_all":true,"generation":"0123456789abcdef0123456789abcdef","at":%d,"signature":"%s"}`, now, signingKey),
	} {
		hub.dispatch(payload)
	}
	select {
	case <-called:
		t.Fatal("unscoped or invalid connection-IP clear reached the subscriber")
	case <-time.After(25 * time.Millisecond):
	}

	event := deviceSyncEvent{
		Version:    1,
		APIHost:    "https://panel.example/",
		Action:     "device.connection_ips_cleared",
		ClearAll:   true,
		Generation: "0123456789abcdef0123456789abcdef",
		At:         now,
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	_, _ = mac.Write([]byte(deviceIPClearSignatureInput(event)))
	event.Signature = hex.EncodeToString(mac.Sum(nil))
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	hub.dispatch(string(payload))
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("valid scoped connection-IP clear did not reach the subscriber")
	}

	// A valid HMAC alone is insufficient: clear-all must not carry a target
	// list, otherwise malformed scope could be interpreted differently by a
	// future limiter implementation.
	malformed := event
	malformed.Generation = "fedcba9876543210fedcba9876543210"
	malformed.CredentialHashes = []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	mac = hmac.New(sha256.New, []byte(signingKey))
	_, _ = mac.Write([]byte(deviceIPClearSignatureInput(malformed)))
	malformed.Signature = hex.EncodeToString(mac.Sum(nil))
	payload, err = json.Marshal(malformed)
	if err != nil {
		t.Fatal(err)
	}
	hub.dispatch(string(payload))
	select {
	case <-called:
		t.Fatal("semantically malformed but signed connection-IP clear reached the subscriber")
	case <-time.After(25 * time.Millisecond):
	}
}
