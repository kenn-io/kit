package embedserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

type testBackend struct{}

func (testBackend) Encode(_ context.Context, _ int, inputs []embedclient.Input) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		for _, p := range in.Parts {
			if p.MediaType != "" {
				return nil, embedmodel.ErrUnsupportedContent
			}
		}
		out[i] = []float32{0.6, 0.8}
	}
	return out, nil
}
func TestKitHandler(t *testing.T) {
	handler := Handler(testBackend{}, []string{"example"})
	for _, tc := range []struct {
		name, method, body string
		status             int
		code               string
	}{
		{"success", "POST", `{"model":"example","dimensions":2,"inputs":[{"role":"query","parts":[{"text":"hello"}]}]}`, 200, ""},
		{"base64", "POST", `{"model":"example","encoding_format":"base64","inputs":[{"role":"document","parts":[{"text":"hello"}]}]}`, 200, ""},
		{"unknown", "POST", `{"model":"other","inputs":[{"role":"query","parts":[{"text":"hello"}]}]}`, 404, "model_not_found"},
		{"role", "POST", `{"model":"example","inputs":[{"role":"bad","parts":[{"text":"hello"}]}]}`, 400, "invalid_request"},
		{"media", "POST", `{"model":"example","inputs":[{"role":"query","parts":[{"media_type":"image/png","data":"AQ=="}]}]}`, 415, "unsupported_content"},
		{"method", "GET", "", 405, "method_not_allowed"},
		{"mixed part", "POST", `{"model":"example","inputs":[{"role":"query","parts":[{"text":"hello","data":""}]}]}`, 400, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/v1/embed", bytes.NewBufferString(tc.body))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, tc.status, w.Code)
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			if tc.code != "" {
				assert.Equal(t, tc.code, body["error"].(map[string]any)["code"])
			}
		})
	}
}
func TestKitCheck(t *testing.T) {
	server := httptest.NewServer(Handler(testBackend{}, []string{"example"}))
	t.Cleanup(server.Close)
	failures := embedclient.Check(t.Context(), embedclient.CheckOptions{BaseURL: server.URL, Model: "example", Dimensions: 2})
	assert.Empty(t, failures)
}
func TestKitLocalMedia(t *testing.T) {
	local := newLocal(nil)
	_, err := local.Encode(t.Context(), 2, []embedclient.Input{{Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: []byte{1}}}}})
	assert.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
}

type captureBackend struct{ inputs []embedclient.Input }

func (b *captureBackend) Encode(_ context.Context, _ int, in []embedclient.Input) ([][]float32, error) {
	b.inputs = in
	return [][]float32{{1, 0}}, nil
}
func TestKitHandlerPreservesText(t *testing.T) {
	backend := &captureBackend{}
	req := httptest.NewRequest("POST", "/v1/embed", bytes.NewBufferString(`{"model":"example","inputs":[{"role":"query","parts":[{"text":"query: hello"}]}]}`))
	w := httptest.NewRecorder()
	Handler(backend, []string{"example"}).ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	assert.Equal(t, []embedclient.Input{{Role: embedconfig.RoleQuery, Parts: []embedclient.Part{{Text: "query: hello"}}}}, backend.inputs)
}

func TestKitBackendsDirectAndHTTP(t *testing.T) {
	for _, kind := range []string{"openai", "vertex", "local"} {
		t.Run(kind, func(t *testing.T) {
			var backend embedclient.Backend
			modelName := "example"
			if kind == "local" {
				backend = newLocal(&fakeEngine{})
			} else {
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					w.Header().Set("Content-Type", "application/json")
					if kind == "vertex" {
						_, _ = w.Write([]byte(`{"embedding":{"values":[0.6,0.8]}}`))
						return
					}
					inputs := req["input"].([]any)
					data := make([]map[string]any, len(inputs))
					for i := range inputs {
						data[i] = map[string]any{"index": i, "embedding": []float32{0.6, 0.8}}
					}
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
				}))
				t.Cleanup(provider.Close)
				api := embedconfig.APIOpenAI
				base := provider.URL
				if kind == "vertex" {
					api = embedconfig.APIVertex
					base += "/publishers/google/models"
					modelName = "gemini-embedding-2"
				}
				client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: modelName, Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Roles: embedconfig.Roles{QueryFormatter: "as-given"}, Deployment: embedconfig.Deployment{API: api, BaseURL: base}})
				require.NoError(t, err)
				backend = client
			}
			inputs := []embedclient.Input{{Role: embedconfig.RoleQuery, Parts: []embedclient.Part{{Text: "query text"}}}, {Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{Text: "document text"}}}}
			direct, err := backend.Encode(t.Context(), 2, inputs)
			require.NoError(t, err)
			server := httptest.NewServer(Handler(backend, []string{modelName}))
			t.Cleanup(server.Close)
			client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: modelName, Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Roles: embedconfig.Roles{QueryFormatter: "as-given"}, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
			require.NoError(t, err)
			viaHTTP, err := client.Encode(t.Context(), 2, inputs)
			require.NoError(t, err)
			assert.Equal(t, direct, viaHTTP)
			media := []embedclient.Input{{Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: []byte{1}}}}}
			_, directErr := backend.Encode(t.Context(), 2, media)
			_, httpErr := client.Encode(t.Context(), 2, media)
			if kind == "vertex" {
				assert.NoError(t, directErr)
				assert.NoError(t, httpErr)
			} else {
				assert.ErrorIs(t, directErr, embedmodel.ErrUnsupportedContent)
				assert.ErrorIs(t, httpErr, embedmodel.ErrUnsupportedContent)
			}
			assert.Empty(t, embedclient.Check(t.Context(), embedclient.CheckOptions{BaseURL: server.URL, Model: modelName, Dimensions: 2}))
		})
	}
}

func TestKitServeCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	_, record, done := startServe(t, ctx, t.TempDir(), new(fakeEngine), daemon.Endpoint.ListenContext)
	failures := embedclient.Check(t.Context(), embedclient.CheckOptions{BaseURL: record.Endpoint().BaseURL() + "/v1", Model: "nomic-embed-text", Dimensions: 2, OpenAI: true})
	cancel()
	require.NoError(t, <-done)
	assert.Empty(t, failures)
}

type batchBackend struct{}

func (batchBackend) Encode(_ context.Context, _ int, in []embedclient.Input) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{float32(len(in)), 1}
	}
	return out, nil
}
func TestKitCheckDetectsBrokenServices(t *testing.T) {
	for _, tc := range []struct {
		name, rule string
		handler    http.Handler
	}{
		{"batch", "batch consistency", Handler(batchBackend{}, []string{"example"})},
		{"model", "unknown model", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" {
				var b bytes.Buffer
				_, _ = b.ReadFrom(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(b.Bytes()))
				if bytes.Contains(b.Bytes(), []byte("example-unknown")) {
					_, _ = w.Write([]byte(`{"data":[]}`))
					return
				}
			}
			Handler(testBackend{}, []string{"example"}).ServeHTTP(w, r)
		})},
		{"dimensions", "text batch", Handler(testBackend{}, []string{"example"})},
		{"base64", "base64 consistency", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var b bytes.Buffer
			_, _ = b.ReadFrom(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b.Bytes()))
			if bytes.Contains(b.Bytes(), []byte(`"base64"`)) {
				_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[1,0]}]}`))
				return
			}
			Handler(testBackend{}, []string{"example"}).ServeHTTP(w, r)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			t.Cleanup(server.Close)
			dims := 2
			if tc.name == "dimensions" {
				dims = 3
			}
			failures := embedclient.Check(t.Context(), embedclient.CheckOptions{BaseURL: server.URL, Model: "example", Dimensions: dims})
			found := false
			for _, err := range failures {
				if strings.Contains(err.Error(), tc.rule) {
					found = true
				}
			}
			assert.True(t, found, "failures: %v", failures)
		})
	}
}

type canceledBackend struct{}

func (canceledBackend) Encode(ctx context.Context, _ int, _ []embedclient.Input) ([][]float32, error) {
	return nil, ctx.Err()
}
func TestKitHandlerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequestWithContext(ctx, "POST", "/v1/embed", strings.NewReader(`{"model":"example","inputs":[{"role":"query","parts":[{"text":"hello"}]}]}`))
	w := httptest.NewRecorder()
	Handler(canceledBackend{}, []string{"example"}).ServeHTTP(w, r)
	assert.Empty(t, w.Body.String())
}
func TestKitHandlerLenientJSON(t *testing.T) {
	backend := &captureBackend{}
	body := append([]byte(`{"model":"wrong","model":"example","inputs":[{"role":"query","parts":[{"text":"a`), 0xff)
	body = append(body, []byte(`b"}]}]}`)...)
	w := httptest.NewRecorder()
	Handler(backend, []string{"example"}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/embed", bytes.NewReader(body)))
	require.Equal(t, 200, w.Code)
	assert.Equal(t, "a\ufffdb", backend.inputs[0].Parts[0].Text)
}
