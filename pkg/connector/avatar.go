package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// faceSize is the pixel size of the rendered avatar (8x8 face, scaled up).
const faceSize = 128

// AvatarFetcher produces avatar images for Minecraft players.
//
// By default it renders the face straight from the player's skin texture:
// Java players are looked up through Mojang's API, Floodgate/Bedrock players
// (names starting with ".") through the GeyserMC global API. This avoids
// depending on third-party render services, which have a habit of
// disappearing. If apiURLTemplate is set, that render URL is used instead.
type AvatarFetcher struct {
	apiURLTemplate string
	httpClient     *http.Client
	log            zerolog.Logger

	// API endpoints, overridable in tests.
	mojangProfileURL string // %s = username
	mojangSessionURL string // %s = uuid
	geyserXUIDURL    string // %s = gamertag
	geyserSkinURL    string // %s = xuid
	textureURL       string // %s = texture id
}

func NewAvatarFetcher(apiURLTemplate string, log zerolog.Logger) *AvatarFetcher {
	return &AvatarFetcher{
		apiURLTemplate:   apiURLTemplate,
		httpClient:       &http.Client{Timeout: 15 * time.Second},
		log:              log,
		mojangProfileURL: "https://api.mojang.com/users/profiles/minecraft/%s",
		mojangSessionURL: "https://sessionserver.mojang.com/session/minecraft/profile/%s",
		geyserXUIDURL:    "https://api.geysermc.org/v2/xbox/xuid/%s",
		geyserSkinURL:    "https://api.geysermc.org/v2/skin/%s",
		textureURL:       "https://textures.minecraft.net/texture/%s",
	}
}

type AvatarResult struct {
	Changed bool   // false means unchanged since the given ETag
	Data    []byte // image data, only set if Changed=true
	ETag    string // ETag or skin texture ID, passed back on the next fetch
}

// GhostMetadata is stored per ghost user (MC player) in the database.
type GhostMetadata struct {
	AvatarETag      string    `json:"avatar_etag,omitempty"`
	AvatarFetchedAt time.Time `json:"avatar_fetched_at,omitempty"`
}

// Fetch returns the avatar for username. etag is the value returned by the
// previous fetch; if the avatar hasn't changed since, Changed is false.
func (f *AvatarFetcher) Fetch(ctx context.Context, username string,
	etag string) (*AvatarResult, error) {
	if f.apiURLTemplate != "" {
		return f.fetchFromURL(ctx, username, etag)
	}
	return f.fetchFromSkin(ctx, username, etag)
}

// fetchFromSkin resolves the player's skin texture and renders its face.
// The texture ID doubles as the ETag, so an unchanged skin costs only the
// profile lookup.
func (f *AvatarFetcher) fetchFromSkin(ctx context.Context, username string,
	etag string) (*AvatarResult, error) {

	textureID, err := f.resolveTextureID(ctx, username)
	if err != nil {
		return nil, err
	}
	if textureID == etag {
		f.log.Debug().Str("username", username).Msg("Skin unchanged")
		return &AvatarResult{ETag: etag}, nil
	}

	skin, err := f.get(ctx, fmt.Sprintf(f.textureURL, textureID))
	if err != nil {
		return nil, fmt.Errorf("skin download failed: %w", err)
	}
	face, err := renderFace(skin)
	if err != nil {
		return nil, fmt.Errorf("render face for %s: %w", username, err)
	}
	f.log.Debug().Str("username", username).Str("texture_id", textureID).
		Msg("Rendered avatar from skin")
	return &AvatarResult{Changed: true, Data: face, ETag: textureID}, nil
}

// resolveTextureID returns the textures.minecraft.net ID of the player's skin.
func (f *AvatarFetcher) resolveTextureID(ctx context.Context, username string) (string, error) {
	if gamertag, ok := strings.CutPrefix(username, "."); ok {
		return f.resolveBedrockTexture(ctx, gamertag)
	}
	return f.resolveJavaTexture(ctx, username)
}

func (f *AvatarFetcher) resolveJavaTexture(ctx context.Context, username string) (string, error) {
	var profile struct {
		ID string `json:"id"`
	}
	if err := f.getJSON(ctx, fmt.Sprintf(f.mojangProfileURL, url.PathEscape(username)), &profile); err != nil {
		return "", fmt.Errorf("mojang profile lookup for %s: %w", username, err)
	}
	if profile.ID == "" {
		return "", fmt.Errorf("no Mojang account named %s", username)
	}

	var session struct {
		Properties []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"properties"`
	}
	if err := f.getJSON(ctx, fmt.Sprintf(f.mojangSessionURL, profile.ID), &session); err != nil {
		return "", fmt.Errorf("mojang session lookup for %s: %w", username, err)
	}
	for _, p := range session.Properties {
		if p.Name != "textures" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(p.Value)
		if err != nil {
			return "", fmt.Errorf("decode textures for %s: %w", username, err)
		}
		var tex struct {
			Textures struct {
				Skin struct {
					URL string `json:"url"`
				} `json:"SKIN"`
			} `json:"textures"`
		}
		if err := json.Unmarshal(raw, &tex); err != nil {
			return "", fmt.Errorf("parse textures for %s: %w", username, err)
		}
		if tex.Textures.Skin.URL == "" {
			break
		}
		return path.Base(tex.Textures.Skin.URL), nil
	}
	return "", fmt.Errorf("%s has no custom skin", username)
}

func (f *AvatarFetcher) resolveBedrockTexture(ctx context.Context, gamertag string) (string, error) {
	var xuid struct {
		XUID json.Number `json:"xuid"`
	}
	err := f.getJSON(ctx, fmt.Sprintf(f.geyserXUIDURL, url.PathEscape(gamertag)), &xuid)
	// Floodgate replaces spaces in gamertags with underscores.
	if (err != nil || xuid.XUID == "") && strings.Contains(gamertag, "_") {
		err = f.getJSON(ctx, fmt.Sprintf(f.geyserXUIDURL,
			url.PathEscape(strings.ReplaceAll(gamertag, "_", " "))), &xuid)
	}
	if err != nil {
		return "", fmt.Errorf("geyser xuid lookup for %s: %w", gamertag, err)
	}
	if xuid.XUID == "" {
		return "", fmt.Errorf("no Xbox account named %s", gamertag)
	}

	var skin struct {
		TextureID string `json:"texture_id"`
	}
	if err := f.getJSON(ctx, fmt.Sprintf(f.geyserSkinURL, xuid.XUID), &skin); err != nil {
		return "", fmt.Errorf("geyser skin lookup for %s: %w", gamertag, err)
	}
	if skin.TextureID == "" {
		return "", fmt.Errorf("GeyserMC has no skin cached for %s yet", gamertag)
	}
	return skin.TextureID, nil
}

func (f *AvatarFetcher) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mautrix-minecraft/1.0")
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (f *AvatarFetcher) getJSON(ctx context.Context, u string, v any) error {
	data, err := f.get(ctx, u)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// renderFace crops the face (and hat overlay) out of a 64x64 or legacy
// 64x32 skin and scales it up to faceSize with nearest-neighbour sampling.
func renderFace(skinPNG []byte) ([]byte, error) {
	skin, err := png.Decode(bytes.NewReader(skinPNG))
	if err != nil {
		return nil, fmt.Errorf("decode skin: %w", err)
	}
	b := skin.Bounds()
	if b.Dx() != 64 || (b.Dy() != 64 && b.Dy() != 32) {
		return nil, fmt.Errorf("unexpected skin size %dx%d", b.Dx(), b.Dy())
	}

	face := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(face, face.Bounds(), skin, b.Min.Add(image.Pt(8, 8)), draw.Src)
	// Legacy skins often have a fully opaque hat area; the game ignores
	// the hat layer in that case, so we do too.
	hatOrigin := b.Min.Add(image.Pt(40, 8))
	if b.Dy() == 64 || hasTransparency(skin, image.Rectangle{Min: hatOrigin, Max: hatOrigin.Add(image.Pt(8, 8))}) {
		draw.Draw(face, face.Bounds(), skin, hatOrigin, draw.Over)
	}

	out := image.NewNRGBA(image.Rect(0, 0, faceSize, faceSize))
	scale := faceSize / 8
	for y := 0; y < faceSize; y++ {
		for x := 0; x < faceSize; x++ {
			out.Set(x, y, face.At(x/scale, y/scale))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func hasTransparency(img image.Image, r image.Rectangle) bool {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				return true
			}
		}
	}
	return false
}

// fetchFromURL fetches a pre-rendered avatar from a render service using an
// ETag-based conditional GET.
func (f *AvatarFetcher) fetchFromURL(ctx context.Context, username string,
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
