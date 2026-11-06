package embedserver

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/internal/embedassets"
)

func TestMissingRuntime(t *testing.T) {
	for _, tc := range []struct {
		name string
		load func(context.Context, string, string) (engine, error)
	}{{"text", loadModel}, {"server", loadModels}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.load(t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "missing-library"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestModelsReportInstallation(t *testing.T) {
	var preview bytes.Buffer
	require.NoError(t, png.Encode(&preview, image.NewRGBA(image.Rect(0, 0, 2, 1))))
	for _, scenario := range []struct {
		name  string
		faces bool
	}{
		{name: "text"},
		{name: "faces", faces: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			library := filepath.Join(root, "runtime")
			require.NoError(t, os.WriteFile(library, []byte("unused until models are installed"), 0o600))
			e, err := loadModels(t.Context(), root, library)
			require.NoError(t, err)
			defer func() { require.NoError(t, e.Close()) }()
			var log bytes.Buffer
			h := &handler{engine: e, gate: make(chan struct{}, 1), previews: make(chan struct{}, 1), log: &log}
			recorder := httptest.NewRecorder()
			if scenario.faces {
				body, err := json.Marshal(embedclient.FaceRequest{Image: preview.Bytes(), Boxes: []embedclient.FaceBox{{Width: 1, Height: 1}}})
				require.NoError(t, err)
				h.faces(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/faces", bytes.NewReader(body)))
			} else {
				h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"hello"}`)))
			}
			require.Equal(t, http.StatusFailedDependency, recorder.Code)
			require.Contains(t, recorder.Body.String(), "install ")
			require.Contains(t, recorder.Body.String(), "missing or corrupt")
		})
	}
}

func TestModelsDiscoverCompleteFiles(t *testing.T) {
	root := t.TempDir()
	library := filepath.Join(root, "runtime")
	require.NoError(t, os.WriteFile(library, []byte("unused until models are installed"), 0o600))
	e, err := loadModels(t.Context(), root, library)
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	var log bytes.Buffer
	h := &handler{engine: e, gate: make(chan struct{}, 1), previews: make(chan struct{}, 1), log: &log}
	for _, tc := range []struct {
		model, response string
	}{
		{"", `{"models":[]}`},
		{embedassets.TextModel, `{"models":["nomic-embed-text"]}`},
		{embedassets.FaceModel, `{"models":["nomic-embed-text","auraface-v1"]}`},
	} {
		if tc.model != "" {
			// Discovery checks file sizes. These sparse fixtures are never loaded for inference.
			dir, assets, err := embedassets.SelectedModel(root, tc.model)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(dir, 0o700))
			for _, asset := range assets {
				file, err := os.Create(filepath.Join(dir, asset.Name))
				require.NoError(t, err)
				err = file.Truncate(asset.Size)
				require.NoError(t, file.Close())
				require.NoError(t, err)
			}
		}
		recorder := httptest.NewRecorder()
		h.availableModels(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
		assert.JSONEq(t, tc.response, recorder.Body.String())
	}
}
