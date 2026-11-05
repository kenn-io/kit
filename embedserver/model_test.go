package embedserver

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

func TestMissingRuntimePreventsModelDownload(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	_, err := loadModel(t.Context(), dir, filepath.Join(root, "missing-library"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTextModelRequiresInstallation(t *testing.T) {
	root := t.TempDir()
	library := filepath.Join(root, "runtime")
	require.NoError(t, os.WriteFile(library, []byte("unused until models are installed"), 0o600))
	_, err := loadModel(t.Context(), filepath.Join(root, "models"), library)
	require.ErrorContains(t, err, "install nomic-embed-text: missing or corrupt model.onnx")
}

func TestMissingRuntimePreventsServerStartup(t *testing.T) {
	_, err := loadModels(t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "missing-library"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMissingModelsAreNotRetried(t *testing.T) {
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
			calls := 0
			var responseBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				recorder := httptest.NewRecorder()
				if face {
					h.faces(recorder, r)
				} else {
					h.ServeHTTP(recorder, r)
				}
				responseBody = recorder.Body.String()
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
			}))
			defer server.Close()
			client, err := embedclient.New(embedclient.Options{
				Model:      embedconfig.Model{Name: "nomic-embed-text", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
				Deployment: embedconfig.Deployment{BaseURL: server.URL + "/v1"},
				Retry:      embedclient.Retry{MaxAttempts: 3},
			})
			require.NoError(t, err)
			if face {
				_, err = client.EmbedFaces(t.Context(), preview.Bytes(), []embedclient.FaceBox{{Width: 1, Height: 1}})
			} else {
				_, err = client.EncodeFunc(embedconfig.RoleQuery)(t.Context(), []string{"hello"})
			}
			server.Close()
			api, ok := errors.AsType[*embedclient.APIError](err)
			require.True(t, ok, "%v", err)
			require.Equal(t, http.StatusFailedDependency, api.StatusCode)
			require.False(t, api.Retryable())
			require.Equal(t, 1, calls)
			require.Contains(t, responseBody, "install ")
			require.Contains(t, responseBody, "missing or corrupt")
		})
	}
}
