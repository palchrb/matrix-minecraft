package connector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// eventSource is anything that produces ChatLines for one server: the
// companion plugin's event stream, or the Docker log tailer as a fallback.
type eventSource interface {
	Start(ctx context.Context, lineCh chan<- ChatLine)
}

// pluginEvent is one JSON event from the companion plugin's /events stream.
type pluginEvent struct {
	Type    string `json:"type"`
	Player  string `json:"player"`
	Message string `json:"message"`
	TS      int64  `json:"ts"`
}

// PluginStream subscribes to the companion plugin's Server-Sent Events
// endpoint (http://<container>:<port>/events). Unlike log tailing it gets
// structured events straight from the server API, so console format changes
// can't break it.
type PluginStream struct {
	baseURL       string
	token         string
	containerName string
	client        *http.Client
	log           zerolog.Logger

	// idleTimeout drops the connection if nothing (not even a ping) arrives
	// for this long. The plugin pings every 15 seconds.
	idleTimeout time.Duration
	minBackoff  time.Duration
	maxBackoff  time.Duration
}

func NewPluginStream(host string, port int, token, containerName string,
	log zerolog.Logger) *PluginStream {
	return &PluginStream{
		baseURL:       fmt.Sprintf("http://%s:%d", host, port),
		token:         token,
		containerName: containerName,
		client: &http.Client{
			Transport: &http.Transport{
				// The plugin lives on the Docker network; never route it
				// through an HTTP(S)_PROXY from the environment.
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		log:         log.With().Str("container", containerName).Str("source", "plugin").Logger(),
		idleTimeout: 45 * time.Second,
		minBackoff:  2 * time.Second,
		maxBackoff:  30 * time.Second,
	}
}

// Probe checks whether the plugin is installed and accepts our token.
func (p *PluginStream) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plugin health check returned %s", resp.Status)
	}
	return nil
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
		if ctx.Err() != nil {
			return
		}
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

	req, err := http.NewRequestWithContext(connCtx, http.MethodGet, p.baseURL+"/events", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "text/event-stream")
	if *lastID != "" {
		req.Header.Set("Last-Event-ID", *lastID)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("plugin returned %s", resp.Status)
	}
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
	cl.ContainerName = p.containerName
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

// toChatLine maps a plugin event onto the same ChatLine the log tailer
// produces. For non-chat events the plugin sends the full broadcast text
// ("Sverre was slain by Zombie"); the player name is split off when it
// leads, so the Matrix side renders it the same way as before. Custom join
// messages that don't start with the name are kept whole in FullText.
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
