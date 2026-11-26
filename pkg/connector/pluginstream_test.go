package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func newTestStream(t *testing.T, srv *httptest.Server, token string) *PluginStream {
	t.Helper()
	p := NewPluginStream(srv.URL, token, "mc-test", zerolog.Nop())
	p.minBackoff = 10 * time.Millisecond
	p.maxBackoff = 20 * time.Millisecond
	return p
}

func recv(t *testing.T, ch <-chan ChatLine) ChatLine {
	t.Helper()
	select {
	case cl := <-ch:
		return cl
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
		return ChatLine{}
	}
}

func TestPluginStreamReconnectsWithLastEventID(t *testing.T) {
	var mu sync.Mutex
	var lastIDs []string
	var conns int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		conns++
		n := conns
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "retry: 2000\n: connected\n\n")
		if n == 1 {
			fmt.Fprint(w, "id: boot-1\ndata: {\"type\":\"join\",\"player\":\".palchrb\",\"message\":\".palchrb joined the game\",\"ts\":1700000000000}\n\n")
			fmt.Fprint(w, ": ping\n\n")
			fmt.Fprint(w, "id: boot-2\ndata: {\"type\":\"chat\",\"player\":\"Sverre\",\"message\":\"hei\"}\n\n")
			f.Flush()
			return // drop the connection
		}
		fmt.Fprint(w, "id: boot-3\ndata: {\"type\":\"death\",\"player\":\"Sverre\",\"message\":\"Sverre was slain by Zombie\"}\n\n")
		f.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := newTestStream(t, srv, "secret")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan ChatLine, 8)
	go p.Start(ctx, ch)

	join := recv(t, ch)
	if join.Event != EventJoin || join.PlayerName != ".palchrb" || join.Message != "joined the game" {
		t.Fatalf("unexpected join: %+v", join)
	}
	if !join.Timestamp.Equal(time.UnixMilli(1700000000000)) {
		t.Fatalf("unexpected timestamp: %v", join.Timestamp)
	}
	if join.ServerID != "mc-test" {
		t.Fatalf("unexpected server: %q", join.ServerID)
	}
	chat := recv(t, ch)
	if chat.Event != EventChat || chat.PlayerName != "Sverre" || chat.Message != "hei" {
		t.Fatalf("unexpected chat: %+v", chat)
	}
	death := recv(t, ch)
	if death.Event != EventDeath || death.Message != "was slain by Zombie" {
		t.Fatalf("unexpected death: %+v", death)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lastIDs) < 2 || lastIDs[0] != "" || lastIDs[1] != "boot-2" {
		t.Fatalf("unexpected Last-Event-ID sequence: %q", lastIDs)
	}
}

func TestPluginStreamIdleTimeout(t *testing.T) {
	var mu sync.Mutex
	var conns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never send anything, not even pings
	}))
	defer srv.Close()

	p := newTestStream(t, srv, "secret")
	p.idleTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Start(ctx, make(chan ChatLine))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := conns
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected a reconnect after the idle timeout")
}

func TestPluginProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"plugin":"matrix-minecraft-bridge","server":"Paper 26.1.2","motd":"hi"}`)
	}))
	defer srv.Close()

	info, err := newTestStream(t, srv, "secret").Probe(context.Background())
	if err != nil || info.Server != "Paper 26.1.2" {
		t.Fatalf("probe with right token failed: %+v %v", info, err)
	}
	if _, err := newTestStream(t, srv, "wrong").Probe(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("probe with wrong token should be ErrUnauthorized, got %v", err)
	}
}

func TestPluginEventToChatLine(t *testing.T) {
	cl, ok := pluginEvent{Type: "join", Player: "Sverre", Message: "Velkommen, Sverre!"}.toChatLine()
	if !ok || cl.FullText != "Velkommen, Sverre!" {
		t.Fatalf("custom join message should be kept whole: %+v", cl)
	}
	cl, ok = pluginEvent{Type: "advancement", Player: "Sverre", Message: "Sverre has made the advancement [Stone Age]"}.toChatLine()
	if !ok || cl.Event != EventAdvancement || cl.Message != "has made the advancement [Stone Age]" || cl.FullText != "" {
		t.Fatalf("unexpected advancement: %+v", cl)
	}
	if _, ok := (pluginEvent{Type: "weather", Player: "x", Message: "y"}).toChatLine(); ok {
		t.Fatal("unknown type should be ignored")
	}
}

func TestPluginProbeRejectsOtherServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	if _, err := newTestStream(t, srv, "secret").Probe(context.Background()); err == nil {
		t.Fatal("a non-plugin service must not pass the probe")
	}
}

func TestPluginSend(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/prefix/send" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got["message"] == "fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"server could not display the message"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	// Base URLs with a path (reverse proxy) and a trailing slash must work.
	p := NewPluginStream(srv.URL+"/prefix/", "secret", "mc-test", zerolog.Nop())
	ctx := context.Background()
	format := SendFormat{Prefix: "[Matrix]", SenderColor: "aqua"}
	if err := p.Send(ctx, "Pål", "hei <3", format); err != nil {
		t.Fatal(err)
	}
	if got["sender"] != "Pål" || got["message"] != "hei <3" {
		t.Fatalf("unexpected body: %v", got)
	}
	if f, _ := got["format"].(map[string]any); f["prefix"] != "[Matrix]" || f["sender_color"] != "aqua" {
		t.Fatalf("unexpected format: %v", got["format"])
	}
	if err := p.Send(ctx, "Pål", "fail", format); err == nil || !strings.Contains(err.Error(), "could not display") {
		t.Fatalf("expected plugin error to be surfaced, got %v", err)
	}
	wrong := NewPluginStream(srv.URL+"/prefix", "wrong", "mc-test", zerolog.Nop())
	if err := wrong.Send(ctx, "Pål", "hei", format); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestPluginStreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: b-1\ndata: {\"type\":\"chat\",\"player\":\"Sverre\",\"message\":\"hei\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bad := newTestStream(t, srv, "wrong")
	go bad.Start(ctx, make(chan ChatLine, 1))
	waitFor(t, func() bool { return bad.Status().LastError != "" })
	if st := bad.Status(); st.Connected || !strings.Contains(st.LastError, "rejected the token") {
		t.Fatalf("unexpected status for wrong token: %+v", st)
	}

	good := newTestStream(t, srv, "secret")
	ch := make(chan ChatLine, 1)
	go good.Start(ctx, ch)
	recv(t, ch)
	if st := good.Status(); !st.Connected || st.LastEvent.IsZero() || st.LastError != "" {
		t.Fatalf("unexpected status for good stream: %+v", st)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestPluginDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		fmt.Fprint(w, `{"plugin":"matrix-minecraft-bridge"}`)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	p := newTestStream(t, redirector, "secret")
	if _, err := p.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), target.URL) {
		t.Fatalf("probe should fail on a redirect and name the target, got %v", err)
	}
	if err := p.Send(context.Background(), "a", "b", SendFormat{}); err == nil {
		t.Fatal("send should fail on a redirect")
	}
	if leaked.Load() {
		t.Fatal("token was sent to the redirect target")
	}
}
