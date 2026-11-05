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
	"go.kenn.io/kit/internal/embedassets"
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
	isolated := t.TempDir()
	source, _, err := embedassets.SelectedModel(dir, embedassets.TextModel)
	require.NoError(t, err)
	destination, _, err := embedassets.SelectedModel(isolated, embedassets.TextModel)
	require.NoError(t, err)
	require.NoError(t, os.CopyFS(destination, os.DirFS(source)))
	e, err := loadModels(t.Context(), isolated, library)
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	h := &handler{engine: e, gate: make(chan struct{}, 1), log: os.Stderr}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/faces", h.faces)
	mux.HandleFunc("/v1/models", h.availableModels)
	mux.Handle("/v1/embeddings", h)
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "nomic-embed-text", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/v1"}})
	require.NoError(t, err)
	// A text process advertises faces only after installation, without restart.
	vectors, err := client.EncodeFunc(embedconfig.RoleQuery)(t.Context(), []string{"search_query: hello"})
	require.NoError(t, err)
	require.Len(t, vectors[0], 768)
	before := httptest.NewRecorder()
	h.availableModels(before, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
	assert.JSONEq(t, `{"models":["nomic-embed-text"]}`, before.Body.String())
	source, err = embedassets.InstalledModel(dir, embedassets.FaceModel)
	require.NoError(t, err)
	destination, _, err = embedassets.SelectedModel(isolated, embedassets.FaceModel)
	require.NoError(t, err)
	require.NoError(t, os.CopyFS(destination, os.DirFS(source)))
	after := httptest.NewRecorder()
	h.availableModels(after, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
	assert.JSONEq(t, `{"models":["nomic-embed-text","auraface-v1"]}`, after.Body.String())
	embedtest.AssertFaces(t, client, dir, filepath.Join("testdata", "faces", "boxes.json"))
	preview, err := os.ReadFile(filepath.Join(dir, "faces", "commons-glenn-official.jpg"))
	require.NoError(t, err)
	empty, err := client.EmbedFaces(t.Context(), preview, []embedclient.FaceBox{{X: 0.5, Y: 0.5, Width: 1e-20, Height: 1e-20}})
	require.NoError(t, err)
	require.Len(t, empty.Data, 1)
	assert.Nil(t, empty.Data[0].Embedding)
}
