package engine

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ParseInfoHashFromMagnet extracts the info hash from a magnet URI and returns
// it normalized to lowercase. Returns empty string if parsing fails.
func ParseInfoHashFromMagnet(magnetURI string) string {
	u, err := url.Parse(magnetURI)
	if err != nil {
		return ""
	}

	xt := u.Query().Get("xt")
	if xt == "" {
		return ""
	}

	// xt format: urn:btih:HASH
	parts := strings.Split(xt, ":")
	if len(parts) < 3 || strings.ToLower(parts[1]) != "btih" {
		return ""
	}

	return strings.ToLower(parts[2])
}

// TorrentStats holds live runtime statistics for an active torrent.
type TorrentStats struct {
	DownloadSpeed    float64 `json:"downloadSpeed"`    // bytes/sec
	UploadSpeed      float64 `json:"uploadSpeed"`      // bytes/sec
	ActivePeers      int     `json:"activePeers"`
	TotalPeers       int     `json:"totalPeers"`
	ConnectedSeeders int     `json:"connectedSeeders"`
}

// TorrentInfo holds metadata about an added torrent
type TorrentInfo struct {
	InfoHash  string        `json:"infoHash"`
	Name      string        `json:"name"`
	Files     []TorrentFile `json:"files"`
	EngineID  string        `json:"engineId"`  // Internal engine ID (rqbit uses numeric IDs)
	TotalSize int64         `json:"totalSize"` // Total size in bytes (from engine metadata)
	Stats     *TorrentStats `json:"stats,omitempty"`
}

// TorrentFile represents a single file within a torrent
type TorrentFile struct {
	Index int    `json:"index"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
}

// StreamResponse wraps the engine's streaming response for the proxy to forward
type StreamResponse struct {
	Body          io.ReadCloser
	ContentLength int64
	ContentType   string
	StatusCode    int
	Header        http.Header // Pass through Range-related headers
}

// Engine defines the contract all torrent engine adapters must fulfill
type Engine interface {
	// Name returns a human-readable engine identifier ("torrserver", "rqbit", "qbittorrent")
	Name() string

	// AddTorrent sends a magnet link to the engine. Must be idempotent.
	AddTorrent(ctx context.Context, magnetURI string) (*TorrentInfo, error)

	// StreamFile proxies the video stream from the engine.
	// req is the original HTTP request - adapter forwards Range headers.
	StreamFile(ctx context.Context, infoHash string, fileIndex int, req *http.Request) (*StreamResponse, error)

	// RemoveTorrent removes a torrent. deleteFiles controls whether downloaded files are also removed.
	RemoveTorrent(ctx context.Context, infoHash string, deleteFiles bool) error

	// GetTorrent returns info about a specific torrent, or nil if not found.
	GetTorrent(ctx context.Context, infoHash string) (*TorrentInfo, error)

	// ListTorrents returns all torrents known to this engine.
	ListTorrents(ctx context.Context) ([]TorrentInfo, error)

	// Ping checks if the engine is reachable.
	Ping(ctx context.Context) error
}
