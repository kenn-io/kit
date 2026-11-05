package embedlocal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/internal/embedassets"
)

// Status describes shared local files and Ollama independently of app settings.
type Status struct {
	OllamaAvailable bool
	// Installing means an installer holds the lock, including during confirmation.
	Installing bool
	// DownloadBytes estimates missing downloads. Zero means all pinned files are
	// present by metadata; Install still verifies model contents before reuse.
	DownloadBytes int64
}

// Inspect checks shared files without waiting for an installer or downloading.
// It probes only local Ollama, for at most 500 milliseconds, unless SkipOllama
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
	release, acquired, err := (daemon.RuntimeStore{Dir: dir, Prefix: installLockPrefix}).TryAcquireStartLock(ctx)
	if err != nil {
		return Status{}, err
	}
	status := Status{Installing: !acquired}
	err = func() error {
		if acquired {
			defer release()
		}
		if err := checkBundle(filepath.Join(dir, "bin", embedassets.BundleTag)); err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errIncompleteBundle) {
				return err
			}
			status.DownloadBytes += bundleDownloadBytes
		}
		missing, err := embedassets.MissingModel(embedassets.ModelDir(dir), embedassets.ModelAssets(), false)
		if err != nil {
			return err
		}
		for _, asset := range missing {
			status.DownloadBytes += asset.Size
		}
		return nil
	}()
	if err != nil {
		return Status{}, err
	}
	if !opts.SkipOllama {
		probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		status.OllamaAvailable = probeOllama(probe, ollama)
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	return status, nil
}
