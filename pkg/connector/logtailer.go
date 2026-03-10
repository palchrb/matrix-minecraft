package connector

import (
	"bufio"
	"context"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	dockerclient "github.com/docker/docker/client"
	"github.com/rs/zerolog"
)

// chatRegex matches Minecraft server chat lines in various log formats:
//   - Vanilla:    [HH:MM:SS] [Server thread/INFO]: <PlayerName> message
//   - Paper/etc:  [HH:MM:SS INFO]: <PlayerName> message
//   - Not Secure: [HH:MM:SS INFO]: [Not Secure] <PlayerName> message
//   - Floodgate:  player names may start with '.' (e.g. .palchrb)
var chatRegex = regexp.MustCompile(
	`^\[\d{2}:\d{2}:\d{2}[^\]]*\](?::| \[[^\]]+\]:) (?:\[Not Secure\] )?<([A-Za-z0-9_.]{1,16})> (.+)$`,
)

// joinLeaveRegex matches join/leave messages:
//   [HH:MM:SS] [Server thread/INFO]: PlayerName joined the game
//   [HH:MM:SS] [Server thread/INFO]: PlayerName left the game
var joinLeaveRegex = regexp.MustCompile(
	`^\[\d{2}:\d{2}:\d{2}[^\]]*\](?::| \[[^\]]+\]:) ([A-Za-z0-9_.]{1,16}) (joined the game|left the game)$`,
)

// deathRegex matches death messages. Minecraft death messages always start with the player name
// followed by a death message verb (was slain, was shot, drowned, fell, etc.)
// Captures: [1] player name, [2] full death message after player name
var deathRegex = regexp.MustCompile(
	`^\[\d{2}:\d{2}:\d{2}[^\]]*\](?::| \[[^\]]+\]:) ([A-Za-z0-9_.]{1,16}) ((?:was slain|was shot|was killed|was fireballed|was pummeled|was squished|was squashed|drowned|blew up|was blown up|was burnt|went up in flames|walked into fire|burned to death|tried to swim in lava|suffocated in a wall|starved to death|fell from|fell off|fell out of|hit the ground|was doomed to fall|was struck by lightning|froze to death|was impaled|was stung to death|was obliterated|discovered the floor was lava|didn't want to live|experienced kinetic energy|withered away|died|was poked to death|was killed by).+)$`,
)

// advancementRegex matches advancement/achievement messages:
//   [HH:MM:SS] [Server thread/INFO]: PlayerName has made the advancement [Advancement Name]
//   [HH:MM:SS] [Server thread/INFO]: PlayerName has completed the challenge [Challenge Name]
//   [HH:MM:SS] [Server thread/INFO]: PlayerName has reached the goal [Goal Name]
var advancementRegex = regexp.MustCompile(
	`^\[\d{2}:\d{2}:\d{2}[^\]]*\](?::| \[[^\]]+\]:) ([A-Za-z0-9_.]{1,16}) (has (?:made the advancement|completed the challenge|reached the goal) \[.+\])$`,
)

// EventType angir hvilken type hendelse en ChatLine representerer.
type EventType int

const (
	EventChat        EventType = iota // Vanlig chat-melding
	EventJoin                         // Spiller logget inn
	EventLeave                        // Spiller logget ut
	EventDeath                        // Spiller døde
	EventAdvancement                  // Spiller fikk en advancement
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

// Start leser Docker-logg og sender ChatLine på lineCh.
// Kjører til ctx kanselleres. Har intern retry-logikk.
// Kall alltid i goroutine.
func (t *LogTailer) Start(ctx context.Context, lineCh chan<- ChatLine) {
	backoff := 2 * time.Second
	for {
		if err := t.tail(ctx, lineCh); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.log.Warn().Err(err).Dur("retry_in", backoff).
				Msg("Logg-tailing feilet, prøver igjen")
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
	reader, err := t.docker.ContainerLogs(ctx, t.containerName,
		container.LogsOptions{
			ShowStdout: true,
			ShowStderr: false,
			Follow:     true,
			Tail:       "0",
		})
	if err != nil {
		return err
	}
	defer reader.Close()

	// Check if the container uses TTY mode. With TTY enabled, Docker sends
	// raw output without multiplexing headers, so stdcopy must be skipped.
	var logReader io.Reader
	info, inspectErr := t.docker.ContainerInspect(ctx, t.containerName)
	if inspectErr != nil || !info.Config.Tty {
		// Non-TTY: Docker multiplexes stdout/stderr with 8-byte binary headers.
		pr, pw := io.Pipe()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer pw.Close()
			if _, err := stdcopy.StdCopy(pw, io.Discard, reader); err != nil {
				if ctx.Err() == nil {
					t.log.Warn().Err(err).Msg("stdcopy avsluttet")
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
	for scanner.Scan() {
		line := scanner.Text()
		var cl *ChatLine

		if m := chatRegex.FindStringSubmatch(line); m != nil {
			cl = &ChatLine{
				PlayerName: m[1],
				Message:    m[2],
				Event:      EventChat,
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
			cl.Timestamp = time.Now()
			cl.ContainerName = t.containerName
			t.log.Trace().
				Str("player", cl.PlayerName).
				Int("event", int(cl.Event)).
				Str("message", cl.Message).
				Msg("Matchet logg-linje")
			select {
			case lineCh <- *cl:
			case <-ctx.Done():
				return nil
			}
		} else if len(line) > 0 {
			t.log.Trace().Str("line", line).Msg("Umatchet logg-linje")
		}
	}

	return scanner.Err()
}
