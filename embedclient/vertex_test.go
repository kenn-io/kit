package embedclient_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

func vertexOptions(baseURL, model string) embedclient.Options {
	m := unitModel()
	m.Name = model
	return embedclient.Options{Model: m, Deployment: embedconfig.Deployment{BaseURL: baseURL, API: embedconfig.APIVertex}, HTTP: &http.Client{}}
}

func TestVertexPredict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models/gemini-embedding-001:predict", r.URL.Path)
		assert.Equal(t, map[string]any{"instances": []any{map[string]any{"content": "text", "task_type": "RETRIEVAL_DOCUMENT"}}, "parameters": map[string]any{"outputDimensionality": float64(2)}}, readBody(t, r))
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{3, 4}}}}})
	}))
	t.Cleanup(server.Close)
	opts := vertexOptions(server.URL+"/models", "gemini-embedding-001")
	opts.Model.Normalization = embedconfig.NormalizationL2
	client, err := embedclient.New(opts)
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"text"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.InDeltaSlice(t, []float32{0.6, 0.8}, vectors[0], 1e-6)
}

func TestVertexEmbedContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models/gemini-embedding-2-preview:embedContent", r.URL.Path)
		assert.Equal(t, map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "query: text"}}}, "embedContentConfig": map[string]any{"outputDimensionality": float64(2)}}, readBody(t, r))
		writeJSON(t, w, map[string]any{"embedding": map[string]any{"values": []float64{1, 0}}})
	}))
	t.Cleanup(server.Close)
	opts := vertexOptions(server.URL+"/models", "gemini-embedding-2-preview")
	opts.Roles.QueryPrefix = "query: "
	client, err := embedclient.New(opts)
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"text"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 0}}, vectors)
}

func TestVertexOneInputPerRequest(t *testing.T) {
	var instances []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		instances = append(instances, len(body["instances"].([]any)))
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}}}})
	}))
	t.Cleanup(server.Close)
	opts := vertexOptions(server.URL, "text-embedding-005")
	opts.Batch.Items = 300
	client, err := embedclient.New(opts)
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a", "b"})
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	assert.Equal(t, []int{1, 1}, instances)
}

func TestVertexRejectsPredictionCountMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"predictions":[]}`))
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(vertexOptions(server.URL, "model"))
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	require.Error(t, err)
}

func TestVertexDefaultCredentials(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)
	path := filepath.Join(t.TempDir(), "credentials.json")
	credentials := fmt.Sprintf(`{"type":"external_account_authorized_user","client_id":"test","client_secret":"test","refresh_token":"test","token_url":%q}`, tokenServer.URL)
	require.NoError(t, os.WriteFile(path, []byte(credentials), 0o600))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		if r.URL.Path == "/slow/model:predict" {
			select {
			case <-time.After(150 * time.Millisecond):
			case <-r.Context().Done():
			}
			return
		}
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}}}})
	}))
	t.Cleanup(server.Close)
	opts := vertexOptions(server.URL, "model")
	opts.HTTP = nil
	client, err := embedclient.New(opts)
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	require.NoError(t, err)
	opts.Deployment.BaseURL = server.URL + "/slow"
	opts.Transport.Timeout = 30 * time.Millisecond
	client, err = embedclient.New(opts)
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	var transportErr *embedclient.TransportError
	require.ErrorAs(t, err, &transportErr)
	assert.True(t, transportErr.Timeout())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	_, err = embedclient.New(opts)
	require.Error(t, err)
}
