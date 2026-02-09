package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/krizcold/stremio-torrent-bridge/internal/config"
	"github.com/krizcold/stremio-torrent-bridge/internal/engine"
)

// AccessEntry tracks when a torrent was last accessed for LRU eviction.
type AccessEntry struct {
	InfoHash     string    `json:"infoHash"`
	Name         string    `json:"name"`
	LastAccessed time.Time `json:"lastAccessed"`
	Size         int64     `json:"size"` // total size in bytes (sum of all files)
}

// CacheStats is a snapshot of current cache state returned by GetStats.
type CacheStats struct {
	TotalSizeBytes int64          `json:"totalSizeBytes"`
	TotalSizeGB    float64        `json:"totalSizeGB"`
	TorrentCount   int            `json:"torrentCount"`
	MaxSizeGB      int            `json:"maxSizeGB"`
	MaxAgeDays     int            `json:"maxAgeDays"`
	OldestAccess   *time.Time     `json:"oldestAccess,omitempty"`
	Torrents       []AccessEntry  `json:"torrents"`
}

// CacheManager tracks torrent access times and evicts stale or oversized
// entries from the torrent engine on a background schedule.
type CacheManager struct {
	engine    engine.Engine
	config    *config.Config
	mu        sync.RWMutex
	accessLog map[string]*AccessEntry // infoHash -> access info
	filePath  string                  // persistence path for access log
	stopCh    chan struct{}
}

// NewCacheManager creates a CacheManager that tracks access for the given
// engine and enforces limits from cfg. It loads any previously persisted
// access log from disk.
func NewCacheManager(eng engine.Engine, cfg *config.Config) *CacheManager {
	cm := &CacheManager{
		engine:    eng,
		config:    cfg,
		accessLog: make(map[string]*AccessEntry),
		filePath:  cfg.DataDir + "/cache_access.json",
		stopCh:    make(chan struct{}),
	}

	if err := cm.load(); err != nil {
		fmt.Printf("Cache manager: failed to load access log: %v (starting fresh)\n", err)
	} else if len(cm.accessLog) > 0 {
		fmt.Printf("Cache manager: loaded %d entries from %s\n", len(cm.accessLog), cm.filePath)
	}

	return cm
}

// RecordAccess updates the access timestamp for a torrent. It is called from
// the stream proxy on every stream request and must return quickly. Disk
// persistence happens asynchronously.
func (cm *CacheManager) RecordAccess(infoHash, name string, totalSize int64) {
	cm.mu.Lock()
	entry, exists := cm.accessLog[infoHash]
	if !exists {
		entry = &AccessEntry{InfoHash: infoHash}
		cm.accessLog[infoHash] = entry
	}
	entry.LastAccessed = time.Now()
	if name != "" {
		entry.Name = name
	}
	if totalSize > 0 {
		entry.Size = totalSize
	}
	cm.mu.Unlock()

	// Save to disk in the background so the caller is not blocked.
	go func() {
		if err := cm.save(); err != nil {
			fmt.Printf("Cache manager: failed to save access log: %v\n", err)
		}
	}()
}

// Start launches the background cleanup goroutine. It runs cleanup
// immediately on startup and then every hour until Stop is called.
func (cm *CacheManager) Start() {
	go cm.loop()
}

// Stop signals the background cleanup goroutine to exit.
func (cm *CacheManager) Stop() {
	close(cm.stopCh)
}

// loop is the background goroutine that periodically runs cleanup.
func (cm *CacheManager) loop() {
	// Sync with engine and run cleanup immediately on start.
	cm.syncAndCleanup()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cm.syncAndCleanup()
		case <-cm.stopCh:
			fmt.Println("Cache manager: stopped")
			return
		}
	}
}

// syncAndCleanup syncs the access log with the engine and then runs cleanup.
// Errors are logged but do not stop the background loop.
func (cm *CacheManager) syncAndCleanup() {
	if err := cm.syncWithEngine(); err != nil {
		fmt.Printf("Cache manager: engine sync failed: %v (will retry next cycle)\n", err)
		return
	}
	removed, err := cm.RunCleanup()
	if err != nil {
		fmt.Printf("Cache manager: cleanup failed: %v (will retry next cycle)\n", err)
		return
	}
	if removed > 0 {
		fmt.Printf("Cache manager: cleanup removed %d torrents\n", removed)
	}
}

// syncWithEngine reconciles the in-memory access log with the engine's actual
// torrent list. Torrents the engine knows about but we don't are added with
// the current time. Entries we have for torrents the engine no longer has are
// removed.
func (cm *CacheManager) syncWithEngine() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	torrents, err := cm.engine.ListTorrents(ctx)
	if err != nil {
		return fmt.Errorf("ListTorrents: %w", err)
	}

	engineHashes := make(map[string]engine.TorrentInfo, len(torrents))
	for _, t := range torrents {
		engineHashes[t.InfoHash] = t
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Add entries for torrents the engine has but we don't track yet.
	for hash, t := range engineHashes {
		entry, exists := cm.accessLog[hash]
		if !exists {
			cm.accessLog[hash] = &AccessEntry{
				InfoHash:     hash,
				Name:         t.Name,
				LastAccessed: time.Now(),
				Size:         t.TotalSize,
			}
		} else if entry.Size == 0 && t.TotalSize > 0 {
			// Update size if it was previously unknown (metadata wasn't ready).
			entry.Size = t.TotalSize
			if entry.Name == "" && t.Name != "" {
				entry.Name = t.Name
			}
		}
	}

	// Remove entries for torrents the engine no longer has.
	for hash := range cm.accessLog {
		if _, exists := engineHashes[hash]; !exists {
			delete(cm.accessLog, hash)
		}
	}

	return nil
}

// RunCleanup enforces age and size limits by removing torrents from the engine.
// It returns the number of torrents removed.
func (cm *CacheManager) RunCleanup() (int, error) {
	cm.mu.Lock()

	// Build a sorted slice (oldest first) from the current access log.
	entries := make([]*AccessEntry, 0, len(cm.accessLog))
	for _, e := range cm.accessLog {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].LastAccessed.Before(entries[j].LastAccessed)
	})

	// Determine which torrents to remove based on age.
	maxAge := time.Duration(cm.config.CacheMaxAgeDays) * 24 * time.Hour
	cutoff := time.Now().Add(-maxAge)
	maxBytes := int64(cm.config.CacheSizeGB) * 1024 * 1024 * 1024

	var toRemoveAge []string
	var remaining []*AccessEntry

	for _, e := range entries {
		if e.LastAccessed.Before(cutoff) {
			toRemoveAge = append(toRemoveAge, e.InfoHash)
		} else {
			remaining = append(remaining, e)
		}
	}

	// Determine which additional torrents to remove based on total size.
	// Calculate total size of remaining (non-aged-out) entries.
	var totalSize int64
	for _, e := range remaining {
		totalSize += e.Size
	}

	var toRemoveSize []string
	if totalSize > maxBytes {
		// remaining is already sorted oldest-first, remove from the front.
		for i := 0; i < len(remaining) && totalSize > maxBytes; i++ {
			toRemoveSize = append(toRemoveSize, remaining[i].InfoHash)
			totalSize -= remaining[i].Size
		}
	}

	cm.mu.Unlock()

	// Combine all hashes to remove.
	toRemove := append(toRemoveAge, toRemoveSize...)
	if len(toRemove) == 0 {
		cm.logStats()
		return 0, nil
	}

	// Remove each torrent from the engine. Each call gets its own timeout.
	removed := 0
	for _, hash := range toRemove {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := cm.engine.RemoveTorrent(ctx, hash, true)
		cancel()
		if err != nil {
			fmt.Printf("Cache manager: failed to remove %s: %v\n", hash, err)
			continue
		}
		removed++
		cm.mu.Lock()
		delete(cm.accessLog, hash)
		cm.mu.Unlock()
	}

	// Persist the updated access log.
	if err := cm.save(); err != nil {
		fmt.Printf("Cache manager: failed to save after cleanup: %v\n", err)
	}

	cm.logStats()
	return removed, nil
}

// logStats prints a summary line with the current cache state.
func (cm *CacheManager) logStats() {
	cm.mu.RLock()
	var totalSize int64
	for _, e := range cm.accessLog {
		totalSize += e.Size
	}
	count := len(cm.accessLog)
	cm.mu.RUnlock()

	sizeGB := float64(totalSize) / (1024 * 1024 * 1024)
	fmt.Printf("Cache cleanup: %d torrents using %.2f GB (limit: %d GB, max age: %d days)\n",
		count, sizeGB, cm.config.CacheSizeGB, cm.config.CacheMaxAgeDays)
}

// GetStats returns a snapshot of the current cache state for the API.
func (cm *CacheManager) GetStats() *CacheStats {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	stats := &CacheStats{
		MaxSizeGB:  cm.config.CacheSizeGB,
		MaxAgeDays: cm.config.CacheMaxAgeDays,
		Torrents:   make([]AccessEntry, 0, len(cm.accessLog)),
	}

	for _, e := range cm.accessLog {
		stats.TotalSizeBytes += e.Size
		stats.Torrents = append(stats.Torrents, *e)
	}

	stats.TorrentCount = len(stats.Torrents)
	stats.TotalSizeGB = float64(stats.TotalSizeBytes) / (1024 * 1024 * 1024)

	// Sort by lastAccessed descending (most recent first) for the API response.
	sort.Slice(stats.Torrents, func(i, j int) bool {
		return stats.Torrents[i].LastAccessed.After(stats.Torrents[j].LastAccessed)
	})

	if len(stats.Torrents) > 0 {
		oldest := stats.Torrents[len(stats.Torrents)-1].LastAccessed
		stats.OldestAccess = &oldest
	}

	return stats
}

// load reads the persisted access log from disk. Returns nil if the file
// does not exist (a fresh start is fine).
func (cm *CacheManager) load() error {
	data, err := os.ReadFile(cm.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", cm.filePath, err)
	}

	var entries []*AccessEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse %s: %w", cm.filePath, err)
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()
	for _, e := range entries {
		cm.accessLog[e.InfoHash] = e
	}

	return nil
}

// save writes the access log to disk as JSON.
func (cm *CacheManager) save() error {
	cm.mu.RLock()
	entries := make([]*AccessEntry, 0, len(cm.accessLog))
	for _, e := range cm.accessLog {
		entries = append(entries, e)
	}
	cm.mu.RUnlock()

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if err := os.WriteFile(cm.filePath, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", cm.filePath, err)
	}

	return nil
}
