package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/yundera/stremio-torrent-bridge/pkg/httpclient"
)

// TorrServerAdapter implements Engine for TorrServer (github.com/YouROK/TorrServer)
type TorrServerAdapter struct {
	baseURL      string
	client       *http.Client // For API calls (30s timeout)
	streamClient *http.Client // For streaming (no timeout)
}

// NewTorrServerAdapter creates a new TorrServer engine adapter
func NewTorrServerAdapter(baseURL string) *TorrServerAdapter {
	return &TorrServerAdapter{
		baseURL:      strings.TrimRight(baseURL, "/"),
		client:       httpclient.New(),
		streamClient: httpclient.NewStreaming(),
	}
}

// torrServerRequest is the generic request body for TorrServer /torrents endpoint
type torrServerRequest struct {
	Action string `json:"action"`
	Link   string `json:"link,omitempty"`
	Hash   string `json:"hash,omitempty"`
}

// torrServerTorrent represents a torrent in TorrServer's API response
type torrServerTorrent struct {
	Hash     string                  `json:"hash"`
	Name     string                  `json:"name"`
	FileStat []torrServerFileStat    `json:"file_stat"`
}

// torrServerFileStat represents a file entry in TorrServer's response
type torrServerFileStat struct {
	ID     int    `json:"id"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
}

func (t *TorrServerAdapter) Name() string {
	return "torrserver"
}

func (t *TorrServerAdapter) AddTorrent(ctx context.Context, magnetURI string) (*TorrentInfo, error) {
	reqBody := torrServerRequest{
		Action: "add",
		Link:   magnetURI,
	}

	body, err := t.doTorrentsRequest(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("torrserver add torrent: %w", err)
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("torrserver add torrent: read response: %w", err)
	}

	var ts torrServerTorrent
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("torrserver add torrent: parse response: %w", err)
	}

	return torrentInfoFromTorrServer(&ts), nil
}

func (t *TorrServerAdapter) StreamFile(ctx context.Context, infoHash string, fileIndex int, req *http.Request) (*StreamResponse, error) {
	streamURL := fmt.Sprintf("%s/stream?link=%s&index=%d&play", t.baseURL, strings.ToLower(infoHash), fileIndex)

	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("torrserver stream: create request: %w", err)
	}

	// Forward Range-related headers from the original request
	forwardHeaders := []string{"Range", "If-Range", "If-None-Match"}
	for _, h := range forwardHeaders {
		if v := req.Header.Get(h); v != "" {
			streamReq.Header.Set(h, v)
		}
	}

	resp, err := t.streamClient.Do(streamReq)
	if err != nil {
		return nil, fmt.Errorf("torrserver stream: request failed: %w", err)
	}

	return &StreamResponse{
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		ContentType:   resp.Header.Get("Content-Type"),
		StatusCode:    resp.StatusCode,
		Header:        resp.Header,
	}, nil
}

func (t *TorrServerAdapter) RemoveTorrent(ctx context.Context, infoHash string, deleteFiles bool) error {
	// TorrServer always removes files; deleteFiles param is ignored
	reqBody := torrServerRequest{
		Action: "rem",
		Hash:   strings.ToLower(infoHash),
	}

	body, err := t.doTorrentsRequest(ctx, reqBody)
	if err != nil {
		return fmt.Errorf("torrserver remove torrent: %w", err)
	}
	body.Close()

	return nil
}

func (t *TorrServerAdapter) GetTorrent(ctx context.Context, infoHash string) (*TorrentInfo, error) {
	reqBody := torrServerRequest{
		Action: "get",
		Hash:   strings.ToLower(infoHash),
	}

	body, err := t.doTorrentsRequest(ctx, reqBody)
	if err != nil {
		// TorrServer returns an error for unknown hashes; treat as not found
		return nil, nil
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("torrserver get torrent: read response: %w", err)
	}

	var ts torrServerTorrent
	if err := json.Unmarshal(data, &ts); err != nil {
		// Parse failure likely means torrent not found (empty/error response)
		return nil, nil
	}

	// If hash is empty, the torrent was not found
	if ts.Hash == "" {
		return nil, nil
	}

	return torrentInfoFromTorrServer(&ts), nil
}

func (t *TorrServerAdapter) ListTorrents(ctx context.Context) ([]TorrentInfo, error) {
	reqBody := torrServerRequest{
		Action: "list",
	}

	body, err := t.doTorrentsRequest(ctx, reqBody)
	if err != nil {
		return nil, fmt.Errorf("torrserver list torrents: %w", err)
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("torrserver list torrents: read response: %w", err)
	}

	var torrents []torrServerTorrent
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, fmt.Errorf("torrserver list torrents: parse response: %w", err)
	}

	result := make([]TorrentInfo, 0, len(torrents))
	for i := range torrents {
		result = append(result, *torrentInfoFromTorrServer(&torrents[i]))
	}

	return result, nil
}

func (t *TorrServerAdapter) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/echo", nil)
	if err != nil {
		return fmt.Errorf("torrserver ping: create request: %w", err)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("torrserver ping: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("torrserver ping: unexpected status %d", resp.StatusCode)
	}

	return nil
}

// doTorrentsRequest sends a POST to the /torrents endpoint with the given request body.
// Returns the response body (caller must close) or an error.
func (t *TorrServerAdapter) doTorrentsRequest(ctx context.Context, reqBody torrServerRequest) (io.ReadCloser, error) {
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/torrents", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(errBody))
	}

	return resp.Body, nil
}

// torrentInfoFromTorrServer converts a TorrServer response to our TorrentInfo type
func torrentInfoFromTorrServer(ts *torrServerTorrent) *TorrentInfo {
	files := make([]TorrentFile, 0, len(ts.FileStat))
	for _, f := range ts.FileStat {
		files = append(files, TorrentFile{
			Index: f.ID,
			Path:  f.Path,
			Size:  f.Length,
		})
	}

	return &TorrentInfo{
		InfoHash: strings.ToLower(ts.Hash),
		Name:     ts.Name,
		Files:    files,
		EngineID: strings.ToLower(ts.Hash),
	}
}
