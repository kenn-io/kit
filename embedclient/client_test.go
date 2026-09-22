package embedclient_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jsonv2 "encoding/json/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
)

func TestEmbedReordersIndexedResponses(t *testing.T) {
	var got map[string]any
	client := newClient(t, embedconfig.Model{
		Name: "m", Dimensions: 2,
		Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2,
		RequestDimensions: true, EncodingFormat: "float",
	}, embedconfig.Roles{InputType: embedconfig.InputTypeRetrieval}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		got = readBody(t, r)
		writeJSON(t, w, map[string]any{
			"usage": map[string]any{"total_tokens": 3},
			"data": []map[string]any{
				{"index": 1, "embedding": []float64{0, 1}},
				{"index": 0, "embedding": []float64{1, 0}},
			},
		})
	})
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "beta"},
	})
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[0], 1e-6)
	assert.InDeltaSlice(t, []float32{0, 1}, vectors[1], 1e-6)
	assert.Equal(t, "document", got["input_type"])
	dims, ok := got["dimensions"].(float64)
	require.True(t, ok)
	assert.InDelta(t, 2.0, dims, 0)
	assert.Equal(t, "float", got["encoding_format"])
}

func TestEmbedKeepsPositionalOrderWhenIndexIsAbsent(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{DocumentPrefix: "doc: "}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		_, indexed := body["input_type"]
		assert.False(t, indexed)
		inputs := body["input"].([]any)
		assert.Equal(t, "doc: alpha", inputs[0])
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"embedding": []float64{1, 0}}},
		})
	})
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{{
		Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha",
	}})
	require.NoError(t, err)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[0], 1e-6)
}

func TestEmbedRejectsBadResponses(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "duplicate index", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0}},
			{"index": 0, "embedding": []float64{0, 1}},
		}}},
		{name: "mixed index", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0}},
			{"embedding": []float64{0, 1}},
		}}},
		{name: "short response", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0}},
		}}},
		{name: "wrong dimensions", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1, 0, 0}},
			{"index": 1, "embedding": []float64{0, 1}},
		}}},
		{name: "null component", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []any{1.0, nil}},
			{"index": 1, "embedding": []float64{0, 1}},
		}}},
		{name: "non finite", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{1e308, 0}},
			{"index": 1, "embedding": []float64{0, 1}},
		}}},
		{name: "zero norm", body: map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{0, 0}},
			{"index": 1, "embedding": []float64{0, 1}},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, test.body)
			})
			_, err := client.Embed(t.Context(), twoTexts())
			require.Error(t, err)
		})
	}
}

func TestEmbedFailedResponseDoesNotEchoTheBody(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"secret prompt alpha"}`)
	})
	_, err := client.Embed(t.Context(), oneText())
	var api *embedclient.APIError
	require.ErrorAs(t, err, &api)
	assert.Equal(t, http.StatusTooManyRequests, api.StatusCode)
	assert.Equal(t, 3*time.Second, api.RetryAfter)
	assert.False(t, api.Definitive())
	assert.NotContains(t, err.Error(), "secret")
	assert.NotContains(t, err.Error(), "alpha")

	denied := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret prompt alpha", http.StatusBadRequest)
	})
	_, err = denied.Embed(t.Context(), oneText())
	require.ErrorAs(t, err, &api)
	assert.True(t, api.Definitive())
	assert.True(t, api.InputRejected())
	assert.False(t, api.CredentialsRejected())
	assert.NotContains(t, err.Error(), "secret")

	unauthorized := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret prompt alpha", http.StatusUnauthorized)
	})
	_, err = unauthorized.Embed(t.Context(), oneText())
	require.ErrorAs(t, err, &api)
	assert.False(t, api.Definitive())
	assert.False(t, api.InputRejected())
	assert.True(t, api.CredentialsRejected())
	assert.NotContains(t, err.Error(), "secret")
}

func TestEmbedBatchesByRoleAndRequestSize(t *testing.T) {
	var sizes []int
	client := newClient(t, unitModel(), embedconfig.Roles{
		DocumentPrefix: "doc: ",
		QueryPrefix:    "query: ",
	}, embedconfig.Batch{Items: 2}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		inputs := body["input"].([]any)
		sizes = append(sizes, len(inputs))
		data := make([]map[string]any, len(inputs))
		for i := range inputs {
			embedding := []float64{1, 0}
			if i == 1 {
				embedding = []float64{0, 1}
			}
			data[len(inputs)-1-i] = map[string]any{"index": i, "embedding": embedding}
		}
		writeJSON(t, w, map[string]any{"data": data})
	})
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "one"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "two"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "three"},
		{Role: embedconfig.RoleQuery, Kind: embedmodel.KindText, Text: "four"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 1, 1}, sizes)
	require.Len(t, vectors, 4)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[0], 1e-6)
	assert.InDeltaSlice(t, []float32{0, 1}, vectors[1], 1e-6)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[2], 1e-6)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[3], 1e-6)
}

func TestEmbedKeepsUnnormalizedVectors(t *testing.T) {
	model := unitModel()
	model.Normalization = embedconfig.NormalizationNone
	client := newClient(t, model, embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{3, 4}}},
		})
	})
	vectors, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	assert.InDeltaSlice(t, []float32{3, 4}, vectors[0], 1e-5)
}

func TestEmbedUsesCallerClientWithoutMutatingIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer resolved-key", r.Header.Get("Authorization"))
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{1, 0}}},
		})
	}))
	t.Cleanup(srv.Close)
	owned := &http.Client{Timeout: 5 * time.Second}
	client, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		APIKey:     "resolved-key",
		HTTP:       owned,
	})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	assert.Nil(t, owned.Transport)
	assert.Equal(t, 5*time.Second, owned.Timeout)
}

func TestEmbedRefusesCrossOriginRedirect(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.test/embeddings", http.StatusFound)
	})
	_, err := client.Embed(t.Context(), oneText())
	require.ErrorIs(t, err, embedclient.ErrOrigin)
}

func TestEmbedStripsTransportDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	owned := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial secret-host.example failed")
	})}
	client, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		APIKey:     "resolved-key",
		HTTP:       owned,
	})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), oneText())
	require.EqualError(t, err, "embed request failed")

	owned.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	client, err = embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		HTTP:       owned,
	})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), oneText())
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), srv.URL)
}

func TestEmbedRejectsBlankAndNonTextBeforeTheRequest(t *testing.T) {
	called := false
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{}, func(http.ResponseWriter, *http.Request) {
		called = true
	})
	_, err := client.Embed(t.Context(), []embedmodel.Content{{
		Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: " \u200b",
	}})
	require.ErrorIs(t, err, vector.ErrEmptyEmbeddingInput)
	_, err = client.Embed(t.Context(), []embedmodel.Content{{
		Role: embedconfig.RoleDocument,
		Kind: embedmodel.KindImage,
		Parts: []embedmodel.Part{{
			Kind: embedmodel.KindImage, MediaType: "image/png",
		}},
	}})
	require.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
	assert.False(t, called)
}

func TestEncodeFuncSendsPreparedTextUnchanged(t *testing.T) {
	var got []any
	client := newClient(t, unitModel(), embedconfig.Roles{DocumentPrefix: "doc: ", DocumentSuffix: " END"}, embedconfig.Batch{Items: 4}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		got = body["input"].([]any)
		writeJSON(t, w, map[string]any{"data": []map[string]any{
			{"embedding": []float64{1, 0}},
		}})
	})
	_, err := client.EncodeFunc(embedconfig.RoleDocument)(t.Context(), []string{"doc: ab END"})
	require.NoError(t, err)
	require.Equal(t, []any{"doc: ab END"}, got)
}

func TestEncodeFuncPreservesOrder(t *testing.T) {
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{Items: 8}, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": []float64{0, 1}},
			{"index": 0, "embedding": []float64{1, 0}},
		}})
	})
	vectors, err := client.EncodeFunc(embedconfig.RoleQuery)(t.Context(), []string{"alpha", "beta"})
	require.NoError(t, err)
	assert.InDeltaSlice(t, []float32{1, 0}, vectors[0], 1e-6)
	assert.InDeltaSlice(t, []float32{0, 1}, vectors[1], 1e-6)
}

func TestEmbedRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
	}))
	t.Cleanup(srv.Close)
	limited, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		Transport:  embedconfig.Transport{MaxResponseBytes: 8},
	})
	require.NoError(t, err)
	_, err = limited.Embed(t.Context(), oneText())
	require.EqualError(t, err, "embed response exceeds the configured cap")
}

func TestEmbedSplitsOneRoleWhenTokenBudgetIsSmall(t *testing.T) {
	var sizes []int
	client := newClient(t, unitModel(), embedconfig.Roles{}, embedconfig.Batch{
		Items: 8, MaxTokens: 5, InputTokenUpperBound: 2,
	}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		inputs := body["input"].([]any)
		sizes = append(sizes, len(inputs))
		data := make([]map[string]any, len(inputs))
		for i := range inputs {
			data[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		writeJSON(t, w, map[string]any{"data": data})
	})
	vectors, err := client.Embed(t.Context(), []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "one"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "two"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "three"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{2, 1}, sizes)
	require.Len(t, vectors, 3)
}

func TestEmbedDecodesBase64Float32Payload(t *testing.T) {
	want := []float32{1.25, -3.5}
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint32(raw[0:4], math.Float32bits(want[0]))
	binary.LittleEndian.PutUint32(raw[4:8], math.Float32bits(want[1]))
	encoded := base64.StdEncoding.EncodeToString(raw)
	model := unitModel()
	model.Normalization = embedconfig.NormalizationNone
	model.EncodingFormat = "base64"
	client := newClient(t, model, embedconfig.Roles{}, embedconfig.Batch{}, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		assert.Equal(t, "base64", body["encoding_format"])
		writeJSON(t, w, map[string]any{
			"data": []map[string]any{{"embedding": encoded}},
		})
	})
	vectors, err := client.Embed(t.Context(), oneText())
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.InDeltaSlice(t, want, vectors[0], 0)
}

func TestNewRejectsNonCosineMetric(t *testing.T) {
	for _, metric := range []embedconfig.Metric{embedconfig.MetricDotProduct, embedconfig.MetricL2} {
		t.Run(string(metric), func(t *testing.T) {
			model := unitModel()
			model.Metric = metric
			_, err := embedclient.New(embedclient.Options{
				Model:      model,
				Deployment: embedconfig.Deployment{BaseURL: "https://example.test/v1"},
			})
			require.EqualError(t, err, "embed model metric must be cosine; dot_product and l2 are not storable yet")
		})
	}
}

func TestNewRejectsUnsupportedEncodingFormat(t *testing.T) {
	model := unitModel()
	model.EncodingFormat = "bytes"
	_, err := embedclient.New(embedclient.Options{
		Model:      model,
		Deployment: embedconfig.Deployment{BaseURL: "https://example.test/v1"},
	})
	require.EqualError(t, err, "embed encoding format must be empty, float, or base64")
}

func TestNewRejectsTokenBoundAboveTheBudget(t *testing.T) {
	_, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: "https://example.test/v1"},
		Batch:      embedconfig.Batch{Items: 4, MaxTokens: 3, InputTokenUpperBound: 4},
	})
	require.EqualError(t, err, "embed batch per-input token bound must fit in the aggregate cap")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newClient(
	t *testing.T,
	model embedconfig.Model,
	roles embedconfig.Roles,
	batch embedconfig.Batch,
	handler http.HandlerFunc,
) *embedclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:      model,
		Roles:      roles,
		Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		Batch:      batch,
	})
	require.NoError(t, err)
	return client
}

func unitModel() embedconfig.Model {
	return embedconfig.Model{
		Name: "m", Dimensions: 2,
		Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2,
	}
}

func oneText() []embedmodel.Content {
	return []embedmodel.Content{{
		Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha",
	}}
}

func twoTexts() []embedmodel.Content {
	return []embedmodel.Content{
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "alpha"},
		{Role: embedconfig.RoleDocument, Kind: embedmodel.KindText, Text: "beta"},
	}
}

func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	defer func() { _ = r.Body.Close() }()
	payload, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, jsonv2.Unmarshal(payload, &body))
	return body
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	payload, err := jsonv2.Marshal(body)
	require.NoError(t, err)
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(payload)
	require.NoError(t, err)
}
