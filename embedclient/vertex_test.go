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
	"go.kenn.io/kit/embedmodel"
)

func TestVertexPredict(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/models/gemini-embedding-001:predict", r.URL.Path)
		body := readBody(t, r)
		assert.Equal(t, map[string]any{"instances": []any{map[string]any{"content": "text1", "task_type": "RETRIEVAL_DOCUMENT"}}, "parameters": map[string]any{"outputDimensionality": float64(2), "autoTruncate": false}}, body)
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{3, 4}}}}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-001"
	model.Normalization = embedconfig.NormalizationL2
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/models", API: embedconfig.APIVertex}})
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"text1"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.InDeltaSlice(t, []float32{0.6, 0.8}, vectors[0], 1e-6)
	assert.Equal(t, 1, calls)
}

func TestVertexEmbedContent(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/models/gemini-embedding-2-preview:embedContent", r.URL.Path)
				assert.Equal(t, map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "query: text"}}}, "embedContentConfig": map[string]any{"outputDimensionality": float64(2)}}, readBody(t, r))
				writeJSON(t, w, map[string]any{"embedding": map[string]any{"values": []float64{1, 0}}, "truncated": truncated})
			}))
			t.Cleanup(server.Close)
			model := unitModel()
			model.Name = "gemini-embedding-2-preview"
			client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/models", API: embedconfig.APIVertex}, Roles: embedconfig.Roles{QueryPrefix: "query: "}, Batch: embedconfig.Batch{Items: 300}})
			require.NoError(t, err)
			assert.Equal(t, 1, client.BatchItems())
			vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"text", "text"})
			if truncated {
				require.ErrorContains(t, err, "truncated")
				assert.Nil(t, vectors)
				assert.Equal(t, 1, calls)
			} else {
				require.NoError(t, err)
				assert.Equal(t, [][]float32{{1, 0}, {1, 0}}, vectors)
				assert.Equal(t, 2, calls)
			}
		})
	}
}

func TestVertexRejectsPredictionCountMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"predictions":[]}`))
	}))
	t.Cleanup(server.Close)
	c, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}})
	require.NoError(t, err)
	_, err = c.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	require.Error(t, err)
}

func TestVertexRejectsInvalidModelNames(t *testing.T) {
	for _, name := range []string{"a/b", "a:b", "a?b", "a#b"} {
		model := unitModel()
		model.Name = name
		_, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: "http://localhost", API: embedconfig.APIVertex}})
		require.Error(t, err)
	}
}

func TestVertexRejectsOllamaRecovery(t *testing.T) {
	_, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: "http://localhost/v1", API: embedconfig.APIVertex}, OllamaMetalRecovery: true})
	require.Error(t, err)
}

func TestVertexRejectsFaces(t *testing.T) {
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: "http://localhost/v1", API: embedconfig.APIVertex}})
	require.NoError(t, err)
	_, err = client.EmbedFaces(t.Context(), nil, nil)
	require.Error(t, err)
}

func setGoogleTestCredentials(t *testing.T, tokenURL string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	credentials := fmt.Sprintf(`{"type":"external_account_authorized_user","client_id":"test","client_secret":"test","refresh_token":"test","token_url":%q}`, tokenURL)
	require.NoError(t, os.WriteFile(path, []byte(credentials), 0o600))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

func TestGoogleHTTPClientOverridesAPIKey(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)
	setGoogleTestCredentials(t, tokenServer.URL)
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}}}})
	}))
	t.Cleanup(server.Close)
	httpClient, err := embedclient.GoogleHTTPClient()
	require.NoError(t, err)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}, HTTP: httpClient, APIKey: "different-key"})
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token", authorization)
}

func TestVertexBatchItemsAndRequests(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		items int
		want  int
	}{
		{"gemini", "gemini-embedding-001", 300, 1},
		{"other capped", "text-embedding-005", 300, 5},
		{"caller cap", "text-embedding-005", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sizes []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readBody(t, r)
				instances := body["instances"].([]any)
				sizes = append(sizes, len(instances))
				predictions := make([]any, len(instances))
				for i := range predictions {
					predictions[i] = map[string]any{"embeddings": map[string]any{"values": []float64{float64(i + 1), 1}}}
				}
				writeJSON(t, w, map[string]any{"predictions": predictions})
			}))
			t.Cleanup(server.Close)
			model := unitModel()
			model.Name = tc.model
			model.Normalization = embedconfig.NormalizationNone
			client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}, Batch: embedconfig.Batch{Items: tc.items}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, client.BatchItems())
			texts := make([]string, tc.want+1)
			for i := range texts {
				texts[i] = "text"
			}
			vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, texts)
			require.NoError(t, err)
			require.Len(t, vectors, len(texts))
			assert.Equal(t, []int{tc.want, 1}, sizes)
			assert.Equal(t, float32(tc.want), vectors[tc.want-1][0])
		})
	}
}

func TestVertexInvalidVectorReportsInputIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"predictions": []any{
			map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}},
			map[string]any{"embeddings": map[string]any{"values": []float64{0, 0}}},
		}})
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}})
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a", "b"})
	var vectorErr *embedclient.VectorError
	require.ErrorAs(t, err, &vectorErr)
	assert.Equal(t, 1, vectorErr.Index)
	assert.Nil(t, vectors)
}

func TestVertexUsesTransportTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(150 * time.Millisecond):
			writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}}}})
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		name        string
		timeout     time.Duration
		httpTimeout time.Duration
	}{
		{"short", 30 * time.Millisecond, 0},
		{"caller timeout", time.Second, 30 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpClient := &http.Client{Timeout: tc.httpTimeout}
			client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}, HTTP: httpClient, Transport: embedconfig.Transport{Timeout: tc.timeout}})
			require.NoError(t, err)
			_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"text"})
			var transportErr *embedclient.TransportError
			require.ErrorAs(t, err, &transportErr)
			assert.True(t, transportErr.Timeout())
			assert.Equal(t, tc.httpTimeout, httpClient.Timeout)
		})
	}
}

func TestVertexBatchesMixedRoles(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body := readBody(t, r)
		assert.Equal(t, []any{
			map[string]any{"content": "doc: alpha!", "task_type": "RETRIEVAL_DOCUMENT"},
			map[string]any{"content": "query: beta?", "task_type": "RETRIEVAL_DOCUMENT"},
		}, body["instances"])
		writeJSON(t, w, map[string]any{"predictions": []any{
			map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}},
			map[string]any{"embeddings": map[string]any{"values": []float64{0, 1}}},
		}})
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}, Roles: embedconfig.Roles{DocumentPrefix: "doc: ", DocumentSuffix: "!", QueryPrefix: "query: ", QuerySuffix: "?"}})
	require.NoError(t, err)
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{{Role: embedconfig.RoleDocument, Text: "alpha"}, {Role: embedconfig.RoleQuery, Text: "beta"}})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
	assert.Equal(t, [][]float32{{1, 0}, {0, 1}}, vectors)
}
