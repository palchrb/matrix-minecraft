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

var chatRegex = regexp.MustCompile(
	`^\[\d{2}:\d{2}:\d{2}\] \[[^\]]+/INFO\]: <([A-Za-z0-9_]{1,16})> (.+)$`,
)

type ChatLine struct {
	PlayerName    string
	Message       string
	Timestamp     time.Time
	ContainerName string
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

	// Docker multiplexer stdout/stderr med 8-bytes binær header per linje.
	// stdcopy.StdCopy demultiplexer dette korrekt.
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

	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		if m := chatRegex.FindStringSubmatch(scanner.Text()); m != nil {
			select {
			case lineCh <- ChatLine{
				PlayerName:    m[1],
				Message:       m[2],
				Timestamp:     time.Now(),
				ContainerName: t.containerName,
			}:
			case <-ctx.Done():
				return nil
			}
		}
	}

	wg.Wait()
	return scanner.Err()
}
