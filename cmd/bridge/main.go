package main

import (
	"context"
	"fmt"
	"os"

	"github.com/deflix-tv/go-stremio"

	"github.com/krizcold/stremio-torrent-bridge/internal/addon"
	"github.com/krizcold/stremio-torrent-bridge/internal/api"
	"github.com/krizcold/stremio-torrent-bridge/internal/cache"
	"github.com/krizcold/stremio-torrent-bridge/internal/config"
	"github.com/krizcold/stremio-torrent-bridge/internal/engine"
	"github.com/krizcold/stremio-torrent-bridge/internal/proxy"
	"github.com/krizcold/stremio-torrent-bridge/internal/relay"
)

func main() {
	// 1. Load configuration from environment variables with sensible defaults.
	cfg := config.Load()
	cfg.LogSummary()

	// 2. Create all engine adapters and build the runtime-switchable multi-engine.
	torrserverAdapter := engine.NewTorrServerAdapter(cfg.TorrServerURL, cfg.TorrServerUsername, cfg.TorrServerPassword)
	rqbitAdapter := engine.NewRqbitAdapter(cfg.RqbitURL, cfg.RqbitUsername, cfg.RqbitPassword)
	qbitAdapter := engine.NewQBittorrentAdapter(cfg.QBittorrentURL, cfg.QBitDownloadPath, cfg.QBitUsername, cfg.QBitPassword)

	multi := engine.NewMultiEngine(torrserverAdapter, rqbitAdapter, qbitAdapter)
	if err := multi.SetActive(cfg.DefaultEngine); err != nil {
		fmt.Printf("Unknown engine %q, falling back to torrserver\n", cfg.DefaultEngine)
		_ = multi.SetActive("torrserver")
		cfg.DefaultEngine = "torrserver"
	}
	var eng engine.Engine = multi
	fmt.Printf("Using engine: %s\n", multi.GetActive())

	// 2b. Create the cache manager for LRU cleanup.
	cacheManager := cache.NewCacheManager(eng, cfg)

	// 3. Create the addon store for persisting wrapped addon registrations.
	store, err := addon.NewAddonStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create addon store: %v\n", err)
		os.Exit(1)
	}

	// 4. Create the Browser Tab Relay server for proxying addon fetches
	//    through a connected browser tab (residential IP).
	relayServer := relay.NewServer()

	// 5. Create the addon wrapper (manifest rewrite + stream interception)
	//    and the stream proxy (video passthrough with Range support).
	wrapper := addon.NewWrapper(store, cfg, eng, relayServer)
	streamProxy := proxy.NewStreamProxy(eng, cacheManager)

	// 6. Create the management REST API handlers.
	handlers := api.NewHandlers(store, cfg, eng, multi, cacheManager, wrapper, relayServer)

	// 7. Create the go-stremio addon with manifest and placeholder stream handlers.
	//    The placeholder handlers return NotFound because the real stream handling
	//    is done by the wrapper (for addon protocol) and stream proxy (for direct
	//    HTTP streams). go-stremio requires at least one stream handler since our
	//    manifest declares the "stream" resource.
	manifest := stremio.Manifest{
		ID:          "com.yundera.torrent-bridge",
		Name:        "Torrent Bridge",
		Description: "Wraps Stremio addons for full TCP/UDP peer connectivity",
		Version:     "0.1.0",
		// Logo is Stremio's canonical display image for an addon; the
		// Manifest struct has no Icon field.
		Logo:     "https://cdn.jsdelivr.net/gh/krizcold/stremio-torrent-bridge@main/assets/icon.png",
		Types:    []string{"movie", "series"},
		Catalogs: []stremio.CatalogItem{},
		ResourceItems: []stremio.ResourceItem{
			{
				Name:  "stream",
				Types: []string{"movie", "series"},
			},
		},
	}

	placeholderStreamHandler := func(ctx context.Context, id string, userData interface{}) ([]stremio.StreamItem, error) {
		return nil, stremio.NotFound
	}

	streamHandlers := map[string]stremio.StreamHandler{
		"movie":  placeholderStreamHandler,
		"series": placeholderStreamHandler,
	}

	opts := stremio.Options{
		BindAddr: cfg.BindAddr,
		Port:     cfg.Port,
	}

	stremioAddon, err := stremio.NewAddon(manifest, nil, streamHandlers, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create stremio addon: %v\n", err)
		os.Exit(1)
	}

	// 8. Register all routes: management API, wrap endpoints, stream proxy, relay, and UI.
	api.RegisterRoutes(stremioAddon, handlers, wrapper, streamProxy, relayServer)

	// 9. Start cache manager background cleanup.
	cacheManager.Start()
	defer cacheManager.Stop()

	// 10. Start the server.
	fmt.Printf("Torrent Bridge starting on %s:%d\n", cfg.BindAddr, cfg.Port)
	stopChan := make(chan bool, 1)
	stremioAddon.Run(stopChan)
}
