package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber"

	"github.com/krizcold/stremio-torrent-bridge/internal/addon"
	"github.com/krizcold/stremio-torrent-bridge/internal/cache"
	"github.com/krizcold/stremio-torrent-bridge/internal/config"
	"github.com/krizcold/stremio-torrent-bridge/internal/engine"
)

// Handlers groups the HTTP handlers for the management REST API.
type Handlers struct {
	store        *addon.AddonStore
	config       *config.Config
	engine       engine.Engine
	cacheManager *cache.CacheManager // may be nil
}

// NewHandlers creates a new Handlers instance wired to the given dependencies.
func NewHandlers(store *addon.AddonStore, cfg *config.Config, eng engine.Engine, cm *cache.CacheManager) *Handlers {
	return &Handlers{
		store:        store,
		config:       cfg,
		engine:       eng,
		cacheManager: cm,
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
	CreatedAt   time.Time `json:"createdAt"`
}

type engineStatus struct {
	URL    string `json:"url"`
	Status string `json:"status"`
}

type configResponse struct {
	DefaultEngine   string                   `json:"defaultEngine"`
	CacheSizeGB     int                      `json:"cacheSizeGB"`
	CacheMaxAgeDays int                      `json:"cacheMaxAgeDays"`
	Engines         map[string]*engineStatus `json:"engines"`
}

type updateConfigRequest struct {
	DefaultEngine   *string `json:"defaultEngine"`
	CacheSizeGB     *int    `json:"cacheSizeGB"`
	CacheMaxAgeDays *int    `json:"cacheMaxAgeDays"`
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
		DefaultEngine:   h.config.DefaultEngine,
		CacheSizeGB:     h.config.CacheSizeGB,
		CacheMaxAgeDays: h.config.CacheMaxAgeDays,
		Engines:         engines,
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

	// Return the updated config using the same format as GET /api/config,
	// but skip the engine ping for speed.
	resp := configResponse{
		DefaultEngine:   h.config.DefaultEngine,
		CacheSizeGB:     h.config.CacheSizeGB,
		CacheMaxAgeDays: h.config.CacheMaxAgeDays,
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
