package addon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gofiber/fiber"

	"github.com/krizcold/stremio-torrent-bridge/internal/engine"
	"github.com/krizcold/stremio-torrent-bridge/pkg/httpclient"
)

// Wrapper intercepts Stremio addon requests, rewrites manifests to brand them
// as bridge addons, and replaces torrent infoHash streams with direct HTTP
// stream URLs served by the local torrent engine.
type Wrapper struct {
	store       *AddonStore
	engine      engine.Engine
	externalURL string // BRIDGE_EXTERNAL_URL or empty (falls back to Host header)
	httpClient  *http.Client
}

// NewWrapper creates a Wrapper that proxies and rewrites Stremio addon responses.
func NewWrapper(store *AddonStore, eng engine.Engine, externalURL string) *Wrapper {
	return &Wrapper{
		store:       store,
		engine:      eng,
		externalURL: strings.TrimRight(externalURL, "/"),
		httpClient:  httpclient.New(),
	}
}

// HandleManifest fetches the original addon manifest, rebrands it for the
// bridge, and strips behaviorHints so Stremio doesn't prompt for configuration.
//
// Route: GET /wrap/:wrapId/manifest.json
func (w *Wrapper) HandleManifest(c *fiber.Ctx) {
	wrapID := param(c, "wrapId")

	addon, found := w.store.Get(wrapID)
	if !found {
		c.Status(http.StatusNotFound)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"addon not found"}`)
		return
	}

	// Fetch the original manifest from the upstream addon.
	data, err := w.fetchJSON(addon.OriginalURL)
	if err != nil {
		fmt.Printf("wrapper: fetch manifest from %s: %v\n", addon.OriginalURL, err)
		c.Status(http.StatusBadGateway)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"failed to fetch original manifest"}`)
		return
	}

	// Parse into a generic map so we can modify arbitrary fields without
	// needing to know the full Stremio manifest schema.
	var manifest map[string]interface{}
	if err := json.Unmarshal(data, &manifest); err != nil {
		fmt.Printf("wrapper: parse manifest from %s: %v\n", addon.OriginalURL, err)
		c.Status(http.StatusBadGateway)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"failed to parse original manifest"}`)
		return
	}

	// Capture original name before we modify anything.
	originalName := ""
	if name, ok := manifest["name"].(string); ok {
		originalName = name
	}

	// Rebrand: prefix the ID and name so it's clear this goes through the bridge.
	if origID, ok := manifest["id"].(string); ok {
		manifest["id"] = "com.yundera.bridge." + origID
	}
	if name, ok := manifest["name"].(string); ok {
		manifest["name"] = "[Bridge] " + name
	}

	// Remove behaviorHints to prevent Stremio from showing config prompts.
	delete(manifest, "behaviorHints")

	// If the store doesn't have a name yet, persist the original name.
	if addon.Name == "" && originalName != "" {
		if err := w.store.UpdateName(wrapID, originalName); err != nil {
			fmt.Printf("wrapper: update addon name for %s: %v\n", wrapID, err)
		}
	}

	out, err := json.Marshal(manifest)
	if err != nil {
		fmt.Printf("wrapper: marshal modified manifest: %v\n", err)
		c.Status(http.StatusInternalServerError)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"failed to encode manifest"}`)
		return
	}

	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// HandleCatalog proxies a catalog request to the original addon.
//
// Route: GET /wrap/:wrapId/catalog/:type/:catalogId.json
func (w *Wrapper) HandleCatalog(c *fiber.Ctx) {
	wrapID := param(c, "wrapId")
	contentType := param(c, "type")
	catalogID := param(c, "catalogId")

	addon, found := w.store.Get(wrapID)
	if !found {
		c.Status(http.StatusNotFound)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"addon not found"}`)
		return
	}

	originalURL := getBaseURL(addon.OriginalURL) + "/catalog/" + contentType + "/" + catalogID + ".json"

	data, err := w.fetchJSON(originalURL)
	if err != nil {
		fmt.Printf("wrapper: fetch catalog from %s: %v\n", originalURL, err)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"metas":[]}`)
		return
	}

	c.Set("Content-Type", "application/json")
	c.Send(data)
}

// HandleMeta proxies a meta request to the original addon.
//
// Route: GET /wrap/:wrapId/meta/:type/:metaId.json
func (w *Wrapper) HandleMeta(c *fiber.Ctx) {
	wrapID := param(c, "wrapId")
	contentType := param(c, "type")
	metaID := param(c, "metaId")

	addon, found := w.store.Get(wrapID)
	if !found {
		c.Status(http.StatusNotFound)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"addon not found"}`)
		return
	}

	originalURL := getBaseURL(addon.OriginalURL) + "/meta/" + contentType + "/" + metaID + ".json"

	data, err := w.fetchJSON(originalURL)
	if err != nil {
		fmt.Printf("wrapper: fetch meta from %s: %v\n", originalURL, err)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"meta":{}}`)
		return
	}

	c.Set("Content-Type", "application/json")
	c.Send(data)
}

// HandleStream fetches stream results from the original addon, registers any
// torrents with the local engine, and rewrites infoHash-based streams to point
// at our direct HTTP stream proxy endpoint.
//
// Route: GET /wrap/:wrapId/stream/:type/:streamId.json
func (w *Wrapper) HandleStream(c *fiber.Ctx) {
	wrapID := param(c, "wrapId")
	contentType := param(c, "type")
	streamID := param(c, "streamId")

	addon, found := w.store.Get(wrapID)
	if !found {
		c.Status(http.StatusNotFound)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"addon not found"}`)
		return
	}

	originalURL := getBaseURL(addon.OriginalURL) + "/stream/" + contentType + "/" + streamID + ".json"

	data, err := w.fetchJSON(originalURL)
	if err != nil {
		fmt.Printf("wrapper: fetch streams from %s: %v\n", originalURL, err)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"streams":[]}`)
		return
	}

	// Parse the upstream response as generic JSON.
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Printf("wrapper: parse stream response from %s: %v\n", originalURL, err)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"streams":[]}`)
		return
	}

	streamsRaw, ok := resp["streams"]
	if !ok {
		c.Set("Content-Type", "application/json")
		c.SendString(`{"streams":[]}`)
		return
	}

	streams, ok := streamsRaw.([]interface{})
	if !ok {
		c.Set("Content-Type", "application/json")
		c.SendString(`{"streams":[]}`)
		return
	}

	externalBase := w.resolveExternalURL(c)

	for i, raw := range streams {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		infoHash, ok := item["infoHash"].(string)
		if !ok || infoHash == "" {
			continue
		}

		// Build a magnet URI from the infoHash and any tracker URLs.
		magnetURI := "magnet:?xt=urn:btih:" + infoHash
		if sources, ok := item["sources"].([]interface{}); ok {
			for _, s := range sources {
				if tracker, ok := s.(string); ok {
					magnetURI += "&tr=" + url.QueryEscape(tracker)
				}
			}
		}

		// Fire-and-forget: register the torrent with the engine so it starts
		// downloading metadata/pieces. If this fails the stream URL still
		// works -- the engine will add the torrent lazily on first request.
		go func(magnet string) {
			if _, err := w.engine.AddTorrent(context.Background(), magnet); err != nil {
				fmt.Printf("wrapper: background add torrent: %v\n", err)
			}
		}(magnetURI)

		// Determine the file index within the torrent.
		fileIdx := 0
		if fi, ok := item["fileIdx"].(float64); ok {
			fileIdx = int(fi)
		}

		// Replace the infoHash stream with a direct HTTP URL to our proxy.
		delete(item, "infoHash")
		delete(item, "fileIdx")
		delete(item, "sources")
		item["url"] = fmt.Sprintf("%s/stream/%s/%d", externalBase, strings.ToLower(infoHash), fileIdx)

		// Tag the title so users know this stream goes through the bridge.
		if title, ok := item["title"].(string); ok {
			item["title"] = title + " [Bridge]"
		}

		streams[i] = item
	}

	resp["streams"] = streams

	out, err := json.Marshal(resp)
	if err != nil {
		fmt.Printf("wrapper: marshal modified streams: %v\n", err)
		c.Status(http.StatusInternalServerError)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"failed to encode streams"}`)
		return
	}

	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// getBaseURL strips the /manifest.json suffix (and any query string) from a
// manifest URL to derive the addon's base URL.
func getBaseURL(originalManifestURL string) string {
	// Strip query parameters first so "/manifest.json?foo=bar" is handled.
	base := originalManifestURL
	if idx := strings.Index(base, "?"); idx != -1 {
		base = base[:idx]
	}
	base = strings.TrimSuffix(base, "/manifest.json")
	base = strings.TrimRight(base, "/")
	return base
}

// resolveExternalURL returns the base URL that external clients (Stremio) should
// use to reach this bridge. Prefers the explicit BRIDGE_EXTERNAL_URL config, and
// falls back to inferring from request headers.
func (w *Wrapper) resolveExternalURL(c *fiber.Ctx) string {
	if w.externalURL != "" {
		return w.externalURL
	}

	scheme := c.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}

	return scheme + "://" + c.Hostname()
}

// param reads a named value from Fiber context, checking Locals first (set by
// middleware routing) then falling back to Params (set by Fiber route params).
func param(c *fiber.Ctx, key string) string {
	if v, ok := c.Locals(key).(string); ok && v != "" {
		return v
	}
	return c.Params(key)
}

// fetchJSON performs a GET request and returns the response body as bytes.
func (w *Wrapper) fetchJSON(rawURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, rawURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return data, nil
}
