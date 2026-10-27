package embedserver

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

type fakeEngine struct {
	mu     sync.Mutex
	inputs []string
	closed bool
	run    func(context.Context, string) ([]float32, int, error)
}

func (e *fakeEngine) embed(ctx context.Context, text string) ([]float32, int, error) {
	e.mu.Lock()
	e.inputs = append(e.inputs, text)
	e.mu.Unlock()
	if e.run != nil {
		return e.run(ctx, text)
	}
	v := make([]float32, 768)
	v[0] = 3
	v[1] = 4
	return v, 7, nil
}
func (e *fakeEngine) Close() error { e.mu.Lock(); defer e.mu.Unlock(); e.closed = true; return nil }

func TestHandlerThroughClient(t *testing.T) {
	for _, encoding := range []string{"float", "base64"} {
		for _, dimensions := range []int{256, 768} {
			t.Run(encoding+"/"+strconv.Itoa(dimensions), func(t *testing.T) {
				e := new(fakeEngine)
				e.run = func(_ context.Context, text string) ([]float32, int, error) {
					v := make([]float32, 768)
					switch text {
					case "search_query: first":
						v[0], v[1] = 3, 4
					case "search_query: second":
						v[0], v[1] = -3, 4
					default:
						return nil, 0, errors.New("unexpected input")
					}
					return v, 7, nil
				}
				server := httptest.NewServer(&handler{engine: e, gate: make(chan struct{}, 1)})
				defer server.Close()
				client, err := embedclient.New(embedclient.Options{
					Model:      embedconfig.Model{Name: "nomic-embed-text", Dimensions: dimensions, RequestDimensions: true, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2, EncodingFormat: encoding},
					Roles:      embedconfig.Roles{QueryPrefix: "search_query: "},
					Deployment: embedconfig.Deployment{BaseURL: server.URL + "/v1"}, Batch: embedconfig.Batch{Items: 32},
				})
				require.NoError(t, err)
				vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"first", "second"})
				require.NoError(t, err)
				require.Len(t, vectors, 2)
				assert.Len(t, vectors[0], dimensions)
				assert.InDelta(t, 0.6, vectors[0][0], 1e-6)
				assert.InDelta(t, -0.6, vectors[1][0], 1e-6)
				assert.InDelta(t, 0.8, vectors[1][1], 1e-6)
				assert.Equal(t, []string{"search_query: first", "search_query: second"}, e.inputs)
			})
		}
	}
}

func TestHandlerWire(t *testing.T) {
	e := new(fakeEngine)
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	for _, input := range []string{`"search_query: literal"`, `["search_query: literal"]`} {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text:latest","input":`+input+`,"dimensions":0,"input_type":"query"}`)))
		require.Equal(t, 200, recorder.Code)
		var out response
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out))
		assert.Equal(t, "nomic-embed-text:latest", out.Model)
		assert.Equal(t, usage{7, 7}, out.Usage)
		require.Len(t, out.Data, 1)
		assert.Equal(t, 0, out.Data[0].Index)
	}
	assert.Equal(t, []string{"search_query: literal", "search_query: literal"}, e.inputs)
}

func TestHandlerRejectsBeforeInference(t *testing.T) {
	cases := []struct {
		body   string
		status int
	}{
		{`{`, 400},
		{`{"model":"other","input":"x"}`, 404},
		{`{"model":"nomic-embed-text","input":1}`, 400},
		{`{"model":"nomic-embed-text","input":[]}`, 400},
		{`{"model":"nomic-embed-text","input":[1]}`, 400},
		{`{"model":"nomic-embed-text","input":null}`, 400},
		{`{"model":"nomic-embed-text","input":[null]}`, 400},
		{`{"model":"nomic-embed-text","input":"x","dimensions":769}`, 400},
		{`{"model":"nomic-embed-text","input":"x","encoding_format":"int8"}`, 400},
		{`{"model":"nomic-embed-text","input":[` + strings.Repeat(`"x",`, 32) + `"x"]}`, 400},
		{strings.Repeat("x", (4<<20)+1), 413},
	}
	e := new(fakeEngine)
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(tc.body)))
		assert.Equal(t, tc.status, recorder.Code)
	}
	assert.Empty(t, e.inputs)
}

func TestHandlerSerializesAndCancels(t *testing.T) {
	entered := make(chan struct{})
	e := &fakeEngine{run: func(ctx context.Context, _ string) ([]float32, int, error) {
		close(entered)
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}}
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := func(ctx context.Context) *http.Request {
		return httptest.NewRequestWithContext(ctx, "POST", "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`))
	}
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), request(ctx)) }()
	<-entered
	waiting, stop := context.WithCancel(t.Context())
	stop()
	h.ServeHTTP(httptest.NewRecorder(), request(waiting))
	cancel()
	<-done
	e.mu.Lock()
	defer e.mu.Unlock()
	assert.Equal(t, []string{"x"}, e.inputs)
}

func TestReducedDimensionsRenormalizesPrefix(t *testing.T) {
	e := &fakeEngine{run: func(context.Context, string) ([]float32, int, error) {
		v := make([]float32, 768)
		v[0], v[767] = 0.6, 0.8
		return v, 3, nil
	}}
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	reduced, err := h.embeddings(t.Context(), request{Dimensions: 256}, []string{"search_query: river"})
	require.NoError(t, err)
	vector := reduced.Data[0].Embedding.([]float32)
	require.Len(t, vector, 256)
	assert.InDelta(t, 1, vector[0], 1e-6)
	full, err := h.embeddings(t.Context(), request{Dimensions: 768}, []string{"search_query: river"})
	require.NoError(t, err)
	assert.InDelta(t, 0.6, full.Data[0].Embedding.([]float32)[0], 1e-6)
}
