package addon

import "time"

// WrappedAddon represents a wrapped Stremio addon configuration
type WrappedAddon struct {
	ID          string    `json:"id"`
	OriginalURL string    `json:"originalUrl"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"createdAt"`
}
