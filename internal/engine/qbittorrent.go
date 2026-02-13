package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/krizcold/stremio-torrent-bridge/pkg/httpclient"
)

// QBittorrentAdapter implements Engine for qBittorrent via its Web API v2.
// Unlike TorrServer, qBittorrent downloads files to disk. The bridge reads
// those files from a shared Docker volume and serves them with Range support.
type QBittorrentAdapter struct {
	baseURL      string
	downloadPath string // Local path where qBittorrent downloads are mounted (e.g., "/downloads")
	username     string
	password     string
	client       *http.Client

	mu  sync.Mutex
	sid string // Session ID cookie from /api/v2/auth/login
}

// NewQBittorrentAdapter creates a new qBittorrent engine adapter.
// baseURL is the qBittorrent WebUI address (e.g., "http://qbittorrent:8080").
// downloadPath is the local mount point for qBittorrent's download directory.
func NewQBittorrentAdapter(baseURL, downloadPath, username, password string) *QBittorrentAdapter {
	return &QBittorrentAdapter{
		baseURL:      strings.TrimRight(baseURL, "/"),
		downloadPath: downloadPath,
		username:     username,
		password:     password,
		client:       httpclient.New(),
	}
}

// qBittorrent API response types

type qbitTorrentInfo struct {
	Hash          string  `json:"hash"`
	Name          string  `json:"name"`
	SavePath      string  `json:"save_path"`
	ContentPath   string  `json:"content_path"`
	Progress      float64 `json:"progress"`
	Size          int64   `json:"size"`
	PieceSize     int64   `json:"piece_size"`
	NumComplete   int     `json:"num_complete"`
	NumIncomplete int     `json:"num_incomplete"`
	NumSeeds      int     `json:"num_seeds"`
	NumLeechs     int     `json:"num_leechs"`
	DlSpeed       int64   `json:"dlspeed"`
	UpSpeed       int64   `json:"upspeed"`
}

type qbitFileInfo struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Priority int    `json:"priority"`
	Progress float64 `json:"progress"`
}

func (q *QBittorrentAdapter) Name() string {
	return "qbittorrent"
}

// login authenticates with the qBittorrent Web API and stores the session cookie.
func (q *QBittorrentAdapter) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", q.username)
	form.Set("password", q.password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		q.baseURL+"/api/v2/auth/login",
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("qbittorrent login: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := q.client.Do(req)
	if err != nil {
		return fmt.Errorf("qbittorrent login: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "Ok." {
		return fmt.Errorf("qbittorrent login: authentication failed (status %d): %s", resp.StatusCode, string(body))
	}

	// Extract SID cookie from response
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "SID" {
			q.mu.Lock()
			q.sid = cookie.Value
			q.mu.Unlock()
			return nil
		}
	}

	return fmt.Errorf("qbittorrent login: no SID cookie in response")
}

// doRequest executes an HTTP request with the SID session cookie attached.
// If the response is 403 (Forbidden), it re-authenticates and retries once.
func (q *QBittorrentAdapter) doRequest(ctx context.Context, method, path string, body string) (*http.Response, error) {
	makeReq := func() (*http.Request, error) {
		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, bodyReader)
		if err != nil {
			return nil, err
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}

		q.mu.Lock()
		sid := q.sid
		q.mu.Unlock()
		if sid != "" {
			req.AddCookie(&http.Cookie{Name: "SID", Value: sid})
		}
		return req, nil
	}

	req, err := makeReq()
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: create request: %w", err)
	}

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: request failed: %w", err)
	}

	// If forbidden, try logging in and retry once
	if resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		if loginErr := q.login(ctx); loginErr != nil {
			return nil, fmt.Errorf("qbittorrent: re-login failed: %w", loginErr)
		}
		req, err = makeReq()
		if err != nil {
			return nil, fmt.Errorf("qbittorrent: create retry request: %w", err)
		}
		resp, err = q.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("qbittorrent: retry request failed: %w", err)
		}
	}

	return resp, nil
}

func (q *QBittorrentAdapter) AddTorrent(ctx context.Context, magnetURI string) (*TorrentInfo, error) {
	infoHash := ParseInfoHashFromMagnet(magnetURI)
	if infoHash == "" {
		return nil, fmt.Errorf("qbittorrent add torrent: could not parse info hash from magnet URI")
	}

	// Check if the torrent already exists
	existing, err := q.GetTorrent(ctx, infoHash)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent add torrent: check existing: %w", err)
	}
	if existing != nil {
		return existing, nil
	}

	// Add the torrent with sequential download and first/last piece priority
	form := url.Values{}
	form.Set("urls", magnetURI)
	form.Set("sequentialDownload", "true")
	form.Set("firstLastPiecePrio", "true")
	form.Set("savepath", q.downloadPath)

	resp, err := q.doRequest(ctx, http.MethodPost, "/api/v2/torrents/add", form.Encode())
	if err != nil {
		return nil, fmt.Errorf("qbittorrent add torrent: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(respBody)) == "Fails." {
		return nil, fmt.Errorf("qbittorrent add torrent: failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Poll until the torrent is registered and has metadata (name + files).
	// qBittorrent may take a moment to fetch metadata from peers.
	var info *TorrentInfo
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		info, err = q.GetTorrent(ctx, infoHash)
		if err != nil {
			return nil, fmt.Errorf("qbittorrent add torrent: get info: %w", err)
		}
		if info != nil && info.Name != "" && len(info.Files) > 0 {
			return info, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}

	// Return whatever we have, even if metadata is incomplete
	if info != nil {
		return info, nil
	}

	return nil, fmt.Errorf("qbittorrent add torrent: timeout waiting for torrent metadata")
}

func (q *QBittorrentAdapter) StreamFile(ctx context.Context, infoHash string, fileIndex int, req *http.Request) (*StreamResponse, error) {
	hash := strings.ToLower(infoHash)

	// Wait for the torrent to appear and have metadata resolved.
	// The wrapper adds torrents asynchronously (fire-and-forget goroutine),
	// so the torrent may not exist yet when Stremio requests the stream.
	var torrents []qbitTorrentInfo
	var files []qbitFileInfo
	deadline := time.Now().Add(90 * time.Second)
	for {
		var err error
		torrents, err = q.getTorrentInfo(ctx, hash)
		if err == nil && len(torrents) > 0 {
			files, err = q.getFiles(ctx, hash)
			if err == nil && fileIndex >= 0 && fileIndex < len(files) {
				break
			}
		}
		if time.Now().After(deadline) {
			if len(torrents) == 0 {
				return nil, fmt.Errorf("qbittorrent stream: torrent not found: %s", hash)
			}
			return nil, fmt.Errorf("qbittorrent stream: file index %d out of range (have %d files)", fileIndex, len(files))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}

	targetFile := files[fileIndex]

	// Focus all bandwidth on the target file: set it to max priority,
	// set all other files to "do not download". This ensures qBittorrent
	// downloads pieces for the streaming file first instead of sequentially
	// from the start of the entire torrent.
	q.focusFile(ctx, hash, fileIndex, len(files))

	// Remove all other torrents to free bandwidth and disk resources.
	// Run in background to not delay the stream start.
	go q.removeOtherTorrents(ctx, hash)

	// Construct the local file path. qBittorrent saves files at:
	//   save_path/torrent_name/file_name  (for multi-file torrents)
	//   save_path/file_name               (for single-file torrents)
	// The file's "name" field from the API already includes the relative path
	// within the torrent, potentially including the torrent name as a prefix.
	filePath := filepath.Join(q.downloadPath, targetFile.Name)

	// Skip piece waiting if the torrent is already fully downloaded
	if torrents[0].Progress < 1.0 {
		if err := q.waitForPieces(ctx, hash, fileIndex, 60*time.Second); err != nil {
			return nil, fmt.Errorf("qbittorrent stream: %w", err)
		}
	}

	// Open the file from the shared volume
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent stream: open file: %w", err)
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("qbittorrent stream: stat file: %w", err)
	}

	totalSize := stat.Size()
	contentType := detectContentType(targetFile.Name)

	// Handle Range header for partial content support
	rangeHeader := req.Header.Get("Range")
	if rangeHeader == "" {
		// No Range header: serve the whole file
		return &StreamResponse{
			Body:          f,
			ContentLength: totalSize,
			ContentType:   contentType,
			StatusCode:    http.StatusOK,
			Header: http.Header{
				"Accept-Ranges":  {"bytes"},
				"Content-Length": {strconv.FormatInt(totalSize, 10)},
			},
		}, nil
	}

	// Parse Range header (supports "bytes=START-END" and "bytes=START-")
	start, end, err := parseRangeHeader(rangeHeader, totalSize)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("qbittorrent stream: %w", err)
	}

	contentLength := end - start + 1

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("qbittorrent stream: seek: %w", err)
	}

	return &StreamResponse{
		Body:          &limitedReadCloser{Reader: io.LimitReader(f, contentLength), Closer: f},
		ContentLength: contentLength,
		ContentType:   contentType,
		StatusCode:    http.StatusPartialContent,
		Header: http.Header{
			"Accept-Ranges":  {"bytes"},
			"Content-Range":  {fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize)},
			"Content-Length": {strconv.FormatInt(contentLength, 10)},
		},
	}, nil
}

func (q *QBittorrentAdapter) RemoveTorrent(ctx context.Context, infoHash string, deleteFiles bool) error {
	hash := strings.ToLower(infoHash)

	form := url.Values{}
	form.Set("hashes", hash)
	form.Set("deleteFiles", strconv.FormatBool(deleteFiles))

	resp, err := q.doRequest(ctx, http.MethodPost, "/api/v2/torrents/delete", form.Encode())
	if err != nil {
		return fmt.Errorf("qbittorrent remove torrent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qbittorrent remove torrent: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (q *QBittorrentAdapter) GetTorrent(ctx context.Context, infoHash string) (*TorrentInfo, error) {
	hash := strings.ToLower(infoHash)

	torrents, err := q.getTorrentInfo(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent get torrent: %w", err)
	}
	if len(torrents) == 0 {
		return nil, nil
	}

	files, err := q.getFiles(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent get torrent: get files: %w", err)
	}

	return torrentInfoFromQBittorrent(&torrents[0], files), nil
}

func (q *QBittorrentAdapter) ListTorrents(ctx context.Context) ([]TorrentInfo, error) {
	// Get all torrents (no hash filter)
	resp, err := q.doRequest(ctx, http.MethodGet, "/api/v2/torrents/info", "")
	if err != nil {
		return nil, fmt.Errorf("qbittorrent list torrents: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent list torrents: read response: %w", err)
	}

	var torrents []qbitTorrentInfo
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, fmt.Errorf("qbittorrent list torrents: parse response: %w", err)
	}

	result := make([]TorrentInfo, 0, len(torrents))
	for i := range torrents {
		// Get files for each torrent
		files, err := q.getFiles(ctx, torrents[i].Hash)
		if err != nil {
			// If we cannot get files for a torrent, include it with empty file list
			files = nil
		}
		result = append(result, *torrentInfoFromQBittorrent(&torrents[i], files))
	}

	return result, nil
}

func (q *QBittorrentAdapter) Ping(ctx context.Context) error {
	resp, err := q.doRequest(ctx, http.MethodGet, "/api/v2/app/version", "")
	if err != nil {
		return fmt.Errorf("qbittorrent ping: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qbittorrent ping: unexpected status %d", resp.StatusCode)
	}

	return nil
}

// getTorrentInfo fetches torrent metadata from the qBittorrent API.
// If hash is empty, returns all torrents.
func (q *QBittorrentAdapter) getTorrentInfo(ctx context.Context, hash string) ([]qbitTorrentInfo, error) {
	path := "/api/v2/torrents/info"
	if hash != "" {
		path += "?hashes=" + hash
	}

	resp, err := q.doRequest(ctx, http.MethodGet, path, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var torrents []qbitTorrentInfo
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return torrents, nil
}

// getFiles fetches the file list for a specific torrent.
func (q *QBittorrentAdapter) getFiles(ctx context.Context, hash string) ([]qbitFileInfo, error) {
	resp, err := q.doRequest(ctx, http.MethodGet, "/api/v2/torrents/files?hash="+hash, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read files response: %w", err)
	}

	var files []qbitFileInfo
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, fmt.Errorf("parse files response: %w", err)
	}

	return files, nil
}

// waitForPieces polls the piece states until the initial pieces for the target
// file are downloaded, enabling streaming to begin. Returns an error if the
// timeout elapses before pieces are ready.
func (q *QBittorrentAdapter) waitForPieces(ctx context.Context, hash string, fileIndex int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	// We need at least the first few pieces of the file to start streaming.
	// The number of required pieces depends on piece size, but 5 is a
	// reasonable minimum for most video formats.
	const minPieces = 5

	for time.Now().Before(deadline) {
		// Get piece size from /api/v2/torrents/properties (not available in /info)
		propResp, err := q.doRequest(ctx, http.MethodGet, "/api/v2/torrents/properties?hash="+hash, "")
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		propData, err := io.ReadAll(propResp.Body)
		propResp.Body.Close()
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		var props struct {
			PieceSize int64 `json:"piece_size"`
		}
		if err := json.Unmarshal(propData, &props); err != nil || props.PieceSize == 0 {
			// Metadata not yet available, wait and retry
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		pieceSize := props.PieceSize

		// Get the file list to determine which pieces belong to our file
		files, err := q.getFiles(ctx, hash)
		if err != nil || fileIndex >= len(files) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		// Calculate the byte offset of the target file within the torrent.
		// Files are stored sequentially, so sum sizes of preceding files.
		var fileOffset int64
		for i := 0; i < fileIndex; i++ {
			fileOffset += files[i].Size
		}

		// Determine the starting piece index for this file
		startPiece := int(fileOffset / pieceSize)

		// How many pieces do we need? At most minPieces, but no more than
		// the total number of pieces for this file.
		filePieces := int((files[fileIndex].Size + pieceSize - 1) / pieceSize)
		needed := minPieces
		if filePieces < needed {
			needed = filePieces
		}

		// Get piece states
		resp, err := q.doRequest(ctx, http.MethodGet, "/api/v2/torrents/pieceStates?hash="+hash, "")
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		var pieceStates []int
		if err := json.Unmarshal(data, &pieceStates); err != nil || len(pieceStates) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		// Check if the first N pieces of the file are downloaded (state == 2)
		ready := true
		for i := 0; i < needed; i++ {
			pieceIdx := startPiece + i
			if pieceIdx >= len(pieceStates) || pieceStates[pieceIdx] != 2 {
				ready = false
				break
			}
		}

		if ready {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	return fmt.Errorf("timeout waiting for initial pieces to download (hash=%s, fileIndex=%d)", hash, fileIndex)
}

// focusFile sets the target file to maximum download priority and all other
// files to "do not download". This ensures qBittorrent only downloads pieces
// belonging to the streaming file, rather than sequentially from piece 0.
func (q *QBittorrentAdapter) focusFile(ctx context.Context, hash string, targetIndex int, totalFiles int) {
	// Set non-target files to "do not download" (priority 0)
	skipIDs := make([]string, 0, totalFiles-1)
	for i := 0; i < totalFiles; i++ {
		if i != targetIndex {
			skipIDs = append(skipIDs, strconv.Itoa(i))
		}
	}
	if len(skipIDs) > 0 {
		form := url.Values{}
		form.Set("hash", hash)
		form.Set("id", strings.Join(skipIDs, "|"))
		form.Set("priority", "0")
		resp, err := q.doRequest(ctx, http.MethodPost, "/api/v2/torrents/filePrio", form.Encode())
		if err == nil {
			resp.Body.Close()
		}
	}

	// Set target file to maximum priority (7)
	form := url.Values{}
	form.Set("hash", hash)
	form.Set("id", strconv.Itoa(targetIndex))
	form.Set("priority", "7")
	resp, err := q.doRequest(ctx, http.MethodPost, "/api/v2/torrents/filePrio", form.Encode())
	if err == nil {
		resp.Body.Close()
	}
}

// removeOtherTorrents deletes all torrents except the one being streamed,
// freeing bandwidth and disk space for the active stream.
func (q *QBittorrentAdapter) removeOtherTorrents(ctx context.Context, keepHash string) {
	torrents, err := q.getTorrentInfo(ctx, "")
	if err != nil {
		return
	}
	for _, t := range torrents {
		if strings.ToLower(t.Hash) != keepHash {
			_ = q.RemoveTorrent(ctx, t.Hash, true)
		}
	}
}

// torrentInfoFromQBittorrent converts qBittorrent API responses to our TorrentInfo type.
func torrentInfoFromQBittorrent(t *qbitTorrentInfo, files []qbitFileInfo) *TorrentInfo {
	torrentFiles := make([]TorrentFile, 0, len(files))
	for _, f := range files {
		torrentFiles = append(torrentFiles, TorrentFile{
			Index: f.Index,
			Path:  f.Name,
			Size:  f.Size,
		})
	}

	totalSize := t.Size
	if totalSize == 0 {
		for _, f := range files {
			totalSize += f.Size
		}
	}

	info := &TorrentInfo{
		InfoHash:  strings.ToLower(t.Hash),
		Name:      t.Name,
		Files:     torrentFiles,
		EngineID:  strings.ToLower(t.Hash),
		TotalSize: totalSize,
	}

	if t.NumSeeds > 0 || t.NumLeechs > 0 || t.DlSpeed > 0 || t.NumComplete > 0 {
		info.Stats = &TorrentStats{
			DownloadSpeed:    float64(t.DlSpeed),
			UploadSpeed:      float64(t.UpSpeed),
			ActivePeers:      t.NumSeeds + t.NumLeechs,
			TotalPeers:       t.NumComplete + t.NumIncomplete,
			ConnectedSeeders: t.NumSeeds,
		}
	}

	return info
}

// parseRangeHeader parses an HTTP Range header value like "bytes=0-499" or
// "bytes=500-" and returns the inclusive start and end byte positions.
func parseRangeHeader(rangeHeader string, totalSize int64) (start, end int64, err error) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, fmt.Errorf("unsupported range format: %s", rangeHeader)
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")

	// Handle multiple ranges by only using the first one
	if idx := strings.Index(rangeSpec, ","); idx != -1 {
		rangeSpec = rangeSpec[:idx]
	}

	parts := strings.SplitN(rangeSpec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range format: %s", rangeHeader)
	}

	startStr := strings.TrimSpace(parts[0])
	endStr := strings.TrimSpace(parts[1])

	if startStr == "" {
		// Suffix range: "-500" means last 500 bytes
		suffixLen, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range suffix: %s", rangeHeader)
		}
		start = totalSize - suffixLen
		if start < 0 {
			start = 0
		}
		end = totalSize - 1
	} else {
		start, err = strconv.ParseInt(startStr, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range start: %s", rangeHeader)
		}
		if endStr == "" {
			// Open-ended range: "500-" means from byte 500 to end
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(endStr, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid range end: %s", rangeHeader)
			}
		}
	}

	if start > end || start >= totalSize {
		return 0, 0, fmt.Errorf("range not satisfiable: %s (file size: %d)", rangeHeader, totalSize)
	}
	if end >= totalSize {
		end = totalSize - 1
	}

	return start, end, nil
}

// detectContentType returns a MIME type based on the file extension.
func detectContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".ts":
		return "video/mp2t"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".flv":
		return "video/x-flv"
	case ".m4v":
		return "video/mp4"
	case ".srt":
		return "text/plain"
	case ".sub":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// limitedReadCloser combines a LimitReader with the underlying file's Close method.
// This ensures we only read the requested byte range while still closing the file.
type limitedReadCloser struct {
	io.Reader
	io.Closer
}
