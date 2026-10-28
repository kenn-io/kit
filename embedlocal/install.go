// Package embedlocal installs a shared local embedding server without loading ONNX Runtime.
package embedlocal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/internal/embedassets"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kit/selfupdate"
)

// InstallOptions controls a first installation. Apps own confirmation and should
// share DataDir, conventionally ~/.kenn/embeddings, across callers.
type InstallOptions struct {
	DataDir string
	// Confirm runs before any network request and receives an approximate byte
	// count. A missing callback or declined confirmation leaves downloads unstarted.
	Confirm func(context.Context, int64) (bool, error)
}

// Installation contains absolute paths to pass to the server launcher.
type Installation struct {
	Executable string
	DataDir    string
}

// Install downloads the server, native runtime, and pinned model after consent.
// A completed installation is reused offline without asking again. Concurrent
// callers share one installation. The install lock is released before returning,
// so callers can immediately start the returned Executable with DataDir.
func Install(ctx context.Context, opts InstallOptions) (Installation, error) {
	client := selfupdate.Client{Owner: "kenn-io", Repo: "kit", BinaryName: "kennembed", AllowUnsignedChecksums: true}
	return install(ctx, opts, client, func(ctx context.Context, dir string, download bool) error {
		if !download {
			return embedassets.CachedModel(dir)
		}
		_, err := embedassets.CacheModel(ctx, dir)
		return err
	})
}

func install(ctx context.Context, opts InstallOptions, client selfupdate.Client, model func(context.Context, string, bool) error) (Installation, error) {
	if opts.DataDir == "" {
		return Installation{}, errors.New("install embeddings: DataDir is required")
	}
	dir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return Installation{}, err
	}
	executable := "kennembed"
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	result := Installation{filepath.Join(dir, "bin", executable), dir}
	complete := func() bool {
		return checkBundle(filepath.Join(dir, "bin"), executable) == nil && model(ctx, dir, false) == nil
	}
	if complete() {
		return result, nil
	}
	release, err := (daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed-install"}).AcquireStartLock(ctx)
	if err != nil {
		return Installation{}, err
	}
	defer release()
	if complete() {
		return result, nil
	}
	if opts.Confirm == nil {
		return Installation{}, errors.New("install embeddings: provide Confirm to approve the approximately 290 MB download")
	}
	approved, err := opts.Confirm(ctx, 290_000_000)
	if err != nil {
		return Installation{}, err
	}
	if !approved {
		return Installation{}, errors.New("embedding download declined; approve installation to use local embeddings")
	}
	info, err := client.Check(ctx, selfupdate.CheckOptions{Force: true})
	if err != nil {
		return Installation{}, err
	}
	if info == nil {
		return Installation{}, errors.New("no kennembed release is available")
	}
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return Installation{}, err
	}
	stage, err := os.MkdirTemp(dir, ".install-*")
	if err != nil {
		return Installation{}, err
	}
	defer os.RemoveAll(stage)
	downloadClient := client.HTTPClient
	if downloadClient == nil {
		downloadClient = &http.Client{}
	}
	securedClient := *downloadClient
	securedClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("kennembed download redirected to non-HTTPS URL")
		}
		if downloadClient.CheckRedirect != nil {
			return downloadClient.CheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("too many kennembed download redirects")
		}
		return nil
	}
	archive := filepath.Join(stage, "archive")
	err = embedassets.CacheAsset(ctx, &securedClient, stage, embedassets.Asset{Name: "archive", URL: info.DownloadURL, Digest: info.Checksum, Size: info.Size})
	if err != nil {
		return Installation{}, fmt.Errorf("download kennembed: %w", err)
	}
	bundle := filepath.Join(stage, "bin")
	if strings.HasSuffix(info.AssetName, ".zip") {
		err = selfupdate.ExtractZip(archive, bundle)
	} else {
		err = selfupdate.ExtractTarGz(archive, bundle)
	}
	if err != nil {
		return Installation{}, err
	}
	if err := os.Chmod(filepath.Join(bundle, executable), 0o755); err != nil {
		return Installation{}, err
	}
	if err := checkBundle(bundle, executable); err != nil {
		return Installation{}, err
	}
	if err := model(ctx, dir, true); err != nil {
		return Installation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Installation{}, err
	}
	// A valid bundle may remain after an interrupted model download.
	if err := checkBundle(filepath.Join(dir, "bin"), executable); err == nil {
		return result, nil
	}
	if err := atomicfile.RenameNoReplace(bundle, filepath.Join(dir, "bin")); err != nil {
		return Installation{}, fmt.Errorf("publish kennembed: %w", err)
	}
	return result, nil
}

func checkBundle(dir, executable string) error {
	library := "libonnxruntime.so.1.23.2"
	if runtime.GOOS == "darwin" {
		library = "libonnxruntime.1.23.2.dylib"
	}
	if runtime.GOOS == "windows" {
		library = "onnxruntime.dll"
	}
	names := []string{executable, library}
	if runtime.GOOS == "windows" {
		names = append(names, "msvcp140.dll", "msvcp140_1.dll", "vcruntime140.dll", "vcruntime140_1.dll")
	}
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("incomplete kennembed bundle: %s", name)
		}
		if name == executable && runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("kennembed is not executable: %s", name)
		}
	}
	return nil
}
