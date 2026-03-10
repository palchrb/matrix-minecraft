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
	Changed bool   // false betyr 304 Not Modified
	Data    []byte // bilde-data, kun satt hvis Changed=true
	ETag    string // ETag fra serveren for conditional GET
}

// GhostMetadata lagres per ghost-bruker (MC-spiller) i databasen.
type GhostMetadata struct {
	AvatarETag      string    `json:"avatar_etag,omitempty"`
	AvatarFetchedAt time.Time `json:"avatar_fetched_at,omitempty"`
}

// Fetch henter avatar med ETag-basert conditional GET.
// Sender If-None-Match hvis vi har en ETag fra forrige henting.
func (f *AvatarFetcher) Fetch(ctx context.Context, username string,
	etag string) (*AvatarResult, error) {

	url := fmt.Sprintf(f.apiURLTemplate, username)
	f.log.Debug().Str("url", url).Str("username", username).Msg("Henter avatar")
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
		return nil, fmt.Errorf("avatar HTTP feilet: %w", err)
	}
	defer resp.Body.Close()

	result := &AvatarResult{
		ETag: etag, // behold gammel ETag som fallback
	}

	if resp.StatusCode == http.StatusNotModified {
		f.log.Debug().Str("username", username).Msg("Avatar uendret (304)")
		return result, nil // Changed=false
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("avatar API svarte %d for %s", resp.StatusCode, url)
	}

	// Oppdater ETag fra respons
	if newETag := resp.Header.Get("ETag"); newETag != "" {
		result.ETag = newETag
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("kunne ikke lese avatar: %w", err)
	}

	result.Changed = true
	result.Data = data
	f.log.Debug().Str("username", username).Int("bytes", len(data)).
		Msg("Ny avatar hentet")
	return result, nil
}
