package connector

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func newTestStream(t *testing.T, srv *httptest.Server, token string) *PluginStream {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	p := NewPluginStream(host, port, token, "mc-test", zerolog.Nop())
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
	if join.ContainerName != "mc-test" {
		t.Fatalf("unexpected container: %q", join.ContainerName)
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
		fmt.Fprint(w, `{"plugin":"matrix-minecraft-bridge"}`)
	}))
	defer srv.Close()

	if err := newTestStream(t, srv, "secret").Probe(context.Background()); err != nil {
		t.Fatalf("probe with right token failed: %v", err)
	}
	if err := newTestStream(t, srv, "wrong").Probe(context.Background()); err == nil {
		t.Fatal("probe with wrong token should fail")
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
