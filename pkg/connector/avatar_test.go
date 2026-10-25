package connector

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

var (
	skinFace = color.NRGBA{R: 200, G: 150, B: 100, A: 255}
	skinHat  = color.NRGBA{R: 10, G: 20, B: 30, A: 255}
)

// testSkin returns a skin PNG with a solid face and, if hat is true, a hat
// pixel in the top-left corner of the face overlay. With opaqueHat, the
// whole hat area is opaque (the legacy-skin case the game ignores).
func testSkin(t *testing.T, height int, hat, opaqueHat bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, height))
	for y := 8; y < 16; y++ {
		for x := 8; x < 16; x++ {
			img.SetNRGBA(x, y, skinFace)
		}
		for x := 40; x < 48; x++ {
			if opaqueHat {
				img.SetNRGBA(x, y, skinHat)
			}
		}
	}
	if hat {
		img.SetNRGBA(40, 8, skinHat)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeFace(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != avatarSize || b.Dy() != avatarSize {
		t.Fatalf("avatar is %dx%d, want %dx%d", b.Dx(), b.Dy(), avatarSize, avatarSize)
	}
	return img
}

func sameColor(a, b color.Color) bool {
	r1, g1, b1, a1 := a.RGBA()
	r2, g2, b2, a2 := b.RGBA()
	return r1 == r2 && g1 == g2 && b1 == b2 && a1 == a2
}

func TestRenderFace(t *testing.T) {
	first := (avatarSize - 8*faceScale) / 2
	last := first + 8*faceScale - 1
	tests := []struct {
		name      string
		skin      []byte
		topLeft   color.Color
		bottomRgt color.Color
	}{
		{"modern skin with hat", testSkin(t, 64, true, false), skinHat, skinFace},
		{"modern skin without hat", testSkin(t, 64, false, false), skinFace, skinFace},
		{"legacy skin with hat", testSkin(t, 32, true, false), skinHat, skinFace},
		{"legacy skin with opaque hat area is ignored", testSkin(t, 32, false, true), skinFace, skinFace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := renderFace(tt.skin)
			if err != nil {
				t.Fatal(err)
			}
			img := decodeFace(t, data)
			// The face's corners must survive a circular crop.
			r := float64(avatarSize) / 2
			if d := math.Hypot(float64(first)-r, float64(first)-r); d > r {
				t.Errorf("face corner is %.1fpx from centre, outside the %.0fpx circle", d, r)
			}
			if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
				t.Errorf("margin should be transparent, got alpha %d", a)
			}
			if got := img.At(first, first); !sameColor(got, tt.topLeft) {
				t.Errorf("top-left = %v, want %v", got, tt.topLeft)
			}
			if got := img.At(last, last); !sameColor(got, tt.bottomRgt) {
				t.Errorf("bottom-right = %v, want %v", got, tt.bottomRgt)
			}
		})
	}

	if _, err := renderFace([]byte("not a png")); err == nil {
		t.Error("expected error for invalid PNG")
	}
}

func newTestFetcher(t *testing.T, handler http.HandlerFunc) *AvatarFetcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	f := NewAvatarFetcher("", zerolog.Nop())
	f.mojangProfileURL = srv.URL + "/mojang/profile/%s"
	f.mojangSessionURL = srv.URL + "/mojang/session/%s"
	f.geyserXUIDURL = srv.URL + "/geyser/xuid/%s"
	f.geyserSkinURL = srv.URL + "/geyser/skin/%s"
	f.textureURL = srv.URL + "/texture/%s"
	return f
}

func TestAvatarFetchJavaAndBedrock(t *testing.T) {
	skin := testSkin(t, 64, false, false)
	textures := base64.StdEncoding.EncodeToString(
		[]byte(`{"textures":{"SKIN":{"url":"http://textures.minecraft.net/texture/javatex"}}}`))
	var textureHits int

	f := newTestFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mojang/profile/Sverre":
			fmt.Fprint(w, `{"id":"uuid1","name":"Sverre"}`)
		case "/mojang/session/uuid1":
			fmt.Fprintf(w, `{"properties":[{"name":"textures","value":%q}]}`, textures)
		case "/geyser/xuid/Some Gamer":
			fmt.Fprint(w, `{"xuid":2535437126507482}`)
		case "/geyser/skin/2535437126507482":
			fmt.Fprint(w, `{"texture_id":"bedrocktex","is_steve":false}`)
		case "/texture/javatex", "/texture/bedrocktex":
			textureHits++
			w.Write(skin)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()

	res, err := f.Fetch(ctx, "Sverre", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.ETag != "javatex" {
		t.Fatalf("unexpected Java result: changed=%v etag=%q", res.Changed, res.ETag)
	}
	decodeFace(t, res.Data)

	// Floodgate turns spaces into underscores; the lookup retries with spaces.
	res, err = f.Fetch(ctx, ".Some_Gamer", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.ETag != "bedrocktex" {
		t.Fatalf("unexpected Bedrock result: changed=%v etag=%q", res.Changed, res.ETag)
	}

	hits := textureHits
	res, err = f.Fetch(ctx, "Sverre", "javatex")
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Data != nil || textureHits != hits {
		t.Fatalf("unchanged skin should not be downloaded again: %+v", res)
	}

	if _, err := f.Fetch(ctx, "NoSuchPlayer", ""); err == nil {
		t.Fatal("expected error for unknown player")
	}
}

func TestAvatarFetchFromURLTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte("img"))
	}))
	defer srv.Close()

	f := NewAvatarFetcher(srv.URL+"/%s", zerolog.Nop())
	res, err := f.Fetch(context.Background(), "Sverre", "")
	if err != nil || !res.Changed || res.ETag != `"v1"` || string(res.Data) != "img" {
		t.Fatalf("unexpected result: %+v %v", res, err)
	}
	res, err = f.Fetch(context.Background(), "Sverre", `"v1"`)
	if err != nil || res.Changed {
		t.Fatalf("expected 304 to be unchanged: %+v %v", res, err)
	}
}
