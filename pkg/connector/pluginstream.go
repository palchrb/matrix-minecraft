package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// EventType indicates which type of event a ChatLine represents.
type EventType int

const (
	EventChat        EventType = iota // Regular chat message
	EventJoin                         // Player joined the game
	EventLeave                        // Player left the game
	EventDeath                        // Player died
	EventAdvancement                  // Player got an advancement
)

// ChatLine is one game event on its way to Matrix.
type ChatLine struct {
	PlayerName string
	Message    string
	Timestamp  time.Time
	ServerID   string
	Event      EventType

	// FullText, if set, is the complete event text to show instead of
	// "<PlayerName> <Message>", e.g. a custom join message that doesn't
	// start with the player name.
	FullText string
}

// pluginEvent is one JSON event from the plugin's /events stream.
type pluginEvent struct {
	Type    string `json:"type"`
	Player  string `json:"player"`
	Message string `json:"message"`
	TS      int64  `json:"ts"`
}

// PluginInfo is what the plugin reports on /health.
type PluginInfo struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Server  string `json:"server"`
	MOTD    string `json:"motd"`
}

// SendFormat controls how Matrix messages look in game. The plugin only
// accepts named Minecraft colors and falls back to defaults otherwise.
type SendFormat struct {
	Prefix       string `json:"prefix"`
	PrefixColor  string `json:"prefix_color,omitempty"`
	SenderColor  string `json:"sender_color,omitempty"`
	MessageColor string `json:"message_color,omitempty"`
}

// ErrUnauthorized means the plugin rejected the token.
var ErrUnauthorized = errors.New("the plugin rejected the token")

// StreamStatus is a snapshot of a PluginStream for list-servers.
type StreamStatus struct {
	Connected bool
	LastEvent time.Time
	LastError string
}

// PluginStream is the bridge's connection to one server's companion
// plugin: it subscribes to the Server-Sent Events stream at /events for
// game events and posts Matrix messages to /send.
type PluginStream struct {
	baseURL  string
	token    string
	serverID string
	client   *http.Client
	log      zerolog.Logger

	// idleTimeout drops the connection if nothing (not even a ping) arrives
	// for this long. The plugin pings every 15 seconds.
	idleTimeout time.Duration
	minBackoff  time.Duration
	maxBackoff  time.Duration

	connected atomic.Bool
	lastEvent atomic.Int64 // unix millis
	errMu     sync.Mutex
	lastError string
}

// NewPluginStream creates a stream for the plugin at baseURL, e.g.
// http://minecraft-mc-1:25580 or https://mc.example.com/bridge.
func NewPluginStream(baseURL, token, serverID string, log zerolog.Logger) *PluginStream {
	return &PluginStream{
		baseURL:  strings.TrimRight(baseURL, "/"),
		token:    token,
		serverID: serverID,
		client: &http.Client{
			// The plugin never redirects. Following one could send the token
			// somewhere else, e.g. from https:// to http:// on the same host.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		log:         log.With().Str("server", serverID).Logger(),
		idleTimeout: 45 * time.Second,
		minBackoff:  2 * time.Second,
		maxBackoff:  30 * time.Second,
	}
}

func (p *PluginStream) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("User-Agent", "matrix-minecraft")
	return req, nil
}

// Probe checks that the plugin is reachable and accepts the token.
func (p *PluginStream) Probe(ctx context.Context) (*PluginInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := p.newRequest(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	} else if resp.StatusCode != http.StatusOK {
		return nil, unexpectedStatus(resp)
	}
	var info PluginInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&info); err != nil {
		return nil, fmt.Errorf("unexpected health response (is this the matrix-minecraft plugin?): %w", err)
	}
	if info.Plugin != "matrix-minecraft-bridge" {
		return nil, fmt.Errorf("unexpected health response (is this the matrix-minecraft plugin?)")
	}
	return &info, nil
}

// Send shows a Matrix message in game. It returns once the plugin has
// displayed it, or an error if it couldn't.
func (p *PluginStream) Send(ctx context.Context, sender, message string, format SendFormat) error {
	body, err := json.Marshal(map[string]any{
		"sender":  sender,
		"message": message,
		"format":  format,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := p.newRequest(ctx, http.MethodPost, "/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("plugin unreachable: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		if e.Error != "" {
			return fmt.Errorf("plugin returned %s: %s", resp.Status, e.Error)
		}
		return unexpectedStatus(resp)
	}
}

// unexpectedStatus describes a non-OK response. Redirects are never
// followed (see NewPluginStream), so point at where it wanted to go: the
// usual cause is entering http:// for a proxy that redirects to https://.
func unexpectedStatus(resp *http.Response) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			return fmt.Errorf("the URL redirects to %s (%s); redirects aren't followed, "+
				"so add the server with the final URL instead", loc, resp.Status)
		}
	}
	return fmt.Errorf("plugin returned %s", resp.Status)
}

// Status returns the current connection state.
func (p *PluginStream) Status() StreamStatus {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	st := StreamStatus{Connected: p.connected.Load(), LastError: p.lastError}
	if ms := p.lastEvent.Load(); ms > 0 {
		st.LastEvent = time.UnixMilli(ms)
	}
	return st
}

func (p *PluginStream) setError(err error) {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	if err == nil {
		p.lastError = ""
	} else {
		p.lastError = err.Error()
	}
}

// Start streams events into lineCh until ctx is cancelled, reconnecting with
// backoff. On reconnect it sends Last-Event-ID so the plugin replays events
// missed while the connection was down. Always call in a goroutine.
func (p *PluginStream) Start(ctx context.Context, lineCh chan<- ChatLine) {
	p.log.Info().Str("url", p.baseURL+"/events").Msg("Plugin event stream starting")
	defer p.log.Info().Msg("Plugin event stream stopped")

	var lastID string
	backoff := p.minBackoff
	for {
		connected, err := p.stream(ctx, lineCh, &lastID)
		p.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		p.setError(err)
		if connected {
			backoff = p.minBackoff
		}
		p.log.Warn().Err(err).Dur("retry_in", backoff).
			Msg("Plugin event stream disconnected, reconnecting")
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if !connected && backoff < p.maxBackoff {
			backoff = min(backoff*2, p.maxBackoff)
		}
	}
}

// stream runs one connection. connected reports whether the plugin accepted
// the subscription, so the caller can reset its backoff.
func (p *PluginStream) stream(ctx context.Context, lineCh chan<- ChatLine,
	lastID *string) (connected bool, err error) {

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := p.newRequest(connCtx, http.MethodGet, "/events", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if *lastID != "" {
		req.Header.Set("Last-Event-ID", *lastID)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return false, ErrUnauthorized
	} else if resp.StatusCode != http.StatusOK {
		return false, unexpectedStatus(resp)
	}
	p.connected.Store(true)
	p.setError(nil)
	p.log.Info().Str("last_event_id", *lastID).Msg("Plugin event stream connected")

	idle := time.AfterFunc(p.idleTimeout, cancel)
	defer idle.Stop()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var id string
	var data strings.Builder
	for scanner.Scan() {
		idle.Reset(p.idleTimeout)
		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				p.dispatch(ctx, lineCh, data.String())
				if id != "" {
					*lastID = id
				}
			}
			id = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
			// Comment / keepalive ping.
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil && connCtx.Err() != nil {
			return true, fmt.Errorf("no data from plugin for %s", p.idleTimeout)
		}
		return true, err
	}
	return true, fmt.Errorf("stream closed by plugin")
}

func (p *PluginStream) dispatch(ctx context.Context, lineCh chan<- ChatLine, raw string) {
	var ev pluginEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		p.log.Warn().Err(err).Str("data", raw).Msg("Invalid event from plugin")
		return
	}
	cl, ok := ev.toChatLine()
	if !ok {
		p.log.Debug().Str("type", ev.Type).Msg("Ignoring unknown plugin event type")
		return
	}
	cl.ServerID = p.serverID
	p.lastEvent.Store(time.Now().UnixMilli())
	p.log.Info().
		Str("player", cl.PlayerName).
		Int("event", int(cl.Event)).
		Str("text", cl.Message).
		Msg("Plugin event received")
	select {
	case lineCh <- cl:
	case <-ctx.Done():
	}
}

// toChatLine maps a plugin event onto a ChatLine. For non-chat events the
// plugin sends the full broadcast text ("Sverre was slain by Zombie"); the
// player name is split off when it leads. Custom join messages that don't
// start with the name are kept whole in FullText.
func (ev pluginEvent) toChatLine() (ChatLine, bool) {
	cl := ChatLine{PlayerName: ev.Player, Message: ev.Message}
	if ev.TS > 0 {
		cl.Timestamp = time.UnixMilli(ev.TS)
	} else {
		cl.Timestamp = time.Now()
	}
	switch ev.Type {
	case "chat":
		cl.Event = EventChat
		return cl, ev.Player != "" && ev.Message != ""
	case "join":
		cl.Event = EventJoin
	case "leave":
		cl.Event = EventLeave
	case "death":
		cl.Event = EventDeath
	case "advancement":
		cl.Event = EventAdvancement
	default:
		return cl, false
	}
	if ev.Player == "" || ev.Message == "" {
		return cl, false
	}
	if rest, ok := strings.CutPrefix(ev.Message, ev.Player+" "); ok {
		cl.Message = rest
	} else {
		cl.FullText = ev.Message
	}
	return cl, true
}
