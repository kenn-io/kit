package embedclient_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
		assert.Equal(t, "/publishers/google/models/gemini-embedding-001:predict", r.URL.Path)
		assert.Equal(t, map[string]any{
			"instances": []any{
				map[string]any{"content": "doc: alpha!", "task_type": "RETRIEVAL_DOCUMENT"},
				map[string]any{"content": "query: beta?", "task_type": "RETRIEVAL_QUERY"},
			},
			"parameters": map[string]any{"outputDimensionality": float64(2), "autoTruncate": false},
		}, readBody(t, r))
		writeJSON(t, w, map[string]any{"predictions": []any{
			map[string]any{"embeddings": map[string]any{"values": []float64{3, 4}}},
			map[string]any{"embeddings": map[string]any{"values": []float64{0, 1}}},
		}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-001"
	model.Normalization = embedconfig.NormalizationL2
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/publishers/google/models", API: embedconfig.APIVertex}, Roles: embedconfig.Roles{DocumentPrefix: "doc: ", DocumentSuffix: "!", QueryPrefix: "query: ", QuerySuffix: "?"}})
	require.NoError(t, err)
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{{Role: embedconfig.RoleDocument, Text: "alpha"}, {Role: embedconfig.RoleQuery, Text: "beta"}})
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	assert.InDeltaSlice(t, []float32{0.6, 0.8}, vectors[0], 1e-6)
	assert.Equal(t, []float32{0, 1}, vectors[1])
	assert.Equal(t, 1, calls)
}

func TestVertexEmbedContent(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(strconv.FormatBool(truncated), func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/publishers/google/models/gemini-embedding-2-preview:embedContent", r.URL.Path)
				body := readBody(t, r)
				assert.Equal(t, map[string]any{"parts": []any{map[string]any{"text": "query: text"}}}, body["content"])
				assert.Equal(t, float64(2), body["embedContentConfig"].(map[string]any)["outputDimensionality"])
				writeJSON(t, w, map[string]any{"embedding": map[string]any{"values": []float64{3, 4}}, "truncated": truncated})
			}))
			t.Cleanup(server.Close)
			model := unitModel()
			model.Name = "gemini-embedding-2-preview"
			client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/publishers/google/models", API: embedconfig.APIVertex}, Roles: embedconfig.Roles{QueryPrefix: "query: "}, Batch: embedconfig.Batch{Items: 300}})
			require.NoError(t, err)
			vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"text", "text"})
			if truncated {
				var apiErr *embedclient.APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, http.StatusOK, apiErr.StatusCode)
				assert.True(t, apiErr.InputRejected())
				assert.Nil(t, vectors)
				assert.Equal(t, 1, calls)
			} else {
				require.NoError(t, err)
				require.Len(t, vectors, 2)
				for _, vector := range vectors {
					assert.InDeltaSlice(t, []float32{0.6, 0.8}, vector, 1e-6)
				}
				assert.Equal(t, 2, calls)
			}
		})
	}
}

func TestGoogleHTTPClientOverridesAPIKey(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)
	path := filepath.Join(t.TempDir(), "credentials.json")
	credentials := fmt.Sprintf(`{"type":"external_account_authorized_user","client_id":"test","client_secret":"test","refresh_token":"test","token_url":%q}`, tokenServer.URL)
	require.NoError(t, os.WriteFile(path, []byte(credentials), 0o600))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{1, 0}}}}})
	}))
	t.Cleanup(server.Close)
	httpClient, err := embedclient.GoogleHTTPClient()
	require.NoError(t, err)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL + "/publishers/google/models", API: embedconfig.APIVertex}, HTTP: httpClient, APIKey: "different-key"})
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token", authorization)
}

func TestVertexBatchItemsAndRequests(t *testing.T) {
	for _, tc := range []struct {
		name        string
		model       string
		items       int
		texts       int
		sizes       []int
		rejectAbove int
		message     string
	}{
		{"gemini capped", "gemini-embedding-001", 300, 251, []int{250, 1}, 0, ""},
		{"gemini caller cap", "gemini-embedding-001", 3, 4, []int{3, 1}, 0, ""},
		{"other model", "text-embedding-005", 300, 2, []int{1, 1}, 0, ""},
		{"split", "gemini-embedding-001", 32, 5, []int{5, 2, 3, 1, 2}, 2, "input token count is 23000 but the model supports up to 20000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sizes []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readBody(t, r)
				instances := body["instances"].([]any)
				sizes = append(sizes, len(instances))
				if tc.rejectAbove > 0 && len(instances) > tc.rejectAbove {
					w.WriteHeader(http.StatusBadRequest)
					writeJSON(t, w, map[string]any{"error": map[string]any{"message": tc.message}})
					return
				}
				predictions := make([]any, len(instances))
				for i := range predictions {
					content := instances[i].(map[string]any)["content"].(string)
					value, err := strconv.Atoi(content)
					assert.NoError(t, err)
					predictions[i] = map[string]any{"embeddings": map[string]any{"values": []float64{float64(value), 1}}}
				}
				writeJSON(t, w, map[string]any{"predictions": predictions})
			}))
			t.Cleanup(server.Close)
			model := unitModel()
			model.Name = tc.model
			model.Normalization = embedconfig.NormalizationNone
			client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/publishers/google/models", API: embedconfig.APIVertex}, Batch: embedconfig.Batch{Items: tc.items}})
			require.NoError(t, err)
			texts := make([]string, tc.texts)
			for i := range texts {
				texts[i] = strconv.Itoa(i + 1)
			}
			vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, texts)
			require.NoError(t, err)
			require.Len(t, vectors, len(texts))
			assert.Equal(t, tc.sizes, sizes)
			for i, vector := range vectors {
				assert.Equal(t, []float32{float32(i + 1), 1}, vector)
			}
		})
	}
}

func TestVertexInvalidPredictions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"predictions":[]}`))
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-001"
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/publishers/google/models", API: embedconfig.APIVertex}})
	require.NoError(t, err)
	vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	require.ErrorContains(t, err, "0 vectors for 1 inputs")
	assert.Nil(t, vectors)
}

func TestVertexUsesTransportTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(time.Second): //nolint:kennlint // deadlinetest: bound a regression that removes the client timeout
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL + "/publishers/google/models", API: embedconfig.APIVertex}, Transport: embedconfig.Transport{Timeout: 30 * time.Millisecond}}) //nolint:kennlint // deadlinetest: a real HTTP request must time out
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"text"})
	var transportErr *embedclient.TransportError
	require.ErrorAs(t, err, &transportErr)
	assert.True(t, transportErr.Timeout())
}
