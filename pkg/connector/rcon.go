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

// RCONClient er thread-safe wrapper rundt gorcon/rcon.
// gorcon/rcon er IKKE thread-safe – bruk alltid mutex.
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

// Connect kobler til RCON med exponential backoff (maks 10 forsøk).
func (r *RCONClient) Connect(ctx context.Context) error {
	backoff := 2 * time.Second
	addr := fmt.Sprintf("%s:%d", r.host, r.port)
	for attempt := 1; attempt <= 10; attempt++ {
		r.log.Info().Int("attempt", attempt).Str("addr", addr).
			Msg("Kobler til RCON")
		conn, err := rcon.Dial(addr, r.password)
		if err == nil {
			r.mu.Lock()
			r.conn = conn
			r.mu.Unlock()
			r.log.Info().Msg("RCON tilkoblet")
			return nil
		}
		r.log.Warn().Err(err).Dur("retry_in", backoff).Msg("RCON feilet")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
	}
	return fmt.Errorf("kunne ikke koble til RCON etter 10 forsøk")
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
		return "", fmt.Errorf("ikke tilkoblet RCON")
	}
	resp, err := r.conn.Execute(cmd)
	if err != nil {
		r.conn.Close()
		r.conn = nil
		return "", fmt.Errorf("RCON execute feilet: %w", err)
	}
	return resp, nil
}

type tellrawPart struct {
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
	Bold  bool   `json:"bold,omitempty"`
}

// SendMessage sender chat-melding fra Matrix til Minecraft via tellraw.
// Bruker json.Marshal for korrekt escaping av spesialtegn i meldingstekst.
func (r *RCONClient) SendMessage(ctx context.Context, senderName, message string) error {
	parts := []any{
		"",
		tellrawPart{Text: r.prefixText + " ", Color: r.prefixColor, Bold: true},
		tellrawPart{Text: "<" + senderName + "> ", Color: r.senderColor},
		tellrawPart{Text: message, Color: r.messageColor},
	}
	jsonBytes, err := json.Marshal(parts)
	if err != nil {
		return fmt.Errorf("tellraw serialisering feilet: %w", err)
	}
	_, err = r.execute("tellraw @a " + string(jsonBytes))
	return err
}
