package engine

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

var AvailableEngines = []string{"torrserver", "rqbit", "qbittorrent"}

type MultiEngine struct {
	mu       sync.RWMutex
	adapters map[string]Engine
	active   Engine
}

func NewMultiEngine(torrserver, rqbit, qbittorrent Engine) *MultiEngine {
	m := &MultiEngine{
		adapters: map[string]Engine{
			"torrserver":  torrserver,
			"rqbit":       rqbit,
			"qbittorrent": qbittorrent,
		},
	}
	m.active = torrserver
	return m
}

func (m *MultiEngine) SetActive(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	adapter, ok := m.adapters[name]
	if !ok {
		return fmt.Errorf("unknown engine %q", name)
	}
	m.active = adapter
	return nil
}

func (m *MultiEngine) GetActive() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active.Name()
}

func (m *MultiEngine) Adapter(name string) Engine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.adapters[name]
}

func (m *MultiEngine) current() Engine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func (m *MultiEngine) Name() string {
	return m.current().Name()
}

func (m *MultiEngine) AddTorrent(ctx context.Context, magnetURI string) (*TorrentInfo, error) {
	return m.current().AddTorrent(ctx, magnetURI)
}

func (m *MultiEngine) PreloadTorrent(ctx context.Context, magnetURI string) (*TorrentInfo, error) {
	return m.current().PreloadTorrent(ctx, magnetURI)
}

func (m *MultiEngine) StreamFile(ctx context.Context, infoHash string, fileIndex int, req *http.Request) (*StreamResponse, error) {
	return m.current().StreamFile(ctx, infoHash, fileIndex, req)
}

func (m *MultiEngine) RemoveTorrent(ctx context.Context, infoHash string, deleteFiles bool) error {
	return m.current().RemoveTorrent(ctx, infoHash, deleteFiles)
}

func (m *MultiEngine) GetTorrent(ctx context.Context, infoHash string) (*TorrentInfo, error) {
	return m.current().GetTorrent(ctx, infoHash)
}

func (m *MultiEngine) ListTorrents(ctx context.Context) ([]TorrentInfo, error) {
	return m.current().ListTorrents(ctx)
}

func (m *MultiEngine) Ping(ctx context.Context) error {
	return m.current().Ping(ctx)
}
