package dispatcher

import (
	sync "sync"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

type ManagedWriter struct {
	writer  buf.Writer
	manager *LinkManager
}

func (w *ManagedWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	return w.writer.WriteMultiBuffer(mb)
}

func (w *ManagedWriter) Close() error {
	w.manager.RemoveWriter(w)
	return common.Close(w.writer)
}

type LinkManager struct {
	links   map[*ManagedWriter]buf.Reader
	mu      sync.RWMutex
	closed  bool
	onEmpty func(*LinkManager)
}

func (m *LinkManager) AddLink(writer *ManagedWriter, reader buf.Reader) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.links[writer] = reader
		return true
	}
	return false
}

func (m *LinkManager) RemoveWriter(writer *ManagedWriter) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	delete(m.links, writer)
	if len(m.links) == 0 {
		m.closed = true
		onEmpty := m.onEmpty
		m.mu.Unlock()
		if onEmpty != nil {
			onEmpty(m)
		}
		return
	}
	m.mu.Unlock()
}

func (m *LinkManager) CloseAll() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true

	links := m.links
	m.links = make(map[*ManagedWriter]buf.Reader)
	m.mu.Unlock()

	for w, r := range links {
		common.Close(w.writer)
		common.Interrupt(r)
	}
}
