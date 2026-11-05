package embedserver

import (
	"bytes"
	"encoding/json/v2"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
)

func TestMissingRuntimePreventsModelDownload(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	_, err := loadModel(t.Context(), dir, filepath.Join(root, "missing-library"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMissingRuntimePreventsServerStartup(t *testing.T) {
	_, err := loadModels(t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "missing-library"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMissingModelsReportInstallation(t *testing.T) {
	var preview bytes.Buffer
	require.NoError(t, png.Encode(&preview, image.NewRGBA(image.Rect(0, 0, 2, 1))))
	for _, face := range []bool{false, true} {
		name := "text"
		if face {
			name = "faces"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			library := filepath.Join(root, "runtime")
			require.NoError(t, os.WriteFile(library, []byte("unused until models are installed"), 0o600))
			e, err := loadModels(t.Context(), root, library)
			require.NoError(t, err)
			defer func() { require.NoError(t, e.Close()) }()
			var log bytes.Buffer
			h := &handler{engine: e, gate: make(chan struct{}, 1), log: &log}
			recorder := httptest.NewRecorder()
			if face {
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
