package embedserver

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

type fakeEngine struct {
	mu      sync.Mutex
	inputs  []string
	run     func(context.Context, string) ([]float32, int, error)
	onClose func()
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

func (e *fakeEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.onClose != nil {
		e.onClose()
	}
	return nil
}

func TestHandlerThroughClient(t *testing.T) {
	for _, encoding := range []string{"float", "base64"} {
		for _, dimensions := range []int{256, 768} {
			t.Run(encoding+"/"+strconv.Itoa(dimensions), func(t *testing.T) {
				e := new(fakeEngine)
				server := httptest.NewServer(&handler{engine: e, gate: make(chan struct{}, 1)})
				defer server.Close()
				client, err := embedclient.New(embedclient.Options{
					Model:      embedconfig.Model{Name: "nomic-embed-text", Dimensions: dimensions, RequestDimensions: true, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2, EncodingFormat: encoding},
					Deployment: embedconfig.Deployment{BaseURL: server.URL + "/v1"}, Batch: embedconfig.Batch{Items: 32},
				})
				require.NoError(t, err)
				vectors, err := client.EmbedTexts(t.Context(), embedconfig.RoleQuery, []string{"first", "second"})
				require.NoError(t, err)
				require.Len(t, vectors, 2)
				assert.Len(t, vectors[0], dimensions)
				assert.Len(t, vectors[1], dimensions)
			})
		}
	}
}

func TestHandlerWire(t *testing.T) {
	e := new(fakeEngine)
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	for _, tc := range []struct {
		input string
		count int
	}{
		{`"search_query: literal"`, 1},
		{`["search_query: literal","search_document: other"]`, 2},
	} {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text:latest","input":`+tc.input+`,"dimensions":0,"input_type":"query"}`)))
		require.Equal(t, 200, recorder.Code)
		var out response
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out))
		assert.Equal(t, "nomic-embed-text:latest", out.Model)
		assert.Equal(t, usage{7 * tc.count, 7 * tc.count}, out.Usage)
		require.Len(t, out.Data, tc.count)
		for i, item := range out.Data {
			assert.Equal(t, i, item.Index)
			assert.Len(t, item.Embedding.([]any), 768)
		}
	}
	assert.Equal(t, []string{"search_query: literal", "search_query: literal", "search_document: other"}, e.inputs)
}

func TestHandlerRejectsBeforeInference(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"malformed JSON", `{`, 400},
		{"unknown model", `{"model":"other","input":"x"}`, 404},
		{"numeric input", `{"model":"nomic-embed-text","input":1}`, 400},
		{"empty input", `{"model":"nomic-embed-text","input":[]}`, 400},
		{"numeric array", `{"model":"nomic-embed-text","input":[1]}`, 400},
		{"null input", `{"model":"nomic-embed-text","input":null}`, 400},
		{"null array item", `{"model":"nomic-embed-text","input":[null]}`, 400},
		{"dimensions too large", `{"model":"nomic-embed-text","input":"x","dimensions":769}`, 400},
		{"unsupported encoding", `{"model":"nomic-embed-text","input":"x","encoding_format":"int8"}`, 400},
		{"too many inputs", `{"model":"nomic-embed-text","input":[` + strings.Repeat(`"x",`, 32) + `"x"]}`, 400},
		{"body too large", strings.Repeat("x", (4<<20)+1), 413},
	}
	e := new(fakeEngine)
	h := &handler{engine: e, gate: make(chan struct{}, 1)}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(tc.body)))
			assert.Equal(t, tc.status, recorder.Code)
		})
	}
	assert.Empty(t, e.inputs)
}

func TestHandlerRejectsIncompleteBody(t *testing.T) {
	e := new(fakeEngine)
	server := httptest.NewServer(&handler{engine: e, gate: make(chan struct{}, 1)})
	defer server.Close()
	conn, err := net.DialTCP("tcp", nil, server.Listener.Addr().(*net.TCPAddr))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprint(conn, "POST /v1/embeddings HTTP/1.1\r\nHost: example.com\r\nContent-Length: 100\r\n\r\n{")
	require.NoError(t, err)
	require.NoError(t, conn.CloseWrite())
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer res.Body.Close()
	assert.Equal(t, http.StatusBadRequest, res.StatusCode)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.Equal(t, "could not read request body\n", string(body))
	e.mu.Lock()
	defer e.mu.Unlock()
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
		v[0], v[767] = 3, 4
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
