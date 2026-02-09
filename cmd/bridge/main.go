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

	// 2. Create the torrent engine adapter based on configuration.
	var eng engine.Engine
	switch cfg.DefaultEngine {
	case "torrserver":
		eng = engine.NewTorrServerAdapter(cfg.TorrServerURL)
	case "rqbit":
		eng = engine.NewRqbitAdapter(cfg.RqbitURL)
	case "qbittorrent":
		eng = engine.NewQBittorrentAdapter(cfg.QBittorrentURL, cfg.QBitDownloadPath, cfg.QBitUsername, cfg.QBitPassword)
	default:
		eng = engine.NewTorrServerAdapter(cfg.TorrServerURL)
	}
	fmt.Printf("Using engine: %s\n", eng.Name())

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
	handlers := api.NewHandlers(store, cfg, eng, cacheManager, wrapper, relayServer)

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
		Types:       []string{"movie", "series"},
		Catalogs:    []stremio.CatalogItem{},
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
