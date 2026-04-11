package connector

import (
	"bufio"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	dockerclient "github.com/docker/docker/client"
	"github.com/rs/zerolog"
)

// ansiRegex strips ANSI escape codes (color codes, etc.) from log lines.
// Minecraft servers in TTY mode often send color codes that prevent regex
// from matching even though the text looks correct.
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\].*?\x07`)

// logPrefix matches the timestamp + thread tag at the beginning of every
// Minecraft server log line, covering the known formats:
//   - Vanilla:   [HH:MM:SS] [Server thread/INFO]:
//   - Paper:     [HH:MM:SS INFO]:
//   - Paper with extra fields: [HH:MM:SS INFO] [PluginName]:
// The [^\]]* inside the timestamp bracket tolerates "[16:31:38 INFO]" as
// well as "[16:31:38]".
const logPrefix = `\[\d{2}:\d{2}:\d{2}[^\]]*\](?::| \[[^\]]+\]:) `

// chatPrefix matches zero or more bracketed prefixes before the "<player>"
// token. Covers plain chat, "[Not Secure]" marker, and rank plugins like
// [Admin] or [VIP] [Staff] <player>.
const chatPrefix = `(?:\[[^\]]+\] )*`

// chatRegex matches Minecraft server chat lines in various log formats:
//   - Vanilla:    [HH:MM:SS] [Server thread/INFO]: <PlayerName> message
//   - Paper/etc:  [HH:MM:SS INFO]: <PlayerName> message
//   - Not Secure: [HH:MM:SS INFO]: [Not Secure] <PlayerName> message
//   - Rank plugins: [HH:MM:SS INFO]: [Admin] <PlayerName> message
//   - Floodgate:  player names may start with '.' (e.g. .palchrb)
var chatRegex = regexp.MustCompile(
	`^` + logPrefix + chatPrefix + `<([A-Za-z0-9_.]{1,16})> (.+)$`,
)

// joinLeaveRegex matches vanilla join/leave messages:
//
//	[HH:MM:SS] [Server thread/INFO]: PlayerName joined the game
//	[HH:MM:SS] [Server thread/INFO]: PlayerName left the game
var joinLeaveRegex = regexp.MustCompile(
	`^` + logPrefix + `([A-Za-z0-9_.]{1,16}) (joined the game|left the game)$`,
)

// deathRegex matches death messages. Minecraft death messages always start with the player name
// followed by a death message verb (was slain, was shot, drowned, fell, etc.)
// Captures: [1] player name, [2] full death message after player name
var deathRegex = regexp.MustCompile(
	`^` + logPrefix + `([A-Za-z0-9_.]{1,16}) ((?:was slain|was shot|was killed|was fireballed|was pummeled|was squished|was squashed|drowned|blew up|was blown up|was burnt|went up in flames|walked into fire|burned to death|tried to swim in lava|suffocated in a wall|starved to death|fell from|fell off|fell out of|hit the ground|was doomed to fall|was struck by lightning|froze to death|was impaled|was stung to death|was obliterated|discovered the floor was lava|didn't want to live|experienced kinetic energy|withered away|died|was poked to death|was killed by).+)$`,
)

// advancementRegex matches advancement/achievement messages:
//
//	[HH:MM:SS] [Server thread/INFO]: PlayerName has made the advancement [Advancement Name]
//	[HH:MM:SS] [Server thread/INFO]: PlayerName has completed the challenge [Challenge Name]
//	[HH:MM:SS] [Server thread/INFO]: PlayerName has reached the goal [Goal Name]
var advancementRegex = regexp.MustCompile(
	`^` + logPrefix + `([A-Za-z0-9_.]{1,16}) (has (?:made the advancement|completed the challenge|reached the goal) \[.+\])$`,
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

type ChatLine struct {
	PlayerName    string
	Message       string
	Timestamp     time.Time
	ContainerName string
	Event         EventType
}

type LogTailer struct {
	docker        *dockerclient.Client
	containerName string
	log           zerolog.Logger
}

func NewLogTailer(docker *dockerclient.Client, containerName string,
	log zerolog.Logger) *LogTailer {
	return &LogTailer{
		docker:        docker,
		containerName: containerName,
		log:           log.With().Str("container", containerName).Logger(),
	}
}

// Start reads Docker logs and sends ChatLines on lineCh.
// Runs until ctx is cancelled. Has internal retry logic.
// Always call in a goroutine.
func (t *LogTailer) Start(ctx context.Context, lineCh chan<- ChatLine) {
	t.log.Info().Msg("LogTailer goroutine starting")
	defer t.log.Info().Msg("LogTailer goroutine stopped")

	backoff := 2 * time.Second
	iteration := 0
	for {
		iteration++
		t.log.Info().Int("iteration", iteration).
			Msg("LogTailer.tail() entering")
		if err := t.tail(ctx, lineCh); err != nil {
			if ctx.Err() != nil {
				t.log.Info().Err(ctx.Err()).
					Msg("LogTailer ctx cancelled, exiting")
				return
			}
			t.log.Warn().Err(err).Dur("retry_in", backoff).
				Msg("Log tailing failed, retrying")
		} else if ctx.Err() != nil {
			t.log.Info().Msg("LogTailer ctx cancelled during tail, exiting")
			return
		} else {
			t.log.Warn().Dur("retry_in", backoff).
				Msg("LogTailer.tail() returned with no error (stream ended?), retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
	}
}

func (t *LogTailer) tail(ctx context.Context, lineCh chan<- ChatLine) error {
	t.log.Info().Msg("Opening Docker log stream")
	reader, err := t.docker.ContainerLogs(ctx, t.containerName,
		container.LogsOptions{
			ShowStdout: true,
			ShowStderr: false,
			Follow:     true,
			Tail:       "0",
		})
	if err != nil {
		t.log.Warn().Err(err).Msg("ContainerLogs failed")
		return err
	}
	defer reader.Close()

	// Check if the container uses TTY mode. With TTY enabled, Docker sends
	// raw output without multiplexing headers, so stdcopy must be skipped.
	var logReader io.Reader
	info, inspectErr := t.docker.ContainerInspect(ctx, t.containerName)
	tty := false
	if inspectErr == nil {
		tty = info.Config.Tty
	}
	t.log.Info().Bool("tty", tty).Err(inspectErr).
		Msg("Docker container TTY mode detected")

	if !tty {
		// Non-TTY: Docker multiplexes stdout/stderr with 8-byte binary headers.
		pr, pw := io.Pipe()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer pw.Close()
			if _, err := stdcopy.StdCopy(pw, io.Discard, reader); err != nil {
				if ctx.Err() == nil {
					t.log.Warn().Err(err).Msg("stdcopy ended")
				}
			}
		}()
		logReader = pr
		defer func() { wg.Wait() }()
	} else {
		// TTY mode: raw output, read directly.
		logReader = reader
	}

	scanner := bufio.NewScanner(logReader)
	// Increase buffer size for long log lines (default is 64KB which is
	// usually fine, but some MC servers output long advancement JSON).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var linesRead, linesMatched int
	lastHeartbeat := time.Now()
	t.log.Info().Msg("Starting log line scanner loop")
	for scanner.Scan() {
		linesRead++
		// Log the first few raw lines at INFO so we can confirm Docker is
		// actually streaming something, regardless of regex matching.
		if linesRead <= 3 {
			t.log.Info().Int("line_num", linesRead).
				Str("raw_line", scanner.Text()).
				Msg("LogTailer read raw Docker line")
		}
		// Periodic heartbeat so silent periods are visible.
		if time.Since(lastHeartbeat) > 2*time.Minute {
			t.log.Info().
				Int("lines_read", linesRead).
				Int("lines_matched", linesMatched).
				Msg("LogTailer heartbeat")
			lastHeartbeat = time.Now()
		}

		// Normalise the raw scanner output into something the regexes can
		// match reliably, regardless of how the MC container's console is
		// configured:
		//
		// 1. Strip ANSI color codes and CSI sequences (\x1b[...m, \x1b[K).
		// 2. Handle JLine-style interactive consoles (Paper/Spigot with the
		//    "> " prompt): those write the prompt, then on every new log
		//    line emit "\r<CSI>K<actual text>" so the terminal erases the
		//    prompt before rendering the line. bufio.Scanner reads bytes
		//    without simulating terminal rendering, so we get the prompt as
		//    a prefix followed by \r and the real text. Dropping everything
		//    up to the last \r mimics the terminal.
		// 3. Trim leading/trailing whitespace so stray spaces or tabs don't
		//    break the "^\[" anchor on the regexes.
		line := ansiRegex.ReplaceAllString(scanner.Text(), "")
		if idx := strings.LastIndex(line, "\r"); idx >= 0 {
			line = line[idx+1:]
		}
		line = strings.TrimSpace(line)

		var cl *ChatLine

		if m := chatRegex.FindStringSubmatch(line); m != nil {
			cl = &ChatLine{
				PlayerName: m[1],
				Message:    m[2],
				Event:      EventChat,
			}
		} else if m := joinLeaveRegex.FindStringSubmatch(line); m != nil {
			evt := EventJoin
			if m[2] == "left the game" {
				evt = EventLeave
			}
			cl = &ChatLine{
				PlayerName: m[1],
				Message:    m[2],
				Event:      evt,
			}
		} else if m := deathRegex.FindStringSubmatch(line); m != nil {
			cl = &ChatLine{
				PlayerName: m[1],
				Message:    m[2],
				Event:      EventDeath,
			}
		} else if m := advancementRegex.FindStringSubmatch(line); m != nil {
			cl = &ChatLine{
				PlayerName: m[1],
				Message:    m[2],
				Event:      EventAdvancement,
			}
		}

		if cl != nil {
			linesMatched++
			cl.Timestamp = time.Now()
			cl.ContainerName = t.containerName
			t.log.Info().
				Str("player", cl.PlayerName).
				Int("event", int(cl.Event)).
				Str("message", cl.Message).
				Msg("LogTailer matched line")
			select {
			case lineCh <- *cl:
			case <-ctx.Done():
				t.log.Info().Msg("ctx cancelled while queuing matched line")
				return nil
			}
		} else if len(line) > 0 {
			t.log.Debug().Str("line", line).Msg("Unmatched log line")
		}
	}

	scanErr := scanner.Err()
	t.log.Info().
		Int("lines_read", linesRead).
		Int("lines_matched", linesMatched).
		Err(scanErr).
		Msg("LogTailer scanner loop exited")
	return scanErr
}
