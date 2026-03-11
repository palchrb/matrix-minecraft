package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorcon/rcon"
	"github.com/rs/zerolog"
)

// RCONClient is a thread-safe wrapper around gorcon/rcon.
// gorcon/rcon is NOT thread-safe — always use the mutex.
type RCONClient struct {
	host         string
	port         int
	password     string
	conn         *rcon.Conn
	mu           sync.Mutex
	log          zerolog.Logger
	prefixText   string
	prefixColor  string
	senderColor  string
	messageColor string
}

func NewRCONClient(host string, port int, password string,
	cfg Config, log zerolog.Logger) *RCONClient {
	return &RCONClient{
		host:         host,
		port:         port,
		password:     password,
		log:          log.With().Str("rcon_host", host).Logger(),
		prefixText:   cfg.PrefixText,
		prefixColor:  cfg.PrefixColor,
		senderColor:  cfg.SenderColor,
		messageColor: cfg.MessageColor,
	}
}

// Connect connects to RCON with exponential backoff (max 10 attempts).
func (r *RCONClient) Connect(ctx context.Context) error {
	backoff := 2 * time.Second
	addr := fmt.Sprintf("%s:%d", r.host, r.port)
	for attempt := 1; attempt <= 10; attempt++ {
		r.log.Info().Int("attempt", attempt).Str("addr", addr).
			Msg("Connecting to RCON")
		conn, err := rcon.Dial(addr, r.password)
		if err == nil {
			r.mu.Lock()
			r.conn = conn
			r.mu.Unlock()
			r.log.Info().Msg("RCON connected")
			return nil
		}
		r.log.Warn().Err(err).Dur("retry_in", backoff).Msg("RCON failed")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
	}
	return fmt.Errorf("failed to connect to RCON after 10 attempts")
}

func (r *RCONClient) Disconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
}

func (r *RCONClient) IsConnected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn != nil
}

func (r *RCONClient) execute(cmd string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return "", fmt.Errorf("not connected to RCON")
	}
	resp, err := r.conn.Execute(cmd)
	if err != nil {
		r.conn.Close()
		r.conn = nil
		return "", fmt.Errorf("RCON execute failed: %w", err)
	}
	return resp, nil
}

type tellrawPart struct {
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
	Bold  bool   `json:"bold,omitempty"`
}

// List returns a list of online players via the RCON "list" command.
// The format is typically: "There are X of a max of Y players online: player1, player2"
func (r *RCONClient) List() ([]string, error) {
	resp, err := r.execute("list")
	if err != nil {
		return nil, err
	}
	return parseListResponse(resp), nil
}

// parseListResponse parses the response from the "list" command.
// Formats: "There are X of a max of Y players online: p1, p2" or just "...online:"
func parseListResponse(resp string) []string {
	// Find after ":"
	idx := -1
	for i, c := range resp {
		if c == ':' {
			idx = i
			break
		}
	}
	if idx < 0 || idx+2 > len(resp) {
		return nil
	}
	playersStr := resp[idx+2:] // skip ": "
	if playersStr == "" {
		return nil
	}
	var players []string
	for _, p := range splitAndTrim(playersStr) {
		if p != "" {
			players = append(players, p)
		}
	}
	return players
}

func splitAndTrim(s string) []string {
	var result []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			p := s[start:i]
			// Trim spaces
			for len(p) > 0 && p[0] == ' ' {
				p = p[1:]
			}
			for len(p) > 0 && p[len(p)-1] == ' ' {
				p = p[:len(p)-1]
			}
			if p != "" {
				result = append(result, p)
			}
			start = i + 1
		}
	}
	return result
}

// SendMessage sends a chat message from Matrix to Minecraft via tellraw.
// Uses json.Marshal for correct escaping of special characters in message text.
func (r *RCONClient) SendMessage(ctx context.Context, senderName, message string) error {
	parts := []any{
		"",
		tellrawPart{Text: r.prefixText + " ", Color: r.prefixColor, Bold: true},
		tellrawPart{Text: "<" + senderName + "> ", Color: r.senderColor},
		tellrawPart{Text: message, Color: r.messageColor},
	}
	jsonBytes, err := json.Marshal(parts)
	if err != nil {
		return fmt.Errorf("tellraw serialization failed: %w", err)
	}
	_, err = r.execute("tellraw @a " + string(jsonBytes))
	return err
}
