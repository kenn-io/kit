// Package embedassets owns verified embedding downloads and the pinned model cache.
package embedassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

const revision = "e9b6763023c676ca8431644204f50c2b100d9aab"

// Asset describes a download with an exact size and SHA-256 digest.
type Asset struct {
	Name, URL, Digest string
	Size              int64
}

var modelAssets = []Asset{
	{"model.onnx", "onnx/model_fp16.onnx", "cf5b5a86edb00f895561803cfc04729090a958340b8ca2ad76c143f565f6bb04", 273859028},
	{"tokenizer.json", "tokenizer.json", "d241a60d5e8f04cc1b2b3e9ef7a4921b27bf526d9f6050ab90f9267a1f9e5c66", 711396},
}

// CacheModel fetches missing or corrupt pinned files and returns their directory.
func CacheModel(ctx context.Context, dir string) (string, error) {
	dir = filepath.Join(dir, "nomic-embed-text-v1.5", revision)
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	client := &http.Client{}
	if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
		transport := defaults.Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		defer transport.CloseIdleConnections()
		client.Transport = transport
	}
	for _, a := range modelAssets {
		a.URL = "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/" + revision + "/" + a.URL
		if err := CacheAsset(ctx, client, dir, a); err != nil {
			return "", fmt.Errorf("cache %s: %w", a.Name, err)
		}
	}
	return dir, nil
}

func verify(r io.Reader, w io.Writer, a Asset) error {
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, hash), io.LimitReader(r, a.Size+1))
	if err != nil {
		return err
	}
	if n != a.Size || hex.EncodeToString(hash.Sum(nil)) != a.Digest {
		return errors.New("asset size or SHA-256 mismatch")
	}
	return nil
}

// CacheAsset reuses a verified file or atomically replaces it with a verified download.
func CacheAsset(ctx context.Context, client *http.Client, dir string, a Asset) (err error) {
	path := filepath.Join(dir, a.Name)
	cached, openErr := os.Open(path)
	if openErr == nil {
		valid := verify(cached, io.Discard, a)
		closeErr := cached.Close()
		if valid == nil {
			return closeErr
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return openErr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", res.StatusCode)
	}
	file, err := atomicfile.Create(path, atomicfile.WithPrivate())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Abort()) }()
	if err := verify(res.Body, file, a); err != nil {
		return err
	}
	return file.Commit()
}

// CachedModel checks the pinned files without creating files or using the network.
func CachedModel(dir string) error {
	dir = filepath.Join(dir, "nomic-embed-text-v1.5", revision)
	for _, a := range modelAssets {
		f, err := os.Open(filepath.Join(dir, a.Name))
		if err != nil {
			return err
		}
		err = verify(f, io.Discard, a)
		if err = errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
	return nil
}
