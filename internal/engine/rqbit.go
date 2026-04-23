package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/krizcold/stremio-torrent-bridge/pkg/httpclient"
)

// rqbitStatsDecodeErrOnce limits stats-decode warnings to a single line across
// the adapter's lifetime so a schema drift doesn't flood logs on every poll.
var rqbitStatsDecodeErrOnce sync.Once

// RqbitAdapter implements Engine for rqbit (github.com/ikatson/rqbit)
type RqbitAdapter struct {
	baseURL      string
	username     string
	password     string
	client       *http.Client // For API calls (30s timeout)
	streamClient *http.Client // For streaming (no timeout)

	mu       sync.RWMutex
	hashToID map[string]int // infoHash (lowercase) -> rqbit numeric ID
	idToHash map[int]string // rqbit numeric ID -> infoHash (lowercase)
}

// NewRqbitAdapter creates a new rqbit engine adapter.
// If username and password are non-empty, HTTP Basic Auth is used on all requests.
func NewRqbitAdapter(baseURL, username, password string) *RqbitAdapter {
	return &RqbitAdapter{
		baseURL:      strings.TrimRight(baseURL, "/"),
		username:     username,
		password:     password,
		client:       httpclient.New(),
		streamClient: httpclient.NewStreaming(),
		hashToID:     make(map[string]int),
		idToHash:     make(map[int]string),
	}
}

// setAuth adds Basic Auth to a request if credentials are configured.
func (r *RqbitAdapter) setAuth(req *http.Request) {
	if r.username != "" && r.password != "" {
		req.SetBasicAuth(r.username, r.password)
	}
}

// rqbit API response types

// rqbitAddResponse represents the response from POST /torrents
type rqbitAddResponse struct {
	ID      int              `json:"id"`
	Details *rqbitAddDetails `json:"details"`
}

// rqbitAddDetails holds the details returned when adding a torrent
type rqbitAddDetails struct {
	InfoHash string         `json:"info_hash"`
	Name     string         `json:"name"`
	Files    []rqbitAddFile `json:"files"`
}

// rqbitAddFile represents a file in the add torrent response
type rqbitAddFile struct {
	Name     string `json:"name"`
	Length   int64  `json:"length"`
	Included bool   `json:"included"`
}

// rqbitTorrentDetail represents the response from GET /torrents/{id}
type rqbitTorrentDetail struct {
	InfoHash string          `json:"info_hash"`
	Name     string          `json:"name"`
	Files    []rqbitFileInfo `json:"files"`
	Stats    json.RawMessage `json:"stats"`
}

// rqbitFileInfo represents a file in the torrent detail response
type rqbitFileInfo struct {
	Name     string `json:"name"`
	Length   int64  `json:"length"`
	Included bool   `json:"included"`
}

// rqbitStats mirrors the relevant subset of rqbit's TorrentStats JSON.
// See rqbit source: crates/librqbit/src/torrent_state/stats.rs
type rqbitStats struct {
	State string          `json:"state"` // "initializing" | "live" | "paused" | "error"
	Live  *rqbitLiveStats `json:"live"`  // null when torrent is paused/initializing/errored
}

// rqbitLiveStats holds the runtime stats only present while a torrent is live.
type rqbitLiveStats struct {
	Snapshot      rqbitStatsSnapshot `json:"snapshot"`
	DownloadSpeed rqbitSpeed         `json:"download_speed"`
	UploadSpeed   rqbitSpeed         `json:"upload_speed"`
}

// rqbitStatsSnapshot embeds the peer-counter aggregation we care about.
type rqbitStatsSnapshot struct {
	PeerStats rqbitPeerStats `json:"peer_stats"`
}

// rqbitPeerStats is rqbit's AggregatePeerStats. `live` is the current active
// peer count; `seen` is the cumulative count of peers ever observed.
type rqbitPeerStats struct {
	Queued     int `json:"queued"`
	Connecting int `json:"connecting"`
	Live       int `json:"live"`
	Seen       int `json:"seen"`
	Dead       int `json:"dead"`
	NotNeeded  int `json:"not_needed"`
}

// rqbitSpeed is rqbit's Speed struct. Despite the `mbps` name, the value is
// mebibytes per second (MiB/s) -- rqbit's as_bytes() multiplies by 1024*1024.
type rqbitSpeed struct {
	Mbps float64 `json:"mbps"`
}

// bytesPerSec converts rqbit's MiB/s value to bytes/sec for TorrentStats.
func (s rqbitSpeed) bytesPerSec() float64 {
	return s.Mbps * 1024 * 1024
}

// rqbitListResponse handles both array and object response formats.
// rqbit may return a JSON array of torrent details directly, or an object
// like {"torrents": [...]}. We handle both in ListTorrents.

func (r *RqbitAdapter) Name() string {
	return "rqbit"
}

func (r *RqbitAdapter) PreloadTorrent(ctx context.Context, magnetURI string) (*TorrentInfo, error) {
	return r.AddTorrent(ctx, magnetURI)
}

func (r *RqbitAdapter) AddTorrent(ctx context.Context, magnetURI string) (*TorrentInfo, error) {
	// Extract info hash from the magnet URI for idempotency check
	infoHash := ParseInfoHashFromMagnet(magnetURI)

	// Check if we already have this torrent mapped
	if infoHash != "" {
		r.mu.RLock()
		id, exists := r.hashToID[infoHash]
		r.mu.RUnlock()

		if exists {
			// Already added; just return the current info
			return r.getTorrentByID(ctx, id, infoHash)
		}
	}

	// POST the magnet URI to rqbit
	reqURL := r.baseURL + "/torrents?overwrite=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(magnetURI))
	if err != nil {
		return nil, fmt.Errorf("rqbit add torrent: create request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	r.setAuth(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rqbit add torrent: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rqbit add torrent: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rqbit add torrent: unexpected status %d: %s", resp.StatusCode, string(data))
	}

	var addResp rqbitAddResponse
	if err := json.Unmarshal(data, &addResp); err != nil {
		return nil, fmt.Errorf("rqbit add torrent: parse response: %w", err)
	}

	// Determine the info hash from the response or from our magnet parse
	respHash := ""
	if addResp.Details != nil && addResp.Details.InfoHash != "" {
		respHash = strings.ToLower(addResp.Details.InfoHash)
	} else if infoHash != "" {
		respHash = infoHash
	}

	// If we got the hash and details directly from the add response, use them
	if respHash != "" && addResp.Details != nil && addResp.Details.Name != "" {
		r.mu.Lock()
		r.hashToID[respHash] = addResp.ID
		r.idToHash[addResp.ID] = respHash
		r.mu.Unlock()

		files := make([]TorrentFile, 0, len(addResp.Details.Files))
		for i, f := range addResp.Details.Files {
			files = append(files, TorrentFile{
				Index: i,
				Path:  f.Name,
				Size:  f.Length,
			})
		}

		return &TorrentInfo{
			InfoHash: respHash,
			Name:     addResp.Details.Name,
			Files:    files,
			EngineID: strconv.Itoa(addResp.ID),
		}, nil
	}

	// Details not fully available in add response; fetch via GET /torrents/{id}
	r.mu.Lock()
	if respHash != "" {
		r.hashToID[respHash] = addResp.ID
		r.idToHash[addResp.ID] = respHash
	}
	r.mu.Unlock()

	info, err := r.getTorrentByID(ctx, addResp.ID, respHash)
	if err != nil {
		return nil, fmt.Errorf("rqbit add torrent: get details: %w", err)
	}

	// If we didn't have the hash before, update our mapping now
	if respHash == "" && info.InfoHash != "" {
		r.mu.Lock()
		r.hashToID[info.InfoHash] = addResp.ID
		r.idToHash[addResp.ID] = info.InfoHash
		r.mu.Unlock()
	}

	return info, nil
}

func (r *RqbitAdapter) StreamFile(ctx context.Context, infoHash string, fileIndex int, req *http.Request) (*StreamResponse, error) {
	hash := strings.ToLower(infoHash)

	// Look up numeric ID from hash
	r.mu.RLock()
	id, exists := r.hashToID[hash]
	r.mu.RUnlock()

	if !exists {
		// Try refreshing the mapping from rqbit
		if _, err := r.ListTorrents(ctx); err != nil {
			return nil, fmt.Errorf("rqbit stream: refresh mapping failed: %w", err)
		}

		r.mu.RLock()
		id, exists = r.hashToID[hash]
		r.mu.RUnlock()

		if !exists {
			return nil, fmt.Errorf("rqbit stream: torrent %s not found (add it first)", hash)
		}
	}

	streamURL := fmt.Sprintf("%s/torrents/%d/stream/%d", r.baseURL, id, fileIndex)

	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("rqbit stream: create request: %w", err)
	}

	// Forward Range-related headers from the original request
	forwardHeaders := []string{"Range", "If-Range", "If-None-Match"}
	for _, h := range forwardHeaders {
		if v := req.Header.Get(h); v != "" {
			streamReq.Header.Set(h, v)
		}
	}
	r.setAuth(streamReq)

	resp, err := r.streamClient.Do(streamReq)
	if err != nil {
		return nil, fmt.Errorf("rqbit stream: request failed: %w", err)
	}

	return &StreamResponse{
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		ContentType:   resp.Header.Get("Content-Type"),
		StatusCode:    resp.StatusCode,
		Header:        resp.Header,
	}, nil
}

func (r *RqbitAdapter) RemoveTorrent(ctx context.Context, infoHash string, deleteFiles bool) error {
	hash := strings.ToLower(infoHash)

	r.mu.RLock()
	id, exists := r.hashToID[hash]
	r.mu.RUnlock()

	if !exists {
		return fmt.Errorf("rqbit remove: torrent %s not found", hash)
	}

	// POST /torrents/{id}/delete removes torrent and files
	// POST /torrents/{id}/forget removes torrent but keeps files
	action := "forget"
	if deleteFiles {
		action = "delete"
	}

	reqURL := fmt.Sprintf("%s/torrents/%d/%s", r.baseURL, id, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return fmt.Errorf("rqbit remove: create request: %w", err)
	}
	r.setAuth(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("rqbit remove: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rqbit remove: unexpected status %d: %s", resp.StatusCode, string(errBody))
	}

	// Clean up the mapping
	r.mu.Lock()
	delete(r.hashToID, hash)
	delete(r.idToHash, id)
	r.mu.Unlock()

	return nil
}

func (r *RqbitAdapter) GetTorrent(ctx context.Context, infoHash string) (*TorrentInfo, error) {
	hash := strings.ToLower(infoHash)

	r.mu.RLock()
	id, exists := r.hashToID[hash]
	r.mu.RUnlock()

	if !exists {
		// Try refreshing the mapping
		if _, err := r.ListTorrents(ctx); err != nil {
			return nil, nil
		}

		r.mu.RLock()
		id, exists = r.hashToID[hash]
		r.mu.RUnlock()

		if !exists {
			return nil, nil
		}
	}

	return r.getTorrentByID(ctx, id, hash)
}

func (r *RqbitAdapter) ListTorrents(ctx context.Context) ([]TorrentInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/torrents", nil)
	if err != nil {
		return nil, fmt.Errorf("rqbit list torrents: create request: %w", err)
	}
	r.setAuth(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rqbit list torrents: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rqbit list torrents: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rqbit list torrents: unexpected status %d: %s", resp.StatusCode, string(data))
	}

	// rqbit may return an array of torrent details directly, or an object
	// wrapping an array. Try both formats.
	var torrents []rqbitTorrentDetail

	// First try: direct array
	if err := json.Unmarshal(data, &torrents); err != nil {
		// Second try: object with a "torrents" field
		var wrapper struct {
			Torrents []rqbitTorrentDetail `json:"torrents"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			// Third try: object keyed by numeric ID (e.g., {"0": {...}, "1": {...}})
			var idMap map[string]rqbitTorrentDetail
			if err3 := json.Unmarshal(data, &idMap); err3 != nil {
				return nil, fmt.Errorf("rqbit list torrents: parse response: %w (also tried object: %v, map: %v)", err, err2, err3)
			}
			torrents = make([]rqbitTorrentDetail, 0, len(idMap))
			// Rebuild mapping from the numeric key IDs
			r.mu.Lock()
			for idStr, t := range idMap {
				numID, parseErr := strconv.Atoi(idStr)
				if parseErr != nil {
					continue
				}
				hash := strings.ToLower(t.InfoHash)
				if hash != "" {
					r.hashToID[hash] = numID
					r.idToHash[numID] = hash
				}
				torrents = append(torrents, t)
			}
			r.mu.Unlock()

			return r.torrentDetailsToInfoSlice(torrents), nil
		}
		torrents = wrapper.Torrents
	}

	// Rebuild the mapping from list results
	r.mu.Lock()
	for i, t := range torrents {
		hash := strings.ToLower(t.InfoHash)
		if hash != "" {
			r.hashToID[hash] = i
			r.idToHash[i] = hash
		}
	}
	r.mu.Unlock()

	return r.torrentDetailsToInfoSlice(torrents), nil
}

func (r *RqbitAdapter) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/stats", nil)
	if err != nil {
		return fmt.Errorf("rqbit ping: create request: %w", err)
	}
	r.setAuth(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("rqbit ping: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rqbit ping: unexpected status %d", resp.StatusCode)
	}

	return nil
}

// getTorrentByID fetches a single torrent's details by its numeric rqbit ID.
// If knownHash is non-empty, it is used as the info hash (avoids needing to
// parse it from the response if the response format lacks it).
func (r *RqbitAdapter) getTorrentByID(ctx context.Context, id int, knownHash string) (*TorrentInfo, error) {
	reqURL := fmt.Sprintf("%s/torrents/%d", r.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("rqbit get torrent %d: create request: %w", id, err)
	}
	r.setAuth(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rqbit get torrent %d: request failed: %w", id, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rqbit get torrent %d: read response: %w", id, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rqbit get torrent %d: unexpected status %d: %s", id, resp.StatusCode, string(data))
	}

	var detail rqbitTorrentDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return nil, fmt.Errorf("rqbit get torrent %d: parse response: %w", id, err)
	}

	hash := strings.ToLower(detail.InfoHash)
	if hash == "" {
		hash = knownHash
	}

	// Update mapping if we learned something new
	if hash != "" {
		r.mu.Lock()
		r.hashToID[hash] = id
		r.idToHash[id] = hash
		r.mu.Unlock()
	}

	files := make([]TorrentFile, 0, len(detail.Files))
	for i, f := range detail.Files {
		files = append(files, TorrentFile{
			Index: i,
			Path:  f.Name,
			Size:  f.Length,
		})
	}

	var totalSize int64
	for _, f := range files {
		totalSize += f.Size
	}

	return &TorrentInfo{
		InfoHash:  hash,
		Name:      detail.Name,
		Files:     files,
		EngineID:  strconv.Itoa(id),
		TotalSize: totalSize,
		Stats:     parseRqbitStats(detail.Stats),
	}, nil
}

// parseStats decodes the Stats raw JSON from a rqbitTorrentDetail into a
// *TorrentStats. Returns nil when the torrent has no live stats (paused,
// initializing, errored) or when decode fails (schema drift is logged once).
func parseRqbitStats(raw json.RawMessage) *TorrentStats {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s rqbitStats
	if err := json.Unmarshal(raw, &s); err != nil {
		rqbitStatsDecodeErrOnce.Do(func() {
			fmt.Printf("rqbit: stats decode failed (schema drift?): %v\n", err)
		})
		return nil
	}
	if s.Live == nil {
		return nil
	}
	ps := s.Live.Snapshot.PeerStats
	// rqbit doesn't split seeders from leechers; leave ConnectedSeeders at 0.
	return &TorrentStats{
		DownloadSpeed:    s.Live.DownloadSpeed.bytesPerSec(),
		UploadSpeed:      s.Live.UploadSpeed.bytesPerSec(),
		ActivePeers:      ps.Live,
		TotalPeers:       ps.Seen,
		ConnectedSeeders: 0,
	}
}

// torrentDetailsToInfoSlice converts a slice of rqbitTorrentDetail to []TorrentInfo
func (r *RqbitAdapter) torrentDetailsToInfoSlice(details []rqbitTorrentDetail) []TorrentInfo {
	result := make([]TorrentInfo, 0, len(details))
	for _, d := range details {
		hash := strings.ToLower(d.InfoHash)

		files := make([]TorrentFile, 0, len(d.Files))
		for i, f := range d.Files {
			files = append(files, TorrentFile{
				Index: i,
				Path:  f.Name,
				Size:  f.Length,
			})
		}

		// Look up the engine ID from our mapping
		r.mu.RLock()
		id, exists := r.hashToID[hash]
		r.mu.RUnlock()

		engineID := ""
		if exists {
			engineID = strconv.Itoa(id)
		}

		var totalSize int64
		for _, f := range files {
			totalSize += f.Size
		}

		result = append(result, TorrentInfo{
			InfoHash:  hash,
			Name:      d.Name,
			Files:     files,
			EngineID:  engineID,
			TotalSize: totalSize,
			Stats:     parseRqbitStats(d.Stats),
		})
	}
	return result
}

// Compile-time interface check
var _ Engine = (*RqbitAdapter)(nil)
