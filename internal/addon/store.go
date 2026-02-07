package addon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// AddonStore manages wrapped addons with thread-safe in-memory storage and JSON persistence
type AddonStore struct {
	mu       sync.RWMutex
	addons   map[string]*WrappedAddon
	filePath string
}

// NewAddonStore creates a new addon store with the specified data directory
func NewAddonStore(dataDir string) (*AddonStore, error) {
	store := &AddonStore{
		addons:   make(map[string]*WrappedAddon),
		filePath: dataDir + "/addons.json",
	}

	// Load existing data from disk (if file doesn't exist, starts with empty map)
	if err := store.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load addon store: %w", err)
	}

	return store, nil
}

// Add creates a new wrapped addon with the given original URL
// If an addon with the same ID already exists, returns the existing addon (idempotent)
func (s *AddonStore) Add(originalURL string) (*WrappedAddon, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Generate ID from SHA256 hash of the original URL
	hash := sha256.Sum256([]byte(originalURL))
	id := hex.EncodeToString(hash[:])[:8]

	// Check if addon already exists
	if existing, found := s.addons[id]; found {
		return existing, nil
	}

	// Create new addon
	addon := &WrappedAddon{
		ID:          id,
		OriginalURL: originalURL,
		Name:        "", // Will be populated later when manifest is fetched
		CreatedAt:   time.Now(),
	}

	s.addons[id] = addon

	// Save to disk
	if err := s.save(); err != nil {
		return nil, fmt.Errorf("failed to save addon: %w", err)
	}

	return addon, nil
}

// Get retrieves an addon by its ID
func (s *AddonStore) Get(id string) (*WrappedAddon, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	addon, found := s.addons[id]
	return addon, found
}

// List returns all addons sorted by creation time
func (s *AddonStore) List() []*WrappedAddon {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*WrappedAddon, 0, len(s.addons))
	for _, addon := range s.addons {
		result = append(result, addon)
	}

	// Sort by CreatedAt
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})

	return result
}

// Remove deletes an addon by its ID
func (s *AddonStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, found := s.addons[id]; !found {
		return fmt.Errorf("addon with id %s not found", id)
	}

	delete(s.addons, id)

	// Save to disk
	if err := s.save(); err != nil {
		return fmt.Errorf("failed to save after removal: %w", err)
	}

	return nil
}

// UpdateName updates the name of an addon
func (s *AddonStore) UpdateName(id string, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	addon, found := s.addons[id]
	if !found {
		return fmt.Errorf("addon with id %s not found", id)
	}

	addon.Name = name

	// Save to disk
	if err := s.save(); err != nil {
		return fmt.Errorf("failed to save after name update: %w", err)
	}

	return nil
}

// load reads the addons from the JSON file on disk
func (s *AddonStore) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}

	var addons map[string]*WrappedAddon
	if err := json.Unmarshal(data, &addons); err != nil {
		return fmt.Errorf("failed to unmarshal addons: %w", err)
	}

	s.addons = addons
	return nil
}

// save writes the addons to the JSON file on disk
func (s *AddonStore) save() error {
	data, err := json.MarshalIndent(s.addons, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal addons: %w", err)
	}

	if err := os.WriteFile(s.filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write addons file: %w", err)
	}

	return nil
}
