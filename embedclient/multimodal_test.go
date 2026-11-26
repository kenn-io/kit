package embedclient_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

func TestVertexMedia(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		assert.Equal(t, map[string]any{"parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "AQID"}}}}, body["content"])
		writeJSON(t, w, map[string]any{"embedding": map[string]any{"values": []float64{3, 4}}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-2"
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIVertex, BaseURL: server.URL + "/publishers/google/models"}, Roles: embedconfig.Roles{QueryPrefix: "query: "}})
	require.NoError(t, err)
	vectors, err := client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleDocument, MediaType: "image/png", Data: []byte{1, 2, 3}}})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.InDeltaSlice(t, []float32{0.6, 0.8}, vectors[0], 1e-6)
}

func TestKitInputs(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		bodies = append(bodies, body)
		inputs := body["inputs"].([]any)
		data := make([]any, len(inputs))
		for i := range inputs {
			data[i] = map[string]any{"index": i, "embedding": []float64{3, 4}}
		}
		writeJSON(t, w, map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-2"
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}, Roles: embedconfig.Roles{QueryPrefix: "query: "}})
	require.NoError(t, err)
	_, err = client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleQuery, Text: " text \n"}, {Role: embedconfig.RoleDocument, MediaType: "image/png", Data: []byte{1}}})
	require.NoError(t, err)
	require.Len(t, bodies, 2)
	assert.Equal(t, []any{map[string]any{"role": "query", "text": "query:  text \n"}}, bodies[0]["inputs"])
	assert.Equal(t, []any{map[string]any{"role": "document", "media_type": "image/png", "data": "AQ=="}}, bodies[1]["inputs"])
	_, err = client.Encode(t.Context(), 0, []embedclient.Input{{Role: embedconfig.RoleQuery, Text: " text \n"}})
	require.NoError(t, err)
	assert.Equal(t, []any{map[string]any{"role": "query", "text": " text \n"}}, bodies[2]["inputs"])
	noRoles, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	_, err = noRoles.Encode(t.Context(), 2, []embedclient.Input{{Role: embedconfig.RoleQuery, Text: "as given"}})
	require.NoError(t, err)
	assert.Equal(t, []any{map[string]any{"role": "query", "text": "as given"}}, bodies[3]["inputs"])
}

func TestKitRejectsInvalidInputs(t *testing.T) {
	for _, in := range []embedclient.Input{{Role: embedconfig.RoleQuery}, {Role: embedconfig.RoleQuery, Text: " "}, {Role: embedconfig.RoleQuery, MediaType: "image/png"}, {Role: embedconfig.RoleQuery, Text: "text", MediaType: "image/png", Data: []byte{1}}, {Role: "bad", Text: "text"}} {
		assert.Error(t, in.Validate())
	}
	for _, api := range []embedconfig.API{embedconfig.APIOpenAI, embedconfig.APIVertex} {
		model := unitModel()
		base := "http://127.0.0.1/v1"
		if api == embedconfig.APIVertex {
			model.Name = "text-embedding-005"
			base = "http://127.0.0.1/publishers/google/models"
		}
		client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: api, BaseURL: base}})
		require.NoError(t, err)
		_, err = client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleQuery, MediaType: "image/png", Data: []byte{1}}})
		assert.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
	}
}

func TestKitUnsupportedContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(415)
		_, _ = w.Write([]byte(`{"error":{"code":"unsupported_content"}}`))
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	_, err = client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleDocument, MediaType: "image/png", Data: []byte{1}}})
	assert.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
	var api *embedclient.APIError
	require.ErrorAs(t, err, &api)
	assert.False(t, api.InputRejected())
}

func TestKitGeminiRoleGuard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{3, 4}}}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-2"
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	input := []embedclient.Input{{Role: embedconfig.RoleQuery, Text: "beach"}}
	_, err = client.Encode(t.Context(), 2, input)
	require.NoError(t, err)
	_, err = client.EmbedInputs(t.Context(), input)
	assert.ErrorContains(t, err, "needs its task")
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"beach"})
	assert.ErrorContains(t, err, "needs its task")
	_, err = client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleDocument, MediaType: "image/png", Data: []byte{1}}})
	require.NoError(t, err)
}

func TestKitMediaFailureIndex(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 3 {
			w.WriteHeader(415)
			_, _ = w.Write([]byte(`{"error":{"code":"unsupported_content"}}`))
			return
		}
		writeJSON(t, w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{3, 4}}}})
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	inputs := []embedclient.Input{{Role: embedconfig.RoleDocument, MediaType: "image/png", Data: []byte{1}}, {Role: embedconfig.RoleDocument, MediaType: "image/png", Data: []byte{2}}, {Role: embedconfig.RoleDocument, MediaType: "video/mp4", Data: []byte{3}}}
	vectors, err := client.EmbedInputs(t.Context(), inputs)
	require.Error(t, err)
	assert.ErrorContains(t, err, "input 2")
	assert.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
	var api *embedclient.APIError
	require.ErrorAs(t, err, &api)
	assert.Equal(t, 415, api.StatusCode)
	assert.Nil(t, vectors)
	assert.Equal(t, 3, calls)
}

func TestKitBatchSingletonRejection(t *testing.T) {
	var counts []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		inputs := body["inputs"].([]any)
		counts = append(counts, len(inputs))
		if len(inputs) > 1 || inputs[0].(map[string]any)["text"] == "large" {
			w.WriteHeader(413)
			_, _ = w.Write([]byte(`{"error":{"code":"input_too_long"}}`))
			return
		}
		writeJSON(t, w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{3, 4}}}})
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	vectors, err := client.Encode(t.Context(), 2, []embedclient.Input{{Role: embedconfig.RoleDocument, Text: "short"}, {Role: embedconfig.RoleDocument, Text: "large"}})
	var api *embedclient.APIError
	require.ErrorAs(t, err, &api)
	assert.True(t, api.InputRejected())
	assert.Equal(t, []int{2, 1, 1}, counts)
	assert.Nil(t, vectors)
}
