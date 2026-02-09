package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber"

	"github.com/krizcold/stremio-torrent-bridge/internal/addon"
	"github.com/krizcold/stremio-torrent-bridge/internal/cache"
	"github.com/krizcold/stremio-torrent-bridge/internal/config"
	"github.com/krizcold/stremio-torrent-bridge/internal/engine"
	"github.com/krizcold/stremio-torrent-bridge/internal/relay"
)

// Handlers groups the HTTP handlers for the management REST API.
type Handlers struct {
	store        *addon.AddonStore
	config       *config.Config
	engine       engine.Engine
	cacheManager *cache.CacheManager // may be nil
	wrapper      *addon.Wrapper      // for health check (manifest cache status)
	relay        *relay.Server       // for health check (relay status)
}

// NewHandlers creates a new Handlers instance wired to the given dependencies.
func NewHandlers(store *addon.AddonStore, cfg *config.Config, eng engine.Engine, cm *cache.CacheManager, w *addon.Wrapper, rs *relay.Server) *Handlers {
	return &Handlers{
		store:        store,
		config:       cfg,
		engine:       eng,
		cacheManager: cm,
		wrapper:      w,
		relay:        rs,
	}
}

// --- request / response types ------------------------------------------------

type addAddonRequest struct {
	ManifestURL string `json:"manifestUrl"`
}

type addAddonResponse struct {
	ID          string `json:"id"`
	OriginalURL string `json:"originalUrl"`
	WrappedURL  string `json:"wrappedUrl"`
	Name        string `json:"name"`
}

type listAddonItem struct {
	ID          string    `json:"id"`
	OriginalURL string    `json:"originalUrl"`
	WrappedURL  string    `json:"wrappedUrl"`
	Name        string    `json:"name"`
	FetchMethod string    `json:"fetchMethod"`
	FetchStatus string    `json:"fetchStatus"`
	CreatedAt   time.Time `json:"createdAt"`
}

type engineStatus struct {
	URL    string `json:"url"`
	Status string `json:"status"`
}

type configResponse struct {
	DefaultEngine      string                   `json:"defaultEngine"`
	DefaultFetchMethod string                   `json:"defaultFetchMethod"`
	ProxyURL           string                   `json:"proxyURL"`
	CacheSizeGB        int                      `json:"cacheSizeGB"`
	CacheMaxAgeDays    int                      `json:"cacheMaxAgeDays"`
	Engines            map[string]*engineStatus `json:"engines"`
}

type updateConfigRequest struct {
	DefaultEngine      *string `json:"defaultEngine"`
	DefaultFetchMethod *string `json:"defaultFetchMethod"`
	ProxyURL           *string `json:"proxyURL"`
	CacheSizeGB        *int    `json:"cacheSizeGB"`
	CacheMaxAgeDays    *int    `json:"cacheMaxAgeDays"`
}

type updateAddonRequest struct {
	FetchMethod *string `json:"fetchMethod"`
}

// --- addon endpoints ---------------------------------------------------------

// HandleAddAddon handles POST /api/addons.
// It registers a new upstream Stremio addon and returns its wrapped URL.
func (h *Handlers) HandleAddAddon(c *fiber.Ctx) {
	var req addAddonRequest
	if err := json.Unmarshal([]byte(c.Body()), &req); err != nil {
		c.Status(http.StatusBadRequest)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"invalid JSON body"}`)
		return
	}

	if strings.TrimSpace(req.ManifestURL) == "" {
		c.Status(http.StatusBadRequest)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"manifestUrl is required"}`)
		return
	}

	wrapped, err := h.store.Add(req.ManifestURL)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"failed to add addon"}`)
		return
	}

	// Best-effort: fetch the manifest to populate the addon name immediately.
	if wrapped.Name == "" {
		go h.fetchAddonName(wrapped.ID, req.ManifestURL)
	}

	externalBase := resolveExternalURL(h.config, c)

	resp := addAddonResponse{
		ID:          wrapped.ID,
		OriginalURL: wrapped.OriginalURL,
		WrappedURL:  externalBase + "/wrap/" + wrapped.ID + "/manifest.json",
		Name:        wrapped.Name,
	}

	out, _ := json.Marshal(resp)
	c.Status(http.StatusCreated)
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// HandleListAddons handles GET /api/addons.
// It returns all registered addons with their wrapped URLs.
func (h *Handlers) HandleListAddons(c *fiber.Ctx) {
	addons := h.store.List()
	externalBase := resolveExternalURL(h.config, c)

	items := make([]listAddonItem, 0, len(addons))
	for _, a := range addons {
		items = append(items, listAddonItem{
			ID:          a.ID,
			OriginalURL: a.OriginalURL,
			WrappedURL:  externalBase + "/wrap/" + a.ID + "/manifest.json",
			Name:        a.Name,
			FetchMethod: a.FetchMethod,
			FetchStatus: a.FetchStatus,
			CreatedAt:   a.CreatedAt,
		})
	}

	out, _ := json.Marshal(items)
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// HandleRemoveAddon handles DELETE /api/addons/:id.
// It removes a registered addon by its ID.
func (h *Handlers) HandleRemoveAddon(c *fiber.Ctx) {
	id := c.Params("id")

	if err := h.store.Remove(id); err != nil {
		c.Status(http.StatusNotFound)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"addon not found"}`)
		return
	}

	c.Set("Content-Type", "application/json")
	c.SendString(`{"success":true}`)
}

// HandleUpdateAddon handles PATCH /api/addons/:id.
// It updates per-addon settings like fetch method.
func (h *Handlers) HandleUpdateAddon(c *fiber.Ctx) {
	id := c.Params("id")

	if _, found := h.store.Get(id); !found {
		c.Status(http.StatusNotFound)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"addon not found"}`)
		return
	}

	var req updateAddonRequest
	if err := json.Unmarshal([]byte(c.Body()), &req); err != nil {
		c.Status(http.StatusBadRequest)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"invalid JSON body"}`)
		return
	}

	if req.FetchMethod != nil {
		if !addon.ValidFetchMethods[*req.FetchMethod] {
			c.Status(http.StatusBadRequest)
			c.Set("Content-Type", "application/json")
			c.SendString(`{"error":"fetchMethod must be one of: global, sw_fallback, tab_relay, sw_only, direct, proxy"}`)
			return
		}
		if err := h.store.UpdateFetchMethod(id, *req.FetchMethod); err != nil {
			c.Status(http.StatusInternalServerError)
			c.Set("Content-Type", "application/json")
			c.SendString(`{"error":"failed to update fetch method"}`)
			return
		}
	}

	c.Set("Content-Type", "application/json")
	c.SendString(`{"success":true}`)
}

// --- config endpoints --------------------------------------------------------

// HandleGetConfig handles GET /api/config.
// It returns the current runtime configuration including engine health status.
func (h *Handlers) HandleGetConfig(c *fiber.Ctx) {
	engines := map[string]*engineStatus{
		"torrserver": {
			URL:    h.config.TorrServerURL,
			Status: "unknown",
		},
		"rqbit": {
			URL:    h.config.RqbitURL,
			Status: "unknown",
		},
		"qbittorrent": {
			URL:    h.config.QBittorrentURL,
			Status: "unknown",
		},
	}

	// Ping the active engine to get its live status.
	if es, ok := engines[h.engine.Name()]; ok {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		if err := h.engine.Ping(ctx); err != nil {
			es.Status = "offline"
		} else {
			es.Status = "online"
		}
	}

	resp := configResponse{
		DefaultEngine:      h.config.DefaultEngine,
		DefaultFetchMethod: h.config.DefaultFetchMethod,
		ProxyURL:           h.config.ProxyURL,
		CacheSizeGB:        h.config.CacheSizeGB,
		CacheMaxAgeDays:    h.config.CacheMaxAgeDays,
		Engines:            engines,
	}

	out, _ := json.Marshal(resp)
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// HandleUpdateConfig handles PUT /api/config.
// It applies partial runtime configuration updates (not persisted to disk).
func (h *Handlers) HandleUpdateConfig(c *fiber.Ctx) {
	var req updateConfigRequest
	if err := json.Unmarshal([]byte(c.Body()), &req); err != nil {
		c.Status(http.StatusBadRequest)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"invalid JSON body"}`)
		return
	}

	// Validate defaultEngine if provided.
	if req.DefaultEngine != nil {
		valid := map[string]bool{
			"torrserver":  true,
			"rqbit":       true,
			"qbittorrent": true,
		}
		if !valid[*req.DefaultEngine] {
			c.Status(http.StatusBadRequest)
			c.Set("Content-Type", "application/json")
			c.SendString(`{"error":"defaultEngine must be one of: torrserver, rqbit, qbittorrent"}`)
			return
		}
		h.config.DefaultEngine = *req.DefaultEngine
	}

	// Validate cacheSizeGB if provided.
	if req.CacheSizeGB != nil {
		if *req.CacheSizeGB <= 0 {
			c.Status(http.StatusBadRequest)
			c.Set("Content-Type", "application/json")
			c.SendString(`{"error":"cacheSizeGB must be positive"}`)
			return
		}
		h.config.CacheSizeGB = *req.CacheSizeGB
	}

	// Validate cacheMaxAgeDays if provided.
	if req.CacheMaxAgeDays != nil {
		if *req.CacheMaxAgeDays <= 0 {
			c.Status(http.StatusBadRequest)
			c.Set("Content-Type", "application/json")
			c.SendString(`{"error":"cacheMaxAgeDays must be positive"}`)
			return
		}
		h.config.CacheMaxAgeDays = *req.CacheMaxAgeDays
	}

	// Validate defaultFetchMethod if provided.
	if req.DefaultFetchMethod != nil {
		if !addon.ValidGlobalFetchMethods[*req.DefaultFetchMethod] {
			c.Status(http.StatusBadRequest)
			c.Set("Content-Type", "application/json")
			c.SendString(`{"error":"defaultFetchMethod must be one of: sw_fallback, tab_relay, sw_only, direct, proxy"}`)
			return
		}
		h.config.DefaultFetchMethod = *req.DefaultFetchMethod
	}

	// Update proxyURL if provided.
	if req.ProxyURL != nil {
		h.config.ProxyURL = *req.ProxyURL
	}

	// Return the updated config using the same format as GET /api/config,
	// but skip the engine ping for speed.
	resp := configResponse{
		DefaultEngine:      h.config.DefaultEngine,
		DefaultFetchMethod: h.config.DefaultFetchMethod,
		ProxyURL:           h.config.ProxyURL,
		CacheSizeGB:        h.config.CacheSizeGB,
		CacheMaxAgeDays:    h.config.CacheMaxAgeDays,
		Engines: map[string]*engineStatus{
			"torrserver": {
				URL:    h.config.TorrServerURL,
				Status: "unknown",
			},
			"rqbit": {
				URL:    h.config.RqbitURL,
				Status: "unknown",
			},
			"qbittorrent": {
				URL:    h.config.QBittorrentURL,
				Status: "unknown",
			},
		},
	}

	out, _ := json.Marshal(resp)
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// --- cache endpoints ---------------------------------------------------------

// HandleGetCacheStats handles GET /api/cache/stats.
func (h *Handlers) HandleGetCacheStats(c *fiber.Ctx) {
	if h.cacheManager == nil {
		c.Status(http.StatusServiceUnavailable)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"cache manager not available"}`)
		return
	}
	stats := h.cacheManager.GetStats()
	out, _ := json.Marshal(stats)
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// HandleCacheCleanup handles POST /api/cache/cleanup.
func (h *Handlers) HandleCacheCleanup(c *fiber.Ctx) {
	if h.cacheManager == nil {
		c.Status(http.StatusServiceUnavailable)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"cache manager not available"}`)
		return
	}
	removed, err := h.cacheManager.RunCleanup()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		c.Set("Content-Type", "application/json")
		errJSON, _ := json.Marshal(map[string]string{"error": err.Error()})
		c.Send(errJSON)
		return
	}
	out, _ := json.Marshal(map[string]interface{}{
		"removed": removed,
		"stats":   h.cacheManager.GetStats(),
	})
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// HandleRemoveTorrent handles DELETE /api/cache/torrents/:hash.
func (h *Handlers) HandleRemoveTorrent(c *fiber.Ctx) {
	hash := c.Params("hash")
	if hash == "" {
		c.Status(http.StatusBadRequest)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"missing hash parameter"}`)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.engine.RemoveTorrent(ctx, hash, true); err != nil {
		c.Status(http.StatusInternalServerError)
		c.Set("Content-Type", "application/json")
		errJSON, _ := json.Marshal(map[string]string{"error": err.Error()})
		c.Send(errJSON)
		return
	}
	c.Set("Content-Type", "application/json")
	c.SendString(`{"success":true}`)
}

// --- health check endpoints --------------------------------------------------

// addonHealthItem is the per-addon health status.
type addonHealthItem struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	OriginalURL     string `json:"originalUrl"`
	FetchMethod     string `json:"fetchMethod"`     // Per-addon setting (may be "global")
	EffectiveMethod string `json:"effectiveMethod"` // Resolved method
	DirectReachable bool   `json:"directReachable"` // Can the server fetch the manifest directly?
	DirectError     string `json:"directError,omitempty"`
	RelayConnected  bool   `json:"relayConnected"`
	ManifestCached  bool   `json:"manifestCached"`
	Status          string `json:"status"` // "ok", "degraded", "failing"
	Recommendation  string `json:"recommendation,omitempty"`
}

// HandleHealthCheck handles GET /api/health.
// Tests connectivity to each addon and returns diagnostic info.
func (h *Handlers) HandleHealthCheck(c *fiber.Ctx) {
	addons := h.store.List()

	relayConnected := false
	if h.relay != nil {
		relayConnected = h.relay.Connected()
	}

	items := make([]addonHealthItem, 0, len(addons))
	for _, a := range addons {
		effective := a.FetchMethod
		if effective == "" || effective == addon.FetchMethodGlobal {
			effective = h.config.DefaultFetchMethod
		}

		cached := false
		if h.wrapper != nil {
			cached = h.wrapper.HasCachedManifest(a.ID)
		}

		item := addonHealthItem{
			ID:              a.ID,
			Name:            a.Name,
			OriginalURL:     a.OriginalURL,
			FetchMethod:     a.FetchMethod,
			EffectiveMethod: effective,
			RelayConnected:  relayConnected,
			ManifestCached:  cached,
		}

		// Test direct fetch (with short timeout).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, a.OriginalURL, nil)
		if err == nil {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				item.DirectReachable = false
				item.DirectError = "connection failed"
			} else {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 400 {
					item.DirectReachable = true
				} else {
					item.DirectReachable = false
					item.DirectError = fmt.Sprintf("HTTP %d", resp.StatusCode)
				}
			}
		} else {
			item.DirectReachable = false
			item.DirectError = "invalid URL"
		}
		cancel()

		// Determine status and recommendation.
		switch {
		case item.DirectReachable:
			item.Status = "ok"
		case relayConnected || cached:
			item.Status = "degraded"
			if !item.DirectReachable && effective == "direct" {
				item.Recommendation = "Direct fetch is blocked. Switch to Tab Relay or SW + Fallback."
			} else if !relayConnected && effective == "tab_relay" {
				item.Recommendation = "Relay disconnected. Keep this tab open or switch to Direct."
			}
		default:
			item.Status = "failing"
			if effective == "direct" || effective == "sw_fallback" {
				item.Recommendation = "Addon is unreachable. Switch to Tab Relay and keep this tab open."
			} else if effective == "tab_relay" {
				item.Recommendation = "Relay disconnected. Keep this tab open while using Stremio."
			}
		}

		items = append(items, item)
	}

	out, _ := json.Marshal(items)
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// --- live torrent stats endpoints --------------------------------------------

// torrentStatsItem is a single torrent's live stats for the API response.
type torrentStatsItem struct {
	InfoHash         string  `json:"infoHash"`
	Name             string  `json:"name"`
	TotalSize        int64   `json:"totalSize"`
	DownloadSpeed    float64 `json:"downloadSpeed"`
	UploadSpeed      float64 `json:"uploadSpeed"`
	ActivePeers      int     `json:"activePeers"`
	TotalPeers       int     `json:"totalPeers"`
	ConnectedSeeders int     `json:"connectedSeeders"`
}

// HandleTorrentStats handles GET /api/torrents/stats.
// Returns live stats for all torrents from the engine (peers, speed, size).
func (h *Handlers) HandleTorrentStats(c *fiber.Ctx) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	torrents, err := h.engine.ListTorrents(ctx)
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		c.Set("Content-Type", "application/json")
		errJSON, _ := json.Marshal(map[string]string{"error": err.Error()})
		c.Send(errJSON)
		return
	}

	items := make([]torrentStatsItem, 0, len(torrents))
	for _, t := range torrents {
		item := torrentStatsItem{
			InfoHash:  t.InfoHash,
			Name:      t.Name,
			TotalSize: t.TotalSize,
		}
		if t.Stats != nil {
			item.DownloadSpeed = t.Stats.DownloadSpeed
			item.UploadSpeed = t.Stats.UploadSpeed
			item.ActivePeers = t.Stats.ActivePeers
			item.TotalPeers = t.Stats.TotalPeers
			item.ConnectedSeeders = t.Stats.ConnectedSeeders
		}
		items = append(items, item)
	}

	out, _ := json.Marshal(items)
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// --- service worker endpoints ------------------------------------------------

// swConfigResponse is the JSON returned to the Service Worker so it knows
// which addons are wrapped and how to reach the bridge.
type swConfigResponse struct {
	BridgeBaseURL      string         `json:"bridgeBaseURL"`
	DefaultFetchMethod string         `json:"defaultFetchMethod"`
	Addons             []swAddonEntry `json:"addons"`
}

type swAddonEntry struct {
	WrapID      string `json:"wrapId"`
	OriginalURL string `json:"originalUrl"`
	FetchMethod string `json:"fetchMethod"` // Resolved: never "global", always the effective method
}

// HandleSWConfig handles GET /sw/config.json.
// Returns configuration for the injected Service Worker.
func (h *Handlers) HandleSWConfig(c *fiber.Ctx) {
	addons := h.store.List()
	externalBase := resolveExternalURL(h.config, c)

	entries := make([]swAddonEntry, 0, len(addons))
	for _, a := range addons {
		// Resolve "global" to the actual default method.
		method := a.FetchMethod
		if method == "" || method == addon.FetchMethodGlobal {
			method = h.config.DefaultFetchMethod
		}
		entries = append(entries, swAddonEntry{
			WrapID:      a.ID,
			OriginalURL: a.OriginalURL,
			FetchMethod: method,
		})
	}

	resp := swConfigResponse{
		BridgeBaseURL:      externalBase,
		DefaultFetchMethod: h.config.DefaultFetchMethod,
		Addons:             entries,
	}

	out, _ := json.Marshal(resp)
	c.Set("Content-Type", "application/json")
	c.Set("Cache-Control", "no-cache")
	c.Set("Access-Control-Allow-Origin", "*")
	c.Send(out)
}

// fetchAddonName fetches a manifest URL and extracts the "name" field to
// update the addon store. Best-effort; failures are silently logged.
func (h *Handlers) fetchAddonName(addonID, manifestURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("handlers: fetch addon name from %s: %v\n", manifestURL, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return
	}

	if manifest.Name != "" {
		if err := h.store.UpdateName(addonID, manifest.Name); err != nil {
			fmt.Printf("handlers: update addon name for %s: %v\n", addonID, err)
		}
	}
}

// --- helpers -----------------------------------------------------------------

// resolveExternalURL determines the base URL that external clients should use
// to reach this bridge. It prefers the explicit ExternalURL from config and
// falls back to inferring from request headers.
func resolveExternalURL(cfg *config.Config, c *fiber.Ctx) string {
	if cfg.ExternalURL != "" {
		return strings.TrimRight(cfg.ExternalURL, "/")
	}

	scheme := c.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}

	return scheme + "://" + c.Hostname()
}
