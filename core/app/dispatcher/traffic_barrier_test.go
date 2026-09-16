package dispatcher

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/session"
)

func TestUDPContentSniffingChoiceIsScopedToInbound(t *testing.T) {
	legacy := true
	disabled := true
	enabled := false
	d := &DefaultDispatcher{}
	d.ConfigureUDPContentSniffing(legacy)
	d.ConfigureUDPContentSniffingByInbound(map[string]*bool{"node-a": &disabled, "node-b": &enabled})
	if !d.udpContentSniffingDisabled(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "node-a"})) {
		t.Fatal("node-a explicit choice was ignored")
	}
	if d.udpContentSniffingDisabled(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "node-b"})) {
		t.Fatal("node-b explicit choice was ignored")
	}
	if !d.udpContentSniffingDisabled(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "legacy-node"})) {
		t.Fatal("missing panel field must retain local legacy setting")
	}

	other := &DefaultDispatcher{}
	other.ConfigureUDPContentSniffing(false)
	other.ConfigureUDPContentSniffingByInbound(map[string]*bool{"node-a": &enabled})
	if other.udpContentSniffingDisabled(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "node-a"})) {
		t.Fatal("node choice leaked between dispatcher instances")
	}
	if !d.udpContentSniffingDisabled(session.ContextWithInbound(context.Background(), &session.Inbound{Tag: "node-a"})) {
		t.Fatal("configuring a new runtime changed the existing dispatcher")
	}
}

type barrierTestReader struct {
	started     chan struct{}
	interrupted chan struct{}
	release     chan struct{}
	once        sync.Once
	interrupt   sync.Once
}

func (r *barrierTestReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	r.once.Do(func() { close(r.started) })
	<-r.interrupted
	<-r.release
	return buf.MultiBuffer{buf.FromBytes([]byte("final"))}, nil
}

func (r *barrierTestReader) ReadMultiBufferTimeout(time.Duration) (buf.MultiBuffer, error) {
	return r.ReadMultiBuffer()
}

func (r *barrierTestReader) Interrupt() {
	r.interrupt.Do(func() { close(r.interrupted) })
}

type barrierTestWriter struct{}

func (*barrierTestWriter) WriteMultiBuffer(buf.MultiBuffer) error { return nil }
func (*barrierTestWriter) Close() error                           { return nil }

type errorShutdownWriter struct{ err error }

func (*errorShutdownWriter) WriteMultiBuffer(buf.MultiBuffer) error { return nil }
func (w *errorShutdownWriter) Close() error                         { return w.err }

type blockingTrafficWriter struct {
	started chan struct{}
	release chan struct{}
	err     error
}

func (w *blockingTrafficWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	close(w.started)
	<-w.release
	buf.ReleaseMulti(mb)
	return w.err
}

func (*blockingTrafficWriter) Close() error { return nil }

type closeUnblocksTrafficWriter struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

type halfCloseTrafficWriter struct {
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
}

// terminalCloseTarget models session.Inbound.Conn. The protocol reader below
// deliberately does not implement Interrupt, so only closing this target can
// wake its raw read.
type terminalCloseTarget struct {
	closed chan struct{}
	once   sync.Once
}

func (t *terminalCloseTarget) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

type terminalTargetReader struct {
	started chan struct{}
	target  *terminalCloseTarget
	once    sync.Once
}

type closeCountingWriter struct{ closes atomic.Int64 }

func (*closeCountingWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	buf.ReleaseMulti(mb)
	return nil
}
func (w *closeCountingWriter) Close() error {
	w.closes.Add(1)
	return nil
}

func (r *terminalTargetReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	r.once.Do(func() { close(r.started) })
	<-r.target.closed
	return nil, io.EOF
}

func (w *halfCloseTrafficWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	close(w.started)
	select {
	case <-w.release:
		buf.ReleaseMulti(mb)
		return nil
	case <-w.closed:
		buf.ReleaseMulti(mb)
		return io.ErrClosedPipe
	}
}

func (w *halfCloseTrafficWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func (w *closeUnblocksTrafficWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	close(w.started)
	<-w.closed
	buf.ReleaseMulti(mb)
	return io.ErrClosedPipe
}

func (w *closeUnblocksTrafficWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

type blockingShutdownReader struct {
	started chan struct{}
	release chan struct{}
	done    chan struct{}
}

func (*blockingShutdownReader) ReadMultiBuffer() (buf.MultiBuffer, error) { return nil, nil }

func (r *blockingShutdownReader) Interrupt() {
	close(r.started)
	<-r.release
	close(r.done)
}

type blockingShutdownWriter struct {
	started chan struct{}
	release chan struct{}
	done    chan struct{}
}

func (*blockingShutdownWriter) WriteMultiBuffer(buf.MultiBuffer) error { return nil }

func (w *blockingShutdownWriter) Close() error {
	close(w.started)
	<-w.release
	close(w.done)
	return nil
}

type finalPayloadReader struct{}

func (*finalPayloadReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	return buf.MultiBuffer{buf.FromBytes([]byte("final"))}, io.EOF
}

func (r *finalPayloadReader) ReadMultiBufferTimeout(time.Duration) (buf.MultiBuffer, error) {
	return r.ReadMultiBuffer()
}

type terminalErrorReader struct{ err error }

func (r *terminalErrorReader) ReadMultiBuffer() (buf.MultiBuffer, error) { return nil, r.err }

func TestCloseAllWaitsForFinalCounterRead(t *testing.T) {
	reader := &barrierTestReader{
		started: make(chan struct{}), interrupted: make(chan struct{}), release: make(chan struct{}),
	}
	manager := &LinkManager{links: make(map[*ManagedWriter]buf.Reader)}
	writer := &ManagedWriter{writer: &barrierTestWriter{}, manager: manager}
	if !manager.AddLink(writer, reader) {
		t.Fatal("test link was rejected")
	}
	var count atomic.Int64
	counterReader := &CounterReader{Reader: reader, Counter: &count, Manager: manager}
	readDone := make(chan struct{})
	go func() {
		_, _ = counterReader.ReadMultiBuffer()
		close(readDone)
	}()
	<-reader.started

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- manager.CloseAll()
	}()
	<-reader.interrupted
	select {
	case <-closeDone:
		t.Fatal("quiesce returned before the active read published its byte count")
	case <-time.After(20 * time.Millisecond):
	}
	close(reader.release)
	<-readDone
	if err := <-closeDone; err != nil {
		t.Fatalf("close failed after the read drained: %v", err)
	}
	if count.Load() != 5 {
		t.Fatalf("final read count = %d, want 5", count.Load())
	}
}

func TestTrafficWriterRejectsCountsAfterQuiesce(t *testing.T) {
	manager := &LinkManager{links: make(map[*ManagedWriter]buf.Reader)}
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	var count atomic.Int64
	writer := &trafficWriter{writer: &barrierTestWriter{}, manager: manager, counter: &count}
	payload := buf.New()
	_, _ = payload.Write([]byte("late"))
	if err := writer.WriteMultiBuffer(buf.MultiBuffer{payload}); err == nil {
		t.Fatal("late traffic write was accepted after quiesce")
	}
	if payload.Bytes() != nil {
		t.Fatal("rejected writer did not release its MultiBuffer")
	}
	if count.Load() != 0 {
		t.Fatalf("late traffic changed the counter: %d", count.Load())
	}
}

func TestCloseAllWaitsForAcceptedWriteBeforeFinalCounterCapture(t *testing.T) {
	underlying := &blockingTrafficWriter{started: make(chan struct{}), release: make(chan struct{})}
	manager := &LinkManager{links: make(map[*ManagedWriter]buf.Reader), drainTimeout: time.Second}
	managed := &ManagedWriter{writer: underlying, manager: manager}
	reader := &barrierTestReader{interrupted: make(chan struct{})}
	if !manager.AddLink(managed, reader) {
		t.Fatal("test link was rejected")
	}
	var count atomic.Int64
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- (&trafficWriter{writer: underlying, manager: manager, counter: &count}).
			WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("final"))})
	}()
	<-underlying.started

	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.CloseAll() }()
	select {
	case err := <-closeDone:
		t.Fatalf("close escaped an active traffic write: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(underlying.release)
	if err := <-writeDone; err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close failed after write completed: %v", err)
	}
	if got := count.Load(); got != 5 {
		t.Fatalf("accepted write count = %d, want 5", got)
	}
}

func TestQuiesceTagClosesPeerWriterWithPendingTrafficAccounting(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 4, maxConnections: 16, drainTimeout: time.Second}
	peer := &closeUnblocksTrafficWriter{started: make(chan struct{}), closed: make(chan struct{})}
	manager := dispatcher.addManagedLink(
		"node-one|user",
		&ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer},
		&barrierTestReader{interrupted: make(chan struct{})},
	)
	if manager == nil {
		t.Fatal("test link was rejected")
	}

	var count atomic.Int64
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- (&trafficWriter{writer: peer, manager: manager, counter: &count}).
			WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("final"))})
	}()
	<-peer.started

	if err := dispatcher.QuiesceTag("node-one"); err != nil {
		t.Fatalf("quiesce did not close the pending peer write: %v", err)
	}
	if err := <-writeDone; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("peer write error = %v, want io.ErrClosedPipe", err)
	}
	if got := count.Load(); got != 0 {
		t.Fatalf("interrupted peer write changed traffic count: %d", got)
	}
}

func TestQuiesceTagDrainsPeerWriteAfterPrimaryWriterClosed(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 4, maxConnections: 16, drainTimeout: time.Second}
	rawPeer := &closeUnblocksTrafficWriter{started: make(chan struct{}), closed: make(chan struct{})}
	peer := &managedShutdownPeer{writer: rawPeer}
	managed := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer}
	manager := dispatcher.addManagedLink(
		"node-one|user",
		managed,
		&barrierTestReader{interrupted: make(chan struct{})},
	)
	if manager == nil {
		t.Fatal("test link was rejected")
	}
	peer.manager = manager

	var count atomic.Int64
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- (&trafficWriter{writer: peer, manager: manager, counter: &count}).
			WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("final"))})
	}()
	<-rawPeer.started

	if err := managed.Close(); err != nil {
		t.Fatalf("primary writer close failed: %v", err)
	}
	select {
	case err := <-writeDone:
		t.Fatalf("normal primary close terminated peer write: %v", err)
	default:
	}
	if err := dispatcher.QuiesceTag("node-one"); err != nil {
		t.Fatalf("quiesce lost the retired peer shutdown: %v", err)
	}
	if err := <-writeDone; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("peer write error = %v, want io.ErrClosedPipe", err)
	}
	if got := count.Load(); got != 0 {
		t.Fatalf("interrupted peer write changed traffic count: %d", got)
	}
}

func TestQuiesceTagClosesDispatchLinkTerminalTargetForUninterruptibleReader(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 1, maxConnections: 1, drainTimeout: time.Second}
	target := &terminalCloseTarget{closed: make(chan struct{})}
	rawReader := &terminalTargetReader{started: make(chan struct{}), target: target}
	peer := &managedShutdownPeer{target: target}
	reader := &managedTerminalReader{reader: rawReader, shutdownPeer: peer}
	primary := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer}
	manager := dispatcher.addManagedLink("node-one|user", primary, reader)
	if manager == nil {
		t.Fatal("test link was rejected")
	}
	peer.manager = manager

	readDone := make(chan struct{})
	go func() {
		_, _ = (&CounterReader{Reader: reader, Manager: manager}).ReadMultiBuffer()
		close(readDone)
	}()
	<-rawReader.started

	if err := dispatcher.QuiesceTag("node-one"); err != nil {
		t.Fatalf("quiesce did not drain target-backed read: %v", err)
	}
	select {
	case <-target.closed:
	case <-time.After(time.Second):
		t.Fatal("quiesce did not close the terminal target")
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("target close did not wake the uninterruptible reader")
	}
}

func TestDispatchLinkTerminalTargetSurvivesPrimaryCloseUntilQuiesce(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 1, maxConnections: 1, drainTimeout: time.Second}
	target := &terminalCloseTarget{closed: make(chan struct{})}
	rawReader := &terminalTargetReader{started: make(chan struct{}), target: target}
	writer := &closeCountingWriter{}
	peer := &managedShutdownPeer{target: target}
	reader := &managedTerminalReader{reader: rawReader, shutdownPeer: peer}
	primary := &ManagedWriter{writer: writer, shutdownPeer: peer}
	manager := dispatcher.addManagedLink("node-one|user", primary, reader)
	if manager == nil {
		t.Fatal("test link was rejected")
	}
	peer.manager = manager
	if err := primary.Close(); err != nil {
		t.Fatalf("primary close failed: %v", err)
	}
	select {
	case <-target.closed:
		t.Fatal("normal primary half-close closed the terminal target")
	default:
	}
	if !manager.IsAtCapacity() {
		t.Fatal("half-closed terminal target lost its capacity reservation")
	}

	readDone := make(chan struct{})
	go func() {
		_, _ = (&CounterReader{Reader: reader, Manager: manager}).ReadMultiBuffer()
		close(readDone)
	}()
	<-rawReader.started
	if err := dispatcher.QuiesceTag("node-one"); err != nil {
		t.Fatalf("quiesce did not close retained terminal target: %v", err)
	}
	<-readDone
	if got := writer.closes.Load(); got != 1 {
		t.Fatalf("primary writer closes = %d, want exactly one", got)
	}
	if manager.IsAtCapacity() {
		t.Fatal("drained terminal target did not release its reservation")
	}
}

func TestDispatchLinkReaderEOFDoesNotCloseTargetBeforePrimaryResponse(t *testing.T) {
	var active atomic.Int64
	target := &terminalCloseTarget{closed: make(chan struct{})}
	writer := &closeCountingWriter{}
	peer := &managedShutdownPeer{target: target}
	reader := &managedTerminalReader{reader: &finalPayloadReader{}, shutdownPeer: peer}
	primary := &ManagedWriter{writer: writer, shutdownPeer: peer}
	manager := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), activeLinks: &active, maxPerUser: 1, maxGlobal: 1,
	}
	if !manager.AddLink(primary, reader) {
		t.Fatal("test link was rejected")
	}
	primary.manager = manager
	peer.manager = manager

	mb, err := (&CounterReader{Reader: reader, Manager: manager}).ReadMultiBuffer()
	buf.ReleaseMulti(mb)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("reader error = %v, want io.EOF", err)
	}
	select {
	case <-target.closed:
		t.Fatal("reader EOF closed a target needed for downlink response")
	default:
	}
	payload := buf.FromBytes([]byte("reply"))
	if err := primary.WriteMultiBuffer(buf.MultiBuffer{payload}); err != nil {
		t.Fatalf("primary response write failed after reader EOF: %v", err)
	}
	if err := primary.Close(); err != nil {
		t.Fatalf("primary close failed: %v", err)
	}
	if active.Load() != 0 || manager.IsAtCapacity() {
		t.Fatalf("reader-done primary close leaked capacity: active=%d", active.Load())
	}
}

func TestDispatchLinkReadTimeoutDoesNotReleaseTerminalTarget(t *testing.T) {
	var active atomic.Int64
	target := &terminalCloseTarget{closed: make(chan struct{})}
	peer := &managedShutdownPeer{target: target}
	reader := &managedTerminalReader{reader: &terminalErrorReader{err: buf.ErrReadTimeout}, shutdownPeer: peer}
	primary := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer}
	manager := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), activeLinks: &active, maxPerUser: 1, maxGlobal: 1,
	}
	if !manager.AddLink(primary, reader) {
		t.Fatal("test link was rejected")
	}
	primary.manager = manager
	peer.manager = manager
	if _, err := (&CounterReader{Reader: reader, Manager: manager}).ReadMultiBuffer(); !errors.Is(err, buf.ErrReadTimeout) {
		t.Fatalf("read error = %v, want ErrReadTimeout", err)
	}
	if peer.readerDone.Load() {
		t.Fatal("read timeout was treated as terminal")
	}
	if err := primary.Close(); err != nil {
		t.Fatalf("primary close failed: %v", err)
	}
	if active.Load() != 1 || !manager.IsAtCapacity() {
		t.Fatalf("read timeout released retained target: active=%d", active.Load())
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("terminal target close failed: %v", err)
	}
	if active.Load() != 0 {
		t.Fatalf("target close did not release retained capacity: active=%d", active.Load())
	}
}

func TestPrimaryWriterHalfCloseDoesNotTruncatePeerTraffic(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 4, maxConnections: 16, drainTimeout: time.Second}
	rawPeer := &halfCloseTrafficWriter{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	peer := &managedShutdownPeer{writer: rawPeer}
	managed := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer}
	manager := dispatcher.addManagedLink(
		"node-one|user",
		managed,
		&barrierTestReader{interrupted: make(chan struct{})},
	)
	if manager == nil {
		t.Fatal("test link was rejected")
	}
	peer.manager = manager

	var count atomic.Int64
	writeDone := make(chan error, 1)
	if err := managed.Close(); err != nil {
		t.Fatalf("primary writer close failed: %v", err)
	}
	select {
	case <-rawPeer.closed:
		t.Fatal("normal primary close truncated the peer response")
	default:
	}
	go func() {
		writeDone <- (&trafficWriter{writer: peer, manager: manager, counter: &count}).
			WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("reply"))})
	}()
	<-rawPeer.started
	close(rawPeer.release)
	if err := <-writeDone; err != nil {
		t.Fatalf("peer response did not complete normally: %v", err)
	}
	if got := count.Load(); got != 5 {
		t.Fatalf("completed peer response count = %d, want 5", got)
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("peer close failed: %v", err)
	}
	if !manager.CanRemove() {
		t.Fatal("peer close did not release the retired manager")
	}
}

func TestNormalPeerCloseReleasesRetiredPeersWithLongLivedLink(t *testing.T) {
	manager := &LinkManager{links: make(map[*ManagedWriter]buf.Reader), drainTimeout: time.Second}
	longLived := &ManagedWriter{writer: &barrierTestWriter{}, manager: manager}
	if !manager.AddLink(longLived, &barrierTestReader{interrupted: make(chan struct{})}) {
		t.Fatal("long-lived link was rejected")
	}

	for i := 0; i < 8; i++ {
		rawPeer := &halfCloseTrafficWriter{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
		peer := &managedShutdownPeer{writer: rawPeer, manager: manager}
		shortLived := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer, manager: manager}
		if !manager.AddLink(shortLived, &barrierTestReader{interrupted: make(chan struct{})}) {
			t.Fatalf("short-lived link %d was rejected", i)
		}
		if err := shortLived.Close(); err != nil {
			t.Fatalf("short-lived primary close %d failed: %v", i, err)
		}
		manager.mu.RLock()
		retained := len(manager.retiredPeers)
		manager.mu.RUnlock()
		if retained != 1 {
			t.Fatalf("retired peers after primary close = %d, want 1", retained)
		}
		if err := peer.Close(); err != nil {
			t.Fatalf("short-lived peer close %d failed: %v", i, err)
		}
		manager.mu.RLock()
		retained = len(manager.retiredPeers)
		manager.mu.RUnlock()
		if retained != 0 {
			t.Fatalf("normal peer close leaked %d retained peers", retained)
		}
	}
}

func TestRetiredPeerKeepsConnectionCapacityUntilCloseOrQuiesce(t *testing.T) {
	var active atomic.Int64
	manager := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), activeLinks: &active, maxPerUser: 1, maxGlobal: 1,
	}
	rawPeer := &halfCloseTrafficWriter{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	peer := &managedShutdownPeer{writer: rawPeer, manager: manager}
	primary := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer, manager: manager}
	if !manager.AddLink(primary, &barrierTestReader{interrupted: make(chan struct{})}) || active.Load() != 1 {
		t.Fatalf("initial reservation failed: active=%d", active.Load())
	}
	if err := primary.Close(); err != nil {
		t.Fatalf("primary close failed: %v", err)
	}
	if active.Load() != 1 {
		t.Fatalf("retired peer released capacity too early: active=%d", active.Load())
	}
	if manager.AddLink(&ManagedWriter{writer: &barrierTestWriter{}, manager: manager}, &barrierTestReader{interrupted: make(chan struct{})}) {
		t.Fatal("retired peer bypassed per-user/global capacity")
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("peer close failed: %v", err)
	}
	if active.Load() != 0 {
		t.Fatalf("normal peer close did not release capacity: active=%d", active.Load())
	}
	replacementManager := &LinkManager{links: make(map[*ManagedWriter]buf.Reader), activeLinks: &active, maxPerUser: 1, maxGlobal: 1}
	replacement := &ManagedWriter{writer: &barrierTestWriter{}, manager: replacementManager}
	if !replacementManager.AddLink(replacement, &barrierTestReader{interrupted: make(chan struct{})}) || active.Load() != 1 {
		t.Fatalf("capacity did not recover after peer close: active=%d", active.Load())
	}

	quiesceManager := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), activeLinks: &active, maxPerUser: 1, maxGlobal: 1,
	}
	// Release the replacement link above before using the shared global counter.
	if err := replacement.Close(); err != nil {
		t.Fatalf("replacement close failed: %v", err)
	}
	quiesceRawPeer := &halfCloseTrafficWriter{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	quiescePeer := &managedShutdownPeer{writer: quiesceRawPeer, manager: quiesceManager}
	quiescePrimary := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: quiescePeer, manager: quiesceManager}
	if !quiesceManager.AddLink(quiescePrimary, &barrierTestReader{interrupted: make(chan struct{})}) || active.Load() != 1 {
		t.Fatalf("quiesce reservation setup failed: active=%d", active.Load())
	}
	if err := quiescePrimary.Close(); err != nil {
		t.Fatalf("quiesce primary close failed: %v", err)
	}
	if err := quiesceManager.QuiesceAll(); err != nil {
		t.Fatalf("quiesce failed: %v", err)
	}
	if active.Load() != 0 {
		t.Fatalf("quiesce did not release retired peer capacity exactly once: active=%d", active.Load())
	}
}

func TestQuiesceTimeoutKeepsClosingPeerCapacityUntilCloseCompletes(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 1, maxConnections: 1, drainTimeout: 20 * time.Millisecond}
	rawPeer := &blockingShutdownWriter{started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	peer := &managedShutdownPeer{writer: rawPeer}
	primary := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer}
	manager := dispatcher.addManagedLink("node-one|user", primary, &barrierTestReader{interrupted: make(chan struct{})})
	if manager == nil {
		t.Fatal("primary link was rejected")
	}
	peer.manager = manager
	if err := primary.Close(); err != nil {
		t.Fatalf("primary close failed: %v", err)
	}
	if err := dispatcher.QuiesceTag("node-one"); !errors.Is(err, ErrManagedLinkShutdownTimeout) {
		t.Fatalf("quiesce error = %v, want shutdown timeout", err)
	}
	<-rawPeer.started
	dispatcher.ReactivateTag("node-one")
	if got := dispatcher.addManagedLink("node-one|replacement", &ManagedWriter{writer: &barrierTestWriter{}}, &barrierTestReader{interrupted: make(chan struct{})}); got != nil {
		t.Fatal("reactivation admitted a session while the closing peer still owned capacity")
	}
	close(rawPeer.release)
	<-rawPeer.done
	deadline := time.Now().Add(time.Second)
	for {
		if manager.IsAtCapacity() {
			if time.Now().After(deadline) {
				t.Fatal("peer close did not release capacity")
			}
			time.Sleep(time.Millisecond)
			continue
		}
		break
	}
	if got := dispatcher.addManagedLink("node-one|replacement", &ManagedWriter{writer: &barrierTestWriter{}}, &barrierTestReader{interrupted: make(chan struct{})}); got == nil {
		t.Fatal("capacity did not recover after the closing peer completed")
	}
}

func TestQuiesceRetryReusesBlockedPeerShutdown(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 1, maxConnections: 1, drainTimeout: 20 * time.Millisecond}
	rawPeer := &blockingShutdownWriter{started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	peer := &managedShutdownPeer{writer: rawPeer}
	primary := &ManagedWriter{writer: &barrierTestWriter{}, shutdownPeer: peer}
	manager := dispatcher.addManagedLink("node-one|user", primary, &barrierTestReader{interrupted: make(chan struct{})})
	if manager == nil {
		t.Fatal("primary link was rejected")
	}
	peer.manager = manager
	if err := primary.Close(); err != nil {
		t.Fatalf("primary close failed: %v", err)
	}
	if err := dispatcher.QuiesceTag("node-one"); !errors.Is(err, ErrManagedLinkShutdownTimeout) {
		t.Fatalf("first quiesce error = %v, want shutdown timeout", err)
	}
	<-rawPeer.started
	if err := dispatcher.QuiesceTag("node-one"); !errors.Is(err, ErrManagedLinkShutdownTimeout) {
		t.Fatalf("retry quiesce error = %v, want shutdown timeout", err)
	}
	manager.mu.RLock()
	shutdowns := len(manager.shutdowns)
	closing := len(manager.closingPeers)
	manager.mu.RUnlock()
	if shutdowns != 1 || closing != 1 {
		t.Fatalf("quiesce retry duplicated shutdown state: shutdowns=%d closing=%d", shutdowns, closing)
	}
	close(rawPeer.release)
	<-rawPeer.done
	deadline := time.Now().Add(time.Second)
	for manager.IsAtCapacity() {
		if time.Now().After(deadline) {
			t.Fatal("completed peer shutdown did not release capacity")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTrafficWriterDoesNotCountRejectedUnderlyingWrite(t *testing.T) {
	sentinel := errors.New("write rejected")
	underlying := &blockingTrafficWriter{
		started: make(chan struct{}), release: make(chan struct{}), err: sentinel,
	}
	close(underlying.release)
	manager := &LinkManager{links: make(map[*ManagedWriter]buf.Reader)}
	var count atomic.Int64
	err := (&trafficWriter{writer: underlying, manager: manager, counter: &count}).
		WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("unused"))})
	if !errors.Is(err, sentinel) || count.Load() != 0 {
		t.Fatalf("failed write error=%v count=%d, want sentinel/0", err, count.Load())
	}
}

func TestCounterReaderPreservesAndCountsFinalPayloadWithEOF(t *testing.T) {
	var count atomic.Int64
	reader := &CounterReader{Reader: &finalPayloadReader{}, Counter: &count}
	mb, err := reader.ReadMultiBuffer()
	if err != io.EOF {
		t.Fatalf("terminal error = %v, want io.EOF", err)
	}
	defer buf.ReleaseMulti(mb)
	if mb.Len() != 5 || count.Load() != 5 {
		t.Fatalf("final payload len=%d count=%d, want 5/5", mb.Len(), count.Load())
	}
}

func TestCachedReaderDefersEOFUntilCachedPayloadIsDelivered(t *testing.T) {
	reader := &cachedReader{reader: &finalPayloadReader{}}
	payload := buf.New()
	defer payload.Release()

	if err := reader.Cache(payload, time.Second); err != nil {
		t.Fatalf("cache discarded a valid final payload: %v", err)
	}
	if got := string(payload.Bytes()); got != "final" {
		t.Fatalf("sniff payload = %q, want final", got)
	}

	mb, err := reader.ReadMultiBuffer()
	if err != nil {
		buf.ReleaseMulti(mb)
		t.Fatalf("terminal error arrived with cached bytes: %v", err)
	}
	if got := mb.Len(); got != 5 {
		buf.ReleaseMulti(mb)
		t.Fatalf("cached payload length = %d, want 5", got)
	}
	buf.ReleaseMulti(mb)

	mb, err = reader.ReadMultiBuffer()
	buf.ReleaseMulti(mb)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error after cached payload = %v, want io.EOF", err)
	}
}

func TestAdministrativeQuiesceRejectsLateManagedLinksUntilReactivation(t *testing.T) {
	dispatcher := &DefaultDispatcher{}
	if err := dispatcher.QuiesceUser("node|user"); err != nil {
		t.Fatalf("quiesce failed: %v", err)
	}
	writer := &ManagedWriter{writer: &barrierTestWriter{}}
	reader := &barrierTestReader{
		started: make(chan struct{}), interrupted: make(chan struct{}), release: make(chan struct{}),
	}
	if manager := dispatcher.addManagedLink("node|user", writer, reader); manager != nil {
		t.Fatal("a session crossed the administrative quiesce barrier")
	}

	dispatcher.ReactivateUser("node|user")
	writer = &ManagedWriter{writer: &barrierTestWriter{}}
	if manager := dispatcher.addManagedLink("node|user", writer, reader); manager == nil {
		t.Fatal("reactivated user remained behind the quiesce barrier")
	}
}

func TestManagedLinksEnforcePerUserAndGlobalCaps(t *testing.T) {
	reader := &barrierTestReader{
		started: make(chan struct{}), interrupted: make(chan struct{}), release: make(chan struct{}),
	}
	var active atomic.Int64
	first := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), activeLinks: &active, maxPerUser: 1, maxGlobal: 2,
	}
	writerOne := &ManagedWriter{writer: &barrierTestWriter{}, manager: first}
	if !first.AddLink(writerOne, reader) {
		t.Fatal("first managed link was rejected")
	}
	writerTwo := &ManagedWriter{writer: &barrierTestWriter{}, manager: first}
	if first.AddLink(writerTwo, reader) || !first.IsAtCapacity() {
		t.Fatal("per-user connection cap was bypassed")
	}

	second := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), activeLinks: &active, maxPerUser: 2, maxGlobal: 2,
	}
	writerThree := &ManagedWriter{writer: &barrierTestWriter{}, manager: second}
	if !second.AddLink(writerThree, reader) {
		t.Fatal("second global link was rejected too early")
	}
	writerFour := &ManagedWriter{writer: &barrierTestWriter{}, manager: second}
	if second.AddLink(writerFour, reader) || active.Load() != 2 {
		t.Fatalf("global connection cap was bypassed: active=%d", active.Load())
	}

	first.RemoveWriter(writerOne)
	if !second.AddLink(writerFour, reader) || active.Load() != 2 {
		t.Fatalf("released slot was not reusable: active=%d", active.Load())
	}
	if err := second.CloseAll(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if active.Load() != 0 {
		t.Fatalf("closing links leaked global reservations: active=%d", active.Load())
	}
}

func TestClosedUserCounterDrainDoesNotBlockUnrelatedUsers(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 4, maxConnections: 16}
	blockedManager := dispatcher.newLinkManager("node|blocked")
	blockedManager.closed = true
	blockedManager.activeReads = 1
	blockedManager.readsDrained = make(chan struct{})
	dispatcher.LinkManagers.Store("node|blocked", blockedManager)

	blockedDone := make(chan struct{})
	go func() {
		defer close(blockedDone)
		_ = dispatcher.addManagedLink(
			"node|blocked",
			&ManagedWriter{writer: &barrierTestWriter{}},
			&barrierTestReader{},
		)
	}()

	// Give the first goroutine time to reach the retired manager's counter
	// barrier. It must not retain linkManagersMu while it waits.
	time.Sleep(20 * time.Millisecond)
	unrelatedDone := make(chan *LinkManager, 1)
	go func() {
		unrelatedDone <- dispatcher.addManagedLink(
			"node|unrelated",
			&ManagedWriter{writer: &barrierTestWriter{}},
			&barrierTestReader{},
		)
	}()

	select {
	case manager := <-unrelatedDone:
		if manager == nil {
			t.Fatal("unrelated user was rejected while another counter drained")
		}
	case <-time.After(time.Second):
		t.Fatal("one user's counter drain blocked an unrelated session")
	}

	blockedManager.finishCounterRead(nil, 0)
	select {
	case <-blockedDone:
	case <-time.After(time.Second):
		t.Fatal("retired user's session registration did not resume after counter drain")
	}
}

func TestClosedUserRegistrationTimesOutWithoutDroppingAccountingBarrier(t *testing.T) {
	dispatcher := &DefaultDispatcher{
		maxConnectionsPerUser: 4,
		maxConnections:        16,
		drainTimeout:          20 * time.Millisecond,
	}
	manager := dispatcher.newLinkManager("node|blocked")
	manager.closed = true
	manager.activeReads = 1
	manager.readsDrained = make(chan struct{})
	dispatcher.LinkManagers.Store("node|blocked", manager)

	started := time.Now()
	if got := dispatcher.addManagedLink(
		"node|blocked",
		&ManagedWriter{writer: &barrierTestWriter{}},
		&barrierTestReader{},
	); got != nil {
		t.Fatal("registration bypassed an undrained accounting barrier")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("registration drain wait was not bounded: %v", elapsed)
	}
	if stored, ok := dispatcher.LinkManagers.Load("node|blocked"); !ok || stored != manager {
		t.Fatal("timed-out registration dropped the accounting barrier")
	}

	manager.finishCounterRead(nil, 0)
	if got := dispatcher.addManagedLink(
		"node|blocked",
		&ManagedWriter{writer: &barrierTestWriter{}},
		&barrierTestReader{},
	); got == nil {
		t.Fatal("registration did not recover after the accounting drain completed")
	}
}

func TestQuiescedTagRejectsLateSessionsWithoutBlockingOtherTags(t *testing.T) {
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 4, maxConnections: 16}
	if err := dispatcher.QuiesceUser("node-one|removed"); err != nil {
		t.Fatalf("user quiesce failed: %v", err)
	}
	if err := dispatcher.QuiesceTag("node-one"); err != nil {
		t.Fatalf("tag quiesce failed: %v", err)
	}

	if manager := dispatcher.addManagedLink(
		"node-one|user",
		&ManagedWriter{writer: &barrierTestWriter{}},
		&barrierTestReader{},
	); manager != nil {
		t.Fatal("session crossed the inbound quiesce barrier")
	}
	if manager := dispatcher.addManagedLink(
		"node-two|user",
		&ManagedWriter{writer: &barrierTestWriter{}},
		&barrierTestReader{},
	); manager == nil {
		t.Fatal("quiescing one inbound blocked an unrelated node")
	}

	dispatcher.ReactivateTag("node-one")
	if manager := dispatcher.addManagedLink(
		"node-one|active",
		&ManagedWriter{writer: &barrierTestWriter{}},
		&barrierTestReader{},
	); manager == nil {
		t.Fatal("reactivated inbound remained behind the tag barrier")
	}
	if manager := dispatcher.addManagedLink(
		"node-one|removed",
		&ManagedWriter{writer: &barrierTestWriter{}},
		&barrierTestReader{},
	); manager != nil {
		t.Fatal("tag reactivation reopened a user-level quiesce sentinel")
	}
}

func TestCloseAllReturnsDrainTimeoutInsteadOfHanging(t *testing.T) {
	reader := &barrierTestReader{
		started: make(chan struct{}), interrupted: make(chan struct{}), release: make(chan struct{}),
	}
	manager := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), drainTimeout: 20 * time.Millisecond,
	}
	writer := &ManagedWriter{writer: &barrierTestWriter{}, manager: manager}
	if !manager.AddLink(writer, reader) {
		t.Fatal("test link was rejected")
	}
	var count atomic.Int64
	readDone := make(chan struct{})
	go func() {
		_, _ = (&CounterReader{Reader: reader, Counter: &count, Manager: manager}).ReadMultiBuffer()
		close(readDone)
	}()
	<-reader.started

	started := time.Now()
	err := manager.CloseAll()
	if !errors.Is(err, ErrTrafficDrainTimeout) {
		t.Fatalf("close error = %v, want ErrTrafficDrainTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded close took too long: %v", elapsed)
	}

	close(reader.release)
	<-readDone
	if err := manager.WaitForCounterReads(); err != nil {
		t.Fatalf("read did not eventually leave the barrier: %v", err)
	}
}

func TestCloseAllBoundsBlockingShutdownHooks(t *testing.T) {
	reader := &blockingShutdownReader{
		started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}),
	}
	underlyingWriter := &blockingShutdownWriter{
		started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}),
	}
	releaseHooks := func() {
		select {
		case <-reader.release:
		default:
			close(reader.release)
		}
		select {
		case <-underlyingWriter.release:
		default:
			close(underlyingWriter.release)
		}
	}
	defer releaseHooks()
	manager := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), drainTimeout: 20 * time.Millisecond,
	}
	writer := &ManagedWriter{writer: underlyingWriter, manager: manager}
	if !manager.AddLink(writer, reader) {
		t.Fatal("test link was rejected")
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- manager.CloseAll() }()
	select {
	case err := <-closeResult:
		if !errors.Is(err, ErrManagedLinkShutdownTimeout) {
			t.Fatalf("close error = %v, want ErrManagedLinkShutdownTimeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking shutdown hooks made close hang")
	}
	select {
	case <-reader.started:
	default:
		t.Fatal("reader interrupt was not attempted")
	}
	select {
	case <-underlyingWriter.started:
	default:
		t.Fatal("writer close was not attempted")
	}
	if err := manager.CloseAll(); !errors.Is(err, ErrManagedLinkShutdownTimeout) {
		t.Fatalf("close retry forgot pending shutdown hooks: %v", err)
	}

	releaseHooks()
	<-reader.done
	<-underlyingWriter.done
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("close retry failed after shutdown hooks completed: %v", err)
	}
}

func TestQuiesceTagContextUsesCallerDeadlineForBlockingShutdownHooks(t *testing.T) {
	reader := &blockingShutdownReader{started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	writer := &blockingShutdownWriter{started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	defer func() {
		close(reader.release)
		close(writer.release)
		<-reader.done
		<-writer.done
	}()
	manager := &LinkManager{links: make(map[*ManagedWriter]buf.Reader), drainTimeout: time.Second}
	managedWriter := &ManagedWriter{writer: writer, manager: manager}
	if !manager.AddLink(managedWriter, reader) {
		t.Fatal("test link was rejected")
	}
	dispatcher := &DefaultDispatcher{maxConnectionsPerUser: 4, maxConnections: 16}
	dispatcher.LinkManagers.Store("terminal|user", manager)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := dispatcher.QuiesceTagContext(ctx, "terminal")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("quiesce error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("quiesce ignored caller deadline for %v", elapsed)
	}
}

func TestCloseAllDoesNotReplayCompletedShutdownErrorForever(t *testing.T) {
	sentinel := errors.New("writer close failed")
	reader := &barrierTestReader{interrupted: make(chan struct{})}
	manager := &LinkManager{links: make(map[*ManagedWriter]buf.Reader), drainTimeout: time.Second}
	writer := &ManagedWriter{writer: &errorShutdownWriter{err: sentinel}, manager: manager}
	if !manager.AddLink(writer, reader) {
		t.Fatal("test link was rejected")
	}
	if err := manager.CloseAll(); !errors.Is(err, sentinel) {
		t.Fatalf("first close error = %v, want sentinel", err)
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("completed close error poisoned every retry: %v", err)
	}
}

func TestQuiesceUserReturnsDrainTimeoutAndKeepsRejectionBarrier(t *testing.T) {
	dispatcher := &DefaultDispatcher{
		maxConnectionsPerUser: 4,
		maxConnections:        16,
		drainTimeout:          20 * time.Millisecond,
	}
	reader := &barrierTestReader{
		started: make(chan struct{}), interrupted: make(chan struct{}), release: make(chan struct{}),
	}
	manager := dispatcher.addManagedLink(
		"node|blocked",
		&ManagedWriter{writer: &barrierTestWriter{}},
		reader,
	)
	if manager == nil {
		t.Fatal("test link was rejected")
	}
	readDone := make(chan struct{})
	go func() {
		_, _ = (&CounterReader{Reader: reader, Manager: manager}).ReadMultiBuffer()
		close(readDone)
	}()
	<-reader.started

	if err := dispatcher.QuiesceUser("node|blocked"); !errors.Is(err, ErrTrafficDrainTimeout) {
		t.Fatalf("quiesce error = %v, want ErrTrafficDrainTimeout", err)
	}
	if got := dispatcher.addManagedLink(
		"node|blocked",
		&ManagedWriter{writer: &barrierTestWriter{}},
		reader,
	); got != nil {
		t.Fatal("timed-out quiesce dropped its rejection barrier")
	}

	close(reader.release)
	<-readDone
	if err := dispatcher.QuiesceUser("node|blocked"); err != nil {
		t.Fatalf("quiesce retry failed after read drained: %v", err)
	}
}

func TestTimeoutWrapperRawReadStaysInsideTrafficBarrier(t *testing.T) {
	reader := &barrierTestReader{
		started: make(chan struct{}), interrupted: make(chan struct{}), release: make(chan struct{}),
	}
	manager := &LinkManager{
		links: make(map[*ManagedWriter]buf.Reader), drainTimeout: time.Second,
	}
	writer := &ManagedWriter{writer: &barrierTestWriter{}, manager: manager}
	if !manager.AddLink(writer, reader) {
		t.Fatal("test link was rejected")
	}
	var count atomic.Int64
	timeoutReader := newManagedTimeoutReader(reader, &count, manager)

	mb, err := timeoutReader.ReadMultiBufferTimeout(5 * time.Millisecond)
	if err != nil || !mb.IsEmpty() {
		buf.ReleaseMulti(mb)
		t.Fatalf("timeout result = len %d, %v; want empty, nil", mb.Len(), err)
	}
	<-reader.started

	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.CloseAll() }()
	<-reader.interrupted
	select {
	case err := <-closeDone:
		t.Fatalf("close escaped the wrapper's raw read barrier: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(reader.release)
	if err := <-closeDone; err != nil {
		t.Fatalf("close failed after raw read completed: %v", err)
	}
	mb, err = timeoutReader.ReadMultiBuffer()
	defer buf.ReleaseMulti(mb)
	if err != nil || mb.Len() != 5 || count.Load() != 5 {
		t.Fatalf("raw result len=%d count=%d err=%v, want 5/5/nil", mb.Len(), count.Load(), err)
	}
}
