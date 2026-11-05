package embedserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/internal/embedtest"
)

// TestNativeParity uses public, synthetic Ollama outputs. Ordinary unit tests
// need no native runtime or network. CI sets both paths and runs this test on
// Linux, macOS, and Windows with cached, checksummed assets.
func TestNativeParity(t *testing.T) {
	dir, library := os.Getenv("KIT_EMBED_TEST_DIR"), os.Getenv("KIT_EMBED_TEST_LIBRARY")
	if dir == "" || library == "" {
		t.Skip("set KIT_EMBED_TEST_DIR and KIT_EMBED_TEST_LIBRARY for native parity")
	}
	samples := embedtest.Load(t, "testdata/ollama.json")
	e, err := loadModel(t.Context(), dir, library)
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	for _, sample := range samples {
		actual, tokens, err := e.embed(t.Context(), sample.Input)
		require.NoError(t, err)
		assert.Equal(t, sample.Tokens, tokens, sample.Input)
		sample.AssertParity(t, actual)
	}
}

func TestNativeFaces(t *testing.T) {
	dir, library := os.Getenv("KIT_EMBED_TEST_DIR"), os.Getenv("KIT_EMBED_TEST_LIBRARY")
	if dir == "" || library == "" {
		t.Skip("native face fixture is unset")
	}
	e, err := loadModels(t.Context(), dir, library)
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	h := &handler{engine: e, gate: make(chan struct{}, 1), log: os.Stderr}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/faces", h.faces)
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "nomic-embed-text", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/v1"}})
	require.NoError(t, err)
	embedtest.AssertFaces(t, client, dir, filepath.Join("testdata", "faces", "boxes.json"))
	t.Run("128 boxes with default transport", func(t *testing.T) {
		photo, err := os.ReadFile(filepath.Join(dir, "faces", "commons-glenn-official.jpg"))
		require.NoError(t, err)
		boxes := make([]embedclient.FaceBox, 128)
		for i := range boxes {
			boxes[i] = embedclient.FaceBox{X: .12, Y: .01, Width: .57, Height: .61}
		}
		result, err := client.EmbedFaces(t.Context(), photo, boxes)
		require.NoError(t, err)
		require.Len(t, result.Data, 128)
		for i, item := range result.Data {
			assert.Equal(t, i, item.Index)
			require.Len(t, item.Embedding, 512)
		}
	})
}
