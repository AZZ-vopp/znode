package cmd

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AZZ-vopp/znode/conf"
	"github.com/AZZ-vopp/znode/limiter"
	"github.com/AZZ-vopp/znode/node"
)

func TestStartPreparedRuntimeWithZeroAssignedNodes(t *testing.T) {
	limiter.Init()
	nodes, err := node.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &preparedRuntime{config: conf.New(), nodes: nodes}
	running, err := startPreparedRuntime(prepared, make(chan struct{}, 1))
	if err != nil {
		t.Fatalf("zero-node agent runtime failed to start: %v", err)
	}
	running.Close()
}

func TestReloadRestoresPreviousRuntimeWhenReplacementControllerFails(t *testing.T) {
	limiter.Init()
	oldNodes, err := node.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := startPreparedRuntime(&preparedRuntime{config: conf.New(), nodes: oldNodes}, make(chan struct{}, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldCore := old.core
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port

	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/server/config":
			_, _ = w.Write([]byte(`{"listen_ip":"127.0.0.1","server_port":` + strconv.Itoa(occupiedPort) + `,"protocol":"vmess","network":"tcp","tls":0,"base_config":{"push_interval":60,"pull_interval":60}}`))
		case "/api/v1/server/UniProxy/user":
			_, _ = w.Write([]byte(`{"users":[]}`))
		case "/api/v1/server/UniProxy/alivelist":
			_, _ = w.Write([]byte(`{"alive":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer panel.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	configJSON := `{"Nodes":[{"ApiHost":"` + panel.URL + `","NodeID":12,"ApiKey":"legacy"}]}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0600); err != nil {
		t.Fatal(err)
	}

	_, err = reloadRuntime(configPath, old, make(chan struct{}, 1))
	if err == nil || !strings.Contains(err.Error(), "previous runtime restored") {
		t.Fatalf("expected a restored-runtime error, got %v", err)
	}
	if old.core == nil || old.core == oldCore {
		t.Fatal("previous runtime core was not restored in place")
	}
}
