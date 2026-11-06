package embedlocal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/internal/embedassets"
)

// Status describes shared local files and Ollama independently of app settings.
type Status struct {
	OllamaAvailable bool
	// Installing means an installer holds the lock and has staged approved work.
	Installing bool
	// DownloadBytes estimates missing downloads. Zero means all selected files are
	// present by metadata; Install still verifies model contents before reuse.
	DownloadBytes int64
}

// Inspect checks the selected model without waiting for an installer or downloading.
// For text, it probes local Ollama for at most 500 milliseconds unless SkipOllama
// is set. The snapshot is advisory; another installer can publish files later.
// DataDir defaults to DefaultDir. Executable is ignored.
func Inspect(ctx context.Context, opts Options) (Status, error) {
	return inspect(ctx, opts, "http://127.0.0.1:11434")
}

func inspect(ctx context.Context, opts Options, ollama string) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	dir := opts.DataDir
	var err error
	if dir == "" {
		dir, err = DefaultDir()
		if err != nil {
			return Status{}, err
		}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return Status{}, err
	}
	modelDir, assets, err := embedassets.SelectedModel(dir, opts.Model)
	if err != nil {
		return Status{}, err
	}
	held, err := (daemon.RuntimeStore{Dir: dir, Prefix: installLockPrefix}).StartLockHeld(ctx)
	if err != nil {
		return Status{}, err
	}
	var status Status
	if held {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Status{}, err
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), installStagePrefix) {
				status.Installing = true
				break
			}
		}
	}
	_, ready, err := selectBundle(dir, opts.Model)
	if err != nil {
		return Status{}, err
	}
	if !ready {
		status.DownloadBytes += bundleDownloadBytes
	}
	missing, err := embedassets.MissingModel(modelDir, assets, false)
	if err != nil {
		return Status{}, err
	}
	for _, asset := range missing {
		status.DownloadBytes += asset.Size
	}
	if (opts.Model == "" || opts.Model == TextModel) && !opts.SkipOllama {
		status.OllamaAvailable = probeOllama(ctx, ollama)
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	return status, nil
}
