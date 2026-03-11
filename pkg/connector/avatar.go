package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

type AvatarFetcher struct {
	apiURLTemplate string
	httpClient     *http.Client
	log            zerolog.Logger
}

func NewAvatarFetcher(apiURLTemplate string, log zerolog.Logger) *AvatarFetcher {
	return &AvatarFetcher{
		apiURLTemplate: apiURLTemplate,
		httpClient:     &http.Client{Timeout: 15 * time.Second},
		log:            log,
	}
}

type AvatarResult struct {
	Changed bool   // false means 304 Not Modified
	Data    []byte // image data, only set if Changed=true
	ETag    string // ETag from server for conditional GET
}

// GhostMetadata is stored per ghost user (MC player) in the database.
type GhostMetadata struct {
	AvatarETag      string    `json:"avatar_etag,omitempty"`
	AvatarFetchedAt time.Time `json:"avatar_fetched_at,omitempty"`
}

// Fetch fetches an avatar using ETag-based conditional GET.
// Sends If-None-Match if we have an ETag from a previous fetch.
func (f *AvatarFetcher) Fetch(ctx context.Context, username string,
	etag string) (*AvatarResult, error) {

	url := fmt.Sprintf(f.apiURLTemplate, username)
	f.log.Debug().Str("url", url).Str("username", username).Msg("Fetching avatar")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	req.Header.Set("User-Agent", "mautrix-minecraft/1.0")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("avatar HTTP failed: %w", err)
	}
	defer resp.Body.Close()

	result := &AvatarResult{
		ETag: etag, // keep old ETag as fallback
	}

	if resp.StatusCode == http.StatusNotModified {
		f.log.Debug().Str("username", username).Msg("Avatar unchanged (304)")
		return result, nil // Changed=false
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("avatar API returned %d for %s", resp.StatusCode, url)
	}

	// Update ETag from response
	if newETag := resp.Header.Get("ETag"); newETag != "" {
		result.ETag = newETag
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read avatar: %w", err)
	}

	result.Changed = true
	result.Data = data
	f.log.Debug().Str("username", username).Int("bytes", len(data)).
		Msg("New avatar fetched")
	return result, nil
}
