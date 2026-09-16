package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	deviceIPClearJournalVersion = 1
	deviceIPClearJournalLimit   = 256
	maxDeviceIPClearJournalSize = 64 << 10
)

var (
	deviceIPClearJournalDirectory = "/var/lib/znode/device-ip-clears"
	deviceIPClearJournalMu        sync.Mutex
)

type deviceIPClearJournalState struct {
	Version     int      `json:"version"`
	Generations []string `json:"generations,omitempty"`
}

func (c *Controller) deviceIPClearJournalPath() string {
	apiHost := ""
	nodeID := 0
	if c.conf != nil {
		apiHost = strings.TrimRight(strings.TrimSpace(c.conf.APIHost), "/")
		nodeID = c.conf.NodeID
	}
	identity := fmt.Sprintf("%s\n%d\n%s", apiHost, nodeID, c.tag)
	digest := sha256.Sum256([]byte(identity))
	return filepath.Join(deviceIPClearJournalDirectory, hex.EncodeToString(digest[:])+".json")
}

func (c *Controller) loadDeviceIPClearJournalLocked() error {
	if c.deviceIPClearJournalLoaded {
		return nil
	}
	c.deviceIPClearJournalLoaded = true
	if c.appliedDeviceIPClears == nil {
		c.appliedDeviceIPClears = make(map[string]struct{})
	}
	state, err := readDeviceIPClearJournal(c.deviceIPClearJournalPath())
	if err != nil || state == nil {
		return err
	}
	for _, generation := range state.Generations {
		c.appliedDeviceIPClears[generation] = struct{}{}
		c.appliedDeviceIPClearOrder = append(c.appliedDeviceIPClearOrder, generation)
	}
	return nil
}

func (c *Controller) recordDeviceIPClearGenerationLocked(generation string) error {
	if c.appliedDeviceIPClears == nil {
		c.appliedDeviceIPClears = make(map[string]struct{})
	}
	if _, exists := c.appliedDeviceIPClears[generation]; exists {
		return nil
	}
	if len(c.appliedDeviceIPClearOrder) >= deviceIPClearJournalLimit {
		oldest := c.appliedDeviceIPClearOrder[0]
		c.appliedDeviceIPClearOrder = c.appliedDeviceIPClearOrder[1:]
		delete(c.appliedDeviceIPClears, oldest)
	}
	c.appliedDeviceIPClears[generation] = struct{}{}
	c.appliedDeviceIPClearOrder = append(c.appliedDeviceIPClearOrder, generation)
	return writeDeviceIPClearJournal(c.deviceIPClearJournalPath(), &deviceIPClearJournalState{
		Version:     deviceIPClearJournalVersion,
		Generations: append([]string(nil), c.appliedDeviceIPClearOrder...),
	})
}

func readDeviceIPClearJournal(path string) (*deviceIPClearJournalState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect device IP clear journal: %w", err)
	}
	if err := ensureDeviceIPClearJournalDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("device IP clear journal is not a regular file")
	}
	if info.Size() < 0 || info.Size() > maxDeviceIPClearJournalSize {
		return nil, fmt.Errorf("device IP clear journal exceeds the size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open device IP clear journal: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxDeviceIPClearJournalSize+1))
	decoder.DisallowUnknownFields()
	var state deviceIPClearJournalState
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode device IP clear journal: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode device IP clear journal: trailing JSON value")
		}
		return nil, fmt.Errorf("decode device IP clear journal: %w", err)
	}
	if err := validateDeviceIPClearJournal(&state); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("protect device IP clear journal: %w", err)
	}
	return &state, nil
}

func writeDeviceIPClearJournal(path string, state *deviceIPClearJournalState) error {
	if err := validateDeviceIPClearJournal(state); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := ensureDeviceIPClearJournalDirectory(directory); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".device-ip-clear-*.tmp")
	if err != nil {
		return fmt.Errorf("create device IP clear journal temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect device IP clear journal temporary file: %w", err)
	}
	if err := json.NewEncoder(temporary).Encode(state); err != nil {
		return fmt.Errorf("encode device IP clear journal: %w", err)
	}
	if info, err := temporary.Stat(); err != nil {
		return fmt.Errorf("inspect device IP clear journal temporary file: %w", err)
	} else if info.Size() > maxDeviceIPClearJournalSize {
		return fmt.Errorf("device IP clear journal exceeds the size limit")
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync device IP clear journal temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close device IP clear journal temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit device IP clear journal: %w", err)
	}
	committed = true
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect device IP clear journal: %w", err)
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open device IP clear journal directory: %w", err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return fmt.Errorf("sync device IP clear journal directory: %w", err)
	}
	return nil
}

func ensureDeviceIPClearJournalDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create device IP clear journal directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect device IP clear journal directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("device IP clear journal directory is not a real directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect device IP clear journal directory: %w", err)
	}
	return nil
}

func validateDeviceIPClearJournal(state *deviceIPClearJournalState) error {
	if state == nil || state.Version != deviceIPClearJournalVersion {
		return fmt.Errorf("invalid device IP clear journal version")
	}
	if len(state.Generations) > deviceIPClearJournalLimit {
		return fmt.Errorf("device IP clear journal has too many generations")
	}
	seen := make(map[string]struct{}, len(state.Generations))
	for _, generation := range state.Generations {
		if validDeviceIPClearGeneration(generation) == "" {
			return fmt.Errorf("device IP clear journal contains an invalid generation")
		}
		if _, exists := seen[generation]; exists {
			return fmt.Errorf("device IP clear journal contains a duplicate generation")
		}
		seen[generation] = struct{}{}
	}
	return nil
}
