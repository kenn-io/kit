package embedlocal

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/daemon"
)

// Service identifies the shared embeddings server and its runtime records.
const Service = "kit-embed"

// Version identifies the server runtime protocol.
const Version = "1"

// ErrNotFound means no local server was found and no executable was supplied.
var ErrNotFound = errors.New("local embeddings server not found")

// Options configures discovery and optional startup. DataDir defaults to
// DefaultDir. Executable is the installed kennembed binary; leaving it empty
// permits discovery without starting a server.
type Options struct{ DataDir, Executable string }

// DefaultDir returns the shared per-user embeddings data directory.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kenn", "embeddings"), nil
}

// Endpoint returns a /v1 base URL, preferring a running Ollama to kennembed.
// Startup follows ctx's deadline, or waits at most two minutes without one.
// A started server continues loading after ctx is canceled.
func Endpoint(ctx context.Context, opts Options) (string, error) {
	return endpoint(ctx, opts, "http://127.0.0.1:11434", 2*time.Minute)
}

func endpoint(ctx context.Context, opts Options, ollama string, timeout time.Duration) (string, error) {
	if probeOllama(ctx, ollama) {
		return ollama + "/v1", nil
	}
	var err error
	if opts.DataDir == "" {
		opts.DataDir, err = DefaultDir()
		if err != nil {
			return "", err
		}
	}
	opts.DataDir, err = filepath.Abs(opts.DataDir)
	if err != nil {
		return "", err
	}
	manager := daemon.Manager{
		Store: daemon.RuntimeStore{Dir: opts.DataDir, Prefix: Service},
		Discover: daemon.DiscoverOptions{
			RequirePIDAlive: true,
			Probe:           daemon.ProbeOptions{ExpectedService: Service},
		},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == Version
		},
	}
	if opts.Executable == "" {
		rec, _, ok, err := manager.Find(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrNotFound
		}
		return rec.Endpoint().BaseURL() + "/v1", nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	path := filepath.Join(opts.DataDir, "kennembed.log")
	discovery := manager
	joining := false
	exited := make(chan error, 1)
	manager.FindFunc = func(ctx context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
		rec, info, ok, err := discovery.Find(ctx)
		if err == nil && !ok {
			held, probeErr := ownerHeld(ctx, manager.Store)
			if probeErr != nil {
				err = probeErr
			} else if held {
				joining = true
			} else if err = startupFailure(path, joining, exited); err != nil {
				cancel(err)
			}
		}
		return rec, info, ok, err
	}
	manager.Start = func(ctx context.Context) error {
		if joining {
			return nil
		}
		log, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("open embeddings log: %w", err)
		}
		err = daemon.StartDetached(ctx, daemon.StartDetachedOptions{
			Executable: opts.Executable,
			Args:       []string{"-data-dir", opts.DataDir, "-listen", "127.0.0.1:0"},
			Stderr:     log,
			Exited:     func(err error) { exited <- err },
		})
		return errors.Join(err, log.Close())
	}
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	rec, _, err := manager.Ensure(ctx, timeout)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = startupError(path, fmt.Errorf("waiting for embeddings server readiness: %w", err))
		}
		return "", err
	}
	return rec.Endpoint().BaseURL() + "/v1", nil
}

func probeOllama(ctx context.Context, origin string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/tags", nil)
	if err != nil {
		return false
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), &tags) != nil {
		return false
	}
	for _, model := range tags.Models {
		if model.Name == "nomic-embed-text:latest" {
			return true
		}
	}
	return false
}

func ownerHeld(ctx context.Context, store daemon.RuntimeStore) (bool, error) {
	probe, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	release, err := store.AcquireOwnerLock(probe)
	if err == nil {
		release()
		return false, nil
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return true, nil
	}
	return false, err
}

func startupFailure(path string, joining bool, exited <-chan error) error {
	select {
	case err := <-exited:
		if err == nil {
			err = errors.New("server exited before readiness")
		}
		return startupError(path, fmt.Errorf("embeddings server exited: %w", err))
	default:
	}
	if joining {
		return startupError(path, errors.New("embeddings startup owner released before readiness"))
	}
	return nil
}

func startupError(path string, cause error) error {
	failure := fmt.Errorf("%w; log %s", cause, path)
	log, err := os.Open(path)
	if err != nil {
		return errors.Join(failure, err)
	}
	defer log.Close()
	info, err := log.Stat()
	if err != nil {
		return errors.Join(failure, err)
	}
	tail := make([]byte, min(info.Size(), 2048))
	n, err := log.ReadAt(tail, max(0, info.Size()-2048))
	if err != nil && !errors.Is(err, io.EOF) {
		return errors.Join(failure, err)
	}
	return fmt.Errorf("%w:\n%s", failure, strings.TrimSpace(string(tail[:n])))
}
