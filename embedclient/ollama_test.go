package embedclient_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

func TestOllamaMetalRecoveryKeepsTheGoodVector(t *testing.T) {
	var nativeBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[3,4]},{"index":1,"embedding":[null,1]}]}`)
		case "/api/embed":
			body, _ := io.ReadAll(r.Body)
			nativeBodies = append(nativeBodies, string(body))
			_, _ = io.WriteString(w, `{"embeddings":[[3,4]]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: srv.URL + "/v1"},
		Batch:               embedconfig.Batch{Items: 4},
		OllamaMetalRecovery: true,
	})
	require.NoError(t, err)
	got, err := client.Embed(t.Context(), []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "beta"},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDelta(t, 0.6, got[0][0], 0.0001)
	assert.InDelta(t, 0.8, got[0][1], 0.0001)
	assert.InDelta(t, 0.6, got[1][0], 0.0001)
	require.Len(t, nativeBodies, 1)
	assert.Contains(t, nativeBodies[0], `"keep_alive":"0s"`)
	assert.NotContains(t, nativeBodies[0], "num_gpu")
	assert.Contains(t, nativeBodies[0], `"input":["beta"]`)
}

func TestOllamaMetalRecoveryFallsBackToCPU(t *testing.T) {
	var nativeBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"embedding":[null,1]}]}`)
		case "/api/ps":
			_, _ = io.WriteString(w, `{"models":[]}`)
		case "/api/embed":
			body, _ := io.ReadAll(r.Body)
			nativeBodies = append(nativeBodies, string(body))
			if len(nativeBodies) < 3 {
				http.Error(w, "runner failed", http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"embeddings":[[3,4]]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: srv.URL + "/v1"},
		Batch:               embedconfig.Batch{Items: 4},
		OllamaMetalRecovery: true,
	})
	require.NoError(t, err)
	got, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	require.Len(t, nativeBodies, 3)
	assert.NotContains(t, nativeBodies[1], "num_gpu")
	assert.Contains(t, nativeBodies[2], `"num_gpu":0`)
	assert.InDelta(t, 0.6, got[0][0], 0.0001)
}

func TestNewRejectsOllamaRecoveryWithoutAV1Path(t *testing.T) {
	_, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: "https://example.test/embeddings"},
		OllamaMetalRecovery: true,
	})
	require.Error(t, err)
}
