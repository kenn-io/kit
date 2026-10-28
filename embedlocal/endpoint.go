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
	"time"

	"go.kenn.io/kit/daemon"
)

// Service identifies the shared embeddings server and its runtime records.
const Service = "kit-embed"

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
// Startup waits at most two minutes. A started server outlives ctx.
func Endpoint(ctx context.Context, opts Options) (string, error) {
	return endpoint(ctx, opts, "http://127.0.0.1:11434")
}

func endpoint(ctx context.Context, opts Options, ollama string) (string, error) {
	if probeOllama(ctx, ollama) {
		return ollama + "/v1", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
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
		Compatible: func(rec daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return rec.Service == Service && rec.Version == "1" && info.Version == "1"
		},
	}
	if rec, _, ok, err := manager.Find(ctx); err != nil {
		return "", err
	} else if ok {
		return rec.Endpoint().BaseURL() + "/v1", nil
	}
	if opts.Executable == "" {
		return "", ErrNotFound
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	manager.Start = func(ctx context.Context) error {
		return daemon.StartDetached(ctx, daemon.StartDetachedOptions{
			Executable: opts.Executable,
			Args:       []string{"-data-dir", opts.DataDir, "-listen", "127.0.0.1:0"},
			Exited: func(err error) {
				if err == nil {
					err = errors.New("server exited before readiness")
				}
				cancel(fmt.Errorf("embeddings server exited: %w", err))
			},
		})
	}
	rec, _, err := manager.Ensure(ctx, 2*time.Minute)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return "", cause
		}
		return "", err
	}
	return rec.Endpoint().BaseURL() + "/v1", nil
}

func probeOllama(ctx context.Context, origin string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/version", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var version struct {
		Version string `json:"version"`
	}
	return json.UnmarshalRead(io.LimitReader(resp.Body, 4096), &version) == nil && version.Version != ""
}
