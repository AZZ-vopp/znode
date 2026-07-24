package agent

import (
	"context"
	"testing"

	panel "github.com/wyx2685/znode/api/v2board"
	"github.com/wyx2685/znode/conf"
)

type fakeManifestFetcher struct {
	manifest *panel.AgentManifest
}

func (f *fakeManifestFetcher) GetManifest(context.Context) (*panel.AgentManifest, error) {
	return f.manifest, nil
}

func TestMonitorSignalsUntilRevisionIsApplied(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	fetcher := &fakeManifestFetcher{manifest: &panel.AgentManifest{Revision: "rev-2", Nodes: []int{1}}}
	monitor := newMonitor(reloadCh, func(conf.AgentConfig) (manifestFetcher, error) {
		return fetcher, nil
	})
	defer monitor.Close()

	config := conf.AgentConfig{Enable: true, APIHost: "https://panel.example", AgentID: "agent", AgentToken: "token", PollInterval: 15}
	if err := monitor.MarkApplied(config, Assignment{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("expected reload signal")
	}

	// A failed reload does not call MarkApplied, so the same desired revision
	// must trigger another attempt on the next poll.
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("expected retry signal for unapplied revision")
	}

	if err := monitor.MarkApplied(config, Assignment{Revision: "rev-2"}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
		t.Fatal("unexpected signal for applied revision")
	default:
	}
}
