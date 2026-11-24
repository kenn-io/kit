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
		assert.Equal(t, "/models/m:predict", r.URL.Path)
		body := readBody(t, r)
		assert.Equal(t, map[string]any{"instances": []any{map[string]any{"content": fmt.Sprintf("text%d", calls), "task_type": "RETRIEVAL_DOCUMENT"}}, "parameters": map[string]any{"outputDimensionality": float64(2)}}, body)
		if calls == 1 {
			writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{3, 4}}}}})
			return
		}
		writeJSON(t, w, map[string]any{"predictions": []any{map[string]any{"embeddings": map[string]any{"values": []float64{0, 2}}}}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
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
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{BaseURL: server.URL, API: embedconfig.APIVertex}, HTTP: httpClient})
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"a"})
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"b"})
	require.NoError(t, err)
	assert.Equal(t, []string{"Bearer token-1", "Bearer token-2"}, headers)
}
