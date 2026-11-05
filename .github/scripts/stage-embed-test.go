//go:build ignore

// Stage test assets explicitly. The installed server never downloads models.
package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
	client := embedassets.DownloadClient()
	defer client.CloseIdleConnections()
	for _, model := range []string{embedassets.TextModel, embedassets.FaceModel} {
		path, assets, err := embedassets.SelectedModel(dir, model)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		for _, asset := range assets {
			if err := embedassets.CacheAsset(context.Background(), client, path, asset); err != nil {
				return err
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
		if err := embedassets.CacheAsset(context.Background(), client, photos, asset); err != nil {
			return err
		}
	}

	return nil
}
