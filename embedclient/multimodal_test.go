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
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/publishers/google/models", API: embedconfig.APIVertex}, Roles: embedconfig.Roles{QueryPrefix: "query: "}})
	require.NoError(t, err)
	vectors, err := client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: []byte{1, 2, 3}}}}})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.InDeltaSlice(t, []float32{0.6, 0.8}, vectors[0], 1e-6)
}

func TestKitInputs(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embed", r.URL.Path)
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
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL + "/v1"}, Roles: embedconfig.Roles{QueryPrefix: "query: ", DocumentPrefix: "doc: ", DocumentSuffix: "!"}})
	require.NoError(t, err)
	_, err = client.EmbedInputs(t.Context(), []embedclient.Input{
		{Role: embedconfig.RoleQuery, Parts: []embedclient.Part{{Text: "one"}, {Text: "two"}}},
		{Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{Text: "document"}}},
		{Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: []byte{1}}, {Text: "caption"}}},
	})
	require.NoError(t, err)
	require.Len(t, bodies, 2)
	assert.Equal(t, []any{map[string]any{"role": "query", "parts": []any{map[string]any{"text": "query: one\ntwo"}}}, map[string]any{"role": "document", "parts": []any{map[string]any{"text": "doc: document!"}}}}, bodies[0]["inputs"])
	assert.Equal(t, []any{map[string]any{"role": "document", "parts": []any{map[string]any{"media_type": "image/png", "data": "AQ=="}, map[string]any{"text": "caption"}}}}, bodies[1]["inputs"])
}
func TestKitUnsupportedContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(415)
		_, _ = w.Write([]byte(`{"error":{"code":"unsupported_content","message":"unsupported"}}`))
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	_, err = client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: []byte{1}}}}})
	assert.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
	var apiErr *embedclient.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.False(t, apiErr.InputRejected())
}
func TestKitRejectsInvalidInputs(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
	t.Cleanup(server.Close)
	for _, api := range []embedconfig.API{embedconfig.APIOpenAI, embedconfig.APIKit} {
		client, err := embedclient.New(embedclient.Options{Model: unitModel(), Deployment: embedconfig.Deployment{API: api, BaseURL: server.URL}})
		require.NoError(t, err)
		for _, in := range []embedclient.Input{
			{Role: embedconfig.RoleQuery, Parts: []embedclient.Part{{MediaType: "image/png"}}},
			{Role: embedconfig.RoleQuery, Parts: []embedclient.Part{{Text: "caption", MediaType: "image/png", Data: []byte{1}}}},
			{Role: "bad", Parts: []embedclient.Part{{Text: "text"}}},
		} {
			_, err = client.EmbedInputs(t.Context(), []embedclient.Input{in})
			assert.Error(t, err)
		}
		if api == embedconfig.APIOpenAI {
			_, err = client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleQuery, Parts: []embedclient.Part{{MediaType: "image/png", Data: []byte{1}}}}})
			assert.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
		}
	}
	assert.Zero(t, calls)
}

func TestKitGeminiAllowsPreformattedText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		assert.Equal(t, []any{map[string]any{"role": "query", "parts": []any{map[string]any{"text": "one"}, map[string]any{"text": "two"}}}}, body["inputs"])
		writeJSON(t, w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{3, 4}}}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-2"
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	input := []embedclient.Input{{Role: embedconfig.RoleQuery, Parts: []embedclient.Part{{Text: "one"}, {Text: "two"}}}}
	_, err = client.Encode(t.Context(), 0, input)
	require.NoError(t, err)
	_, err = client.EmbedInputs(t.Context(), input)
	assert.ErrorContains(t, err, "needs its task")
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"hello"})
	assert.ErrorContains(t, err, "needs its task")
}
func TestVertexOtherModelsRejectMedia(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "text-embedding-005"
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIVertex, BaseURL: server.URL + "/publishers/google/models"}})
	require.NoError(t, err)
	_, err = client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: []byte{1}}}}})
	assert.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
	assert.Zero(t, calls)
}

func TestKitGeminiMediaNeedsNoTextFormatting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		assert.Equal(t, []any{map[string]any{"role": "document", "parts": []any{map[string]any{"media_type": "image/png", "data": "AQ=="}}}}, body["inputs"])
		writeJSON(t, w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{3, 4}}}})
	}))
	t.Cleanup(server.Close)
	model := unitModel()
	model.Name = "gemini-embedding-2"
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	_, err = client.EmbedInputs(t.Context(), []embedclient.Input{{Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: []byte{1}}}}})
	require.NoError(t, err)
}
