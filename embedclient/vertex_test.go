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
		assert.Equal(t, "/models/text-embedding-005:predict", r.URL.Path)
		assert.Equal(t, map[string]any{
			"instances": []any{
				map[string]any{"content": "doc: alpha!", "task_type": "RETRIEVAL_DOCUMENT"},
				map[string]any{"content": "query: beta?", "task_type": "RETRIEVAL_DOCUMENT"},
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
	model.Name = "text-embedding-005"
	model.Normalization = embedconfig.NormalizationL2
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/models", API: embedconfig.APIVertex}, Roles: embedconfig.Roles{DocumentPrefix: "doc: ", DocumentSuffix: "!", QueryPrefix: "query: ", QuerySuffix: "?"}})
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
		texts int
		sizes []int
	}{
		{"gemini batch", "gemini-embedding-001", 32, 32, 3, []int{3}},
		{"gemini capped", "gemini-embedding-001", 300, 250, 251, []int{250, 1}},
		{"gemini caller cap", "gemini-embedding-001", 3, 3, 4, []int{3, 1}},
		{"other capped", "text-embedding-005", 300, 5, 6, []int{5, 1}},
		{"caller cap", "text-embedding-005", 3, 3, 4, []int{3, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sizes []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readBody(t, r)
				instances := body["instances"].([]any)
				sizes = append(sizes, len(instances))
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
			client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}, Batch: embedconfig.Batch{Items: tc.items}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, client.BatchItems())
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
	for _, tc := range []struct {
		name       string
		body       string
		texts      []string
		countError string
	}{
		{"count mismatch", `{"predictions":[]}`, []string{"a"}, "0 vectors for 1 inputs"},
		{"invalid vector", `{"predictions":[{"embeddings":{"values":[1,0]}},{"embeddings":{"values":[0,0]}}]}`, []string{"a", "b"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}})
			require.NoError(t, err)
			vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, tc.texts)
			if tc.countError != "" {
				require.ErrorContains(t, err, tc.countError)
			} else {
				var vectorErr *embedclient.VectorError
				require.ErrorAs(t, err, &vectorErr)
				assert.Equal(t, 1, vectorErr.Index)
			}
			assert.Nil(t, vectors)
		})
	}
}

func TestVertexUsesTransportTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	for _, tc := range []struct {
		name        string
		timeout     time.Duration
		httpTimeout time.Duration
	}{
		{"short", 30 * time.Millisecond, 0},                    //nolint:kennlint // deadlinetest: a real HTTP request must time out
		{"caller timeout", time.Second, 30 * time.Millisecond}, //nolint:kennlint // deadlinetest: a real HTTP request must time out
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
