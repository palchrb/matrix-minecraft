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
	Changed      bool      // false betyr 304 Not Modified
	Data         []byte    // PNG-data, kun satt hvis Changed=true
	LastModified time.Time // oppdatert timestamp fra serveren
	AccountValid bool      // false hvis ukjent MC-bruker
}

// GhostMetadata lagres per ghost-bruker (MC-spiller) i databasen.
type GhostMetadata struct {
	AvatarLastModified time.Time `json:"avatar_last_modified,omitempty"`
	AvatarMXC          string    `json:"avatar_mxc,omitempty"`
	AvatarValid        bool      `json:"avatar_valid"`
}

// Fetch henter avatar med HTTP If-Modified-Since conditional GET.
func (f *AvatarFetcher) Fetch(ctx context.Context, username string,
	lastModified time.Time) (*AvatarResult, error) {

	url := fmt.Sprintf(f.apiURLTemplate, username)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	if !lastModified.IsZero() {
		req.Header.Set("If-Modified-Since",
			lastModified.UTC().Format(http.TimeFormat))
	}
	req.Header.Set("User-Agent", "mautrix-minecraft/1.0")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("avatar HTTP feilet: %w", err)
	}
	defer resp.Body.Close()

	result := &AvatarResult{
		LastModified: lastModified,
		AccountValid: resp.Header.Get("X-Account-Valid") != "false",
	}

	if resp.StatusCode == http.StatusNotModified {
		f.log.Debug().Str("username", username).Msg("Avatar uendret (304)")
		return result, nil // Changed=false
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("avatar API svarte %d", resp.StatusCode)
	}

	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			result.LastModified = t
		}
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
