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

func TestVertexPredict(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/models/gemini-embedding-001:predict", r.URL.Path)
		body := readBody(t, r)
		assert.Equal(t, map[string]any{"instances": []any{map[string]any{"content": fmt.Sprintf("text%d", calls), "task_type": "RETRIEVAL_DOCUMENT"}}, "parameters": map[string]any{"outputDimensionality": float64(2), "autoTruncate": false}}, body)
		if calls == 1 {
			writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{3, 4}}}}})
			return
		}
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{0, 2}}}}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-001"
	model.RequestDimensions = true
	model.Normalization = embedconfig.NormalizationL2
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/models", API: embedconfig.APIVertex}})
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"text1", "text2"})
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	assert.InDeltaSlice(t, []float32{0.6, 0.8}, vectors[0], 1e-6)
	assert.InDeltaSlice(t, []float32{0, 1}, vectors[1], 1e-6)
	assert.Equal(t, 2, calls)
}

func TestVertexBadResponseAndStatus(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"predictions":[]}`))
			}))
			t.Cleanup(server.Close)
			c, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}})
			require.NoError(t, err)
			_, err = c.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
			require.Error(t, err)
			if status != 200 {
				var api *embedclient.APIError
				require.ErrorAs(t, err, &api)
				assert.Equal(t, status == 401 || status == 403, api.CredentialsRejected())
				assert.Equal(t, status == 429, api.Retryable())
			}
		})
	}
}

func TestVertexRejectsUnsupportedOptions(t *testing.T) {
	for _, name := range []string{"a/b", "a:b", "a?b", "a#b"} {
		model := unitModel()
		model.Name = name
		_, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: "http://localhost", API: embedconfig.APIVertex}})
		require.Error(t, err)
	}
	_, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: "http://localhost", API: embedconfig.APIVertex}, OllamaMetalRecovery: true})
	require.Error(t, err)
}

func TestGoogleHTTPClientRefreshesCredentials(t *testing.T) {
	var tokens int
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":1}`, tokens)
	}))
	t.Cleanup(tokenServer.Close)
	path := filepath.Join(t.TempDir(), "credentials.json")
	subject := filepath.Join(t.TempDir(), "subject")
	require.NoError(t, os.WriteFile(subject, []byte("subject-token"), 0600))
	credentials := fmt.Sprintf(`{"type":"external_account","audience":"//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/test/providers/test","subject_token_type":"urn:ietf:params:oauth:token-type:jwt","token_url":%q,"credential_source":{"file":%q}}`, tokenServer.URL, subject)
	require.NoError(t, os.WriteFile(path, []byte(credentials), 0600))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	var headers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("Authorization"))
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}}}})
	}))
	t.Cleanup(server.Close)
	httpClient, err := embedclient.GoogleHTTPClient()
	require.NoError(t, err)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}, HTTP: httpClient, APIKey: "different-key"})
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"b"})
	require.NoError(t, err)
	assert.Equal(t, []string{"Bearer token-1", "Bearer token-2"}, headers)
}

func TestVertexMidListFailureReturnsNoVectors(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}}}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-001"
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}})
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a", "b", "c"})
	require.Error(t, err)
	assert.Nil(t, vectors)
	assert.Equal(t, 2, calls)
}

func TestVertexEmbedTextsAppliesRoleAffixes(t *testing.T) {
	var instances []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		instances = append(instances, body["instances"].([]any)...)
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}}}})
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex},
		Roles:      embedconfig.Roles{DocumentPrefix: "doc: ", DocumentSuffix: " :doc", QueryPrefix: "query: ", QuerySuffix: " :query"},
	})
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"text"})
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"text"})
	require.NoError(t, err)
	assert.Equal(t, []any{
		map[string]any{"content": "doc: text :doc", "task_type": "RETRIEVAL_DOCUMENT"},
		map[string]any{"content": "query: text :query", "task_type": "RETRIEVAL_DOCUMENT"},
	}, instances)
}

func TestVertexEmbedderRequestsConfiguredDimensions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		assert.Equal(t, map[string]any{"autoTruncate": false, "outputDimensionality": float64(768)}, body["parameters"])
		values := make([]float64, 768)
		values[0] = 1
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": values}}}})
	}))
	t.Cleanup(server.Close)
	parts, err := (embedconfig.Embedder{API: embedconfig.APIVertex, BaseURL: server.URL, Model: "m", Dims: 768}).Parts()
	require.NoError(t, err)
	client, err := embedclient.New(embedclient.Options{Model: parts.Model, Roles: parts.Roles, Deployment: parts.Deployment, Batch: parts.Batch, Transport: parts.Transport})
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"text"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.Len(t, vectors[0], 768)
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
				assert.Equal(t, map[string]any{"autoTruncate": false, "outputDimensionality": float64(2)}, body["parameters"])
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
