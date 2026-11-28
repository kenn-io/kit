//go:build ignore

// Stage test assets explicitly. The installed server never downloads models.
package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/kit/internal/embedassets"
)

func main() {
	if err := stage(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func stage() error {
	dir := os.Getenv("KIT_EMBED_TEST_DIR")
	if dir == "" {
		return fmt.Errorf("KIT_EMBED_TEST_DIR is required")
	}
	base := embedassets.DownloadClient()
	defer base.CloseIdleConnections()
	client := *base
	client.Transport = fixtureTransport{base.Transport}
	for _, model := range []string{embedassets.TextModel, embedassets.FaceModel} {
		path, assets, err := embedassets.SelectedModel(dir, model)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		for _, asset := range assets {
			if err := embedassets.CacheAsset(context.Background(), &client, path, asset); err != nil {
				return fmt.Errorf("stage %s: %w", asset.Name, err)
			}
		}
	}
	manifest, err := os.Open("embedserver/testdata/faces/sources.tsv")
	if err != nil {
		return err
	}
	defer manifest.Close()
	reader := csv.NewReader(manifest)
	reader.Comma = '\t'
	reader.Comment = '#'
	rows, err := reader.ReadAll()
	if err != nil {
		return err
	}
	photos := filepath.Join(dir, "faces")
	if err := os.MkdirAll(photos, 0o700); err != nil {
		return err
	}
	for _, row := range rows {
		size, err := strconv.ParseInt(row[3], 10, 64)
		if err != nil {
			return err
		}
		asset := embedassets.Asset{Name: row[0], URL: row[1], Digest: row[2], Size: size}
		if err := embedassets.CacheAsset(context.Background(), &client, photos, asset); err != nil {
			return fmt.Errorf("stage %s: %w", asset.Name, err)
		}
	}

	return nil
}

// Wikimedia requires an identifying agent for automated fixture downloads.
// Hugging Face and Wikimedia also answer bursts of CI downloads with 429 and
// transient 5xx responses, so each request waits those out on its own.
type fixtureTransport struct{ http.RoundTripper }

func (t fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = 5 * time.Second
	policy.MaxInterval = time.Minute
	return backoff.Retry(req.Context(), func() (*http.Response, error) {
		attempt := req.Clone(req.Context())
		attempt.Header.Set("User-Agent", "kit-embedserver-tests/1.0 (+https://github.com/kenn-io/kit)")
		res, err := t.RoundTripper.RoundTrip(attempt)
		if err != nil {
			return nil, backoff.Permanent(err)
		}
		if res.StatusCode != http.StatusTooManyRequests && res.StatusCode < http.StatusInternalServerError {
			return res, nil
		}
		err = fmt.Errorf("GET %s: HTTP %d", req.URL.Redacted(), res.StatusCode)
		wait, waitOK := retryAfter(res.Header.Get("Retry-After"))
		if closeErr := res.Body.Close(); closeErr != nil {
			return nil, backoff.Permanent(closeErr)
		}
		if waitOK {
			return nil, backoff.RetryAfter(wait, err)
		}
		return nil, err
	},
		backoff.WithBackOff(policy),
		backoff.WithMaxTries(5),
		backoff.WithMaxElapsedTime(3*time.Minute),
		backoff.WithNotify(func(err error, wait time.Duration) {
			fmt.Fprintf(os.Stderr, "%v; retrying in %s\n", err, wait.Round(time.Second))
		}),
	)
}

// retryAfter reads a Retry-After header in either of its HTTP forms.
func retryAfter(header string) (time.Duration, bool) {
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(header); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}
