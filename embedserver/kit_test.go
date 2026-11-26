package embedserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
		if in.MediaType != "" {
			return nil, embedmodel.ErrUnsupportedContent
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
		{"success", "POST", `{"model":"example","dimensions":2,"inputs":[{"role":"query","text":"hello"}]}`, 200, ""},
		{"base64", "POST", `{"model":"example","encoding_format":"base64","inputs":[{"role":"document","text":"hello"}]}`, 200, ""},
		{"unknown", "POST", `{"model":"other","inputs":[{"role":"query","text":"hello"}]}`, 404, "model_not_found"},
		{"role", "POST", `{"model":"example","inputs":[{"role":"bad","text":"hello"}]}`, 400, "invalid_request"},
		{"media", "POST", `{"model":"example","inputs":[{"role":"query","media_type":"image/png","data":"AQ=="}]}`, 415, "unsupported_content"},
		{"method", "GET", "", 405, "method_not_allowed"},
		{"both payloads", "POST", `{"model":"example","inputs":[{"role":"query","text":"hello","media_type":"image/png","data":"AQ=="}]}`, 400, "invalid_request"},
		{"empty", "POST", `{"model":"example","inputs":[{"role":"query"}]}`, 400, "invalid_request"},
		{"blank", "POST", `{"model":"example","inputs":[{"role":"query","text":"  "}]}`, 400, "invalid_request"},
		{"media without data", "POST", `{"model":"example","inputs":[{"role":"query","media_type":"image/png"}]}`, 400, "invalid_request"},
		{"null absent", "POST", `{"model":"example","inputs":[{"role":"query","text":"hello","data":null}]}`, 200, ""},
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
	_, err := local.Encode(t.Context(), 2, []embedclient.Input{{Role: embedconfig.RoleDocument, MediaType: "image/png", Data: []byte{1}}})
	assert.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
}

type captureBackend struct{ inputs []embedclient.Input }

func (b *captureBackend) Encode(_ context.Context, _ int, in []embedclient.Input) ([][]float32, error) {
	b.inputs = in
	return [][]float32{{1, 0}}, nil
}
func TestKitHandlerPreservesText(t *testing.T) {
	backend := &captureBackend{}
	req := httptest.NewRequest("POST", "/v1/embed", bytes.NewBufferString(`{"model":"example","inputs":[{"role":"query","text":"query: hello"}]}`))
	w := httptest.NewRecorder()
	Handler(backend, []string{"example"}).ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	assert.Equal(t, []embedclient.Input{{Role: embedconfig.RoleQuery, Text: "query: hello"}}, backend.inputs)
}

func TestKitBackendsDirectAndHTTP(t *testing.T) {
	for _, kind := range []string{"openai", "vertex", "local"} {
		t.Run(kind, func(t *testing.T) {
			var backend embedclient.Backend
			var seen []string
			var localEngine *fakeEngine
			modelName := "example"
			if kind == "local" {
				localEngine = &fakeEngine{}
				backend = newLocal(localEngine)
			} else {
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					w.Header().Set("Content-Type", "application/json")
					if kind == "vertex" {
						parts := req["content"].(map[string]any)["parts"].([]any)
						if text, ok := parts[0].(map[string]any)["text"].(string); ok {
							seen = append(seen, text)
						}
						_, _ = w.Write([]byte(`{"embedding":{"values":[0.6,0.8]}}`))
						return
					}
					inputs := req["input"].([]any)
					for _, text := range inputs {
						seen = append(seen, text.(string))
					}
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
			inputs := []embedclient.Input{{Role: embedconfig.RoleQuery, Text: "  query\ntext  "}, {Role: embedconfig.RoleDocument, Text: "  document\ntext  "}}
			direct, err := backend.Encode(t.Context(), 2, inputs)
			require.NoError(t, err)
			server := httptest.NewServer(Handler(backend, []string{modelName}))
			t.Cleanup(server.Close)
			client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: modelName, Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Roles: embedconfig.Roles{QueryFormatter: "as-given"}, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
			require.NoError(t, err)
			viaHTTP, err := client.Encode(t.Context(), 2, inputs)
			require.NoError(t, err)
			assert.Equal(t, direct, viaHTTP)
			if localEngine != nil {
				seen = localEngine.inputs
			}
			assert.Equal(t, []string{"  query\ntext  ", "  document\ntext  ", "  query\ntext  ", "  document\ntext  "}, seen)
			media := []embedclient.Input{{Role: embedconfig.RoleDocument, MediaType: "image/png", Data: []byte{1}}}
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
	failures := embedclient.Check(t.Context(), embedclient.CheckOptions{BaseURL: record.Endpoint().BaseURL() + "/v1", Model: "nomic-embed-text", Dimensions: 2})
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
		{"wrong encoding", "base64 encoding", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var b bytes.Buffer
			_, _ = b.ReadFrom(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b.Bytes()))
			if bytes.Contains(b.Bytes(), []byte(`"base64"`)) {
				var req struct {
					Inputs []embedclient.Input `json:"inputs"`
				}
				_ = json.Unmarshal(b.Bytes(), &req)
				data := make([]map[string]any, len(req.Inputs))
				for i := range data {
					data[i] = map[string]any{"index": i, "embedding": []float32{0.6, 0.8}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
				return
			}
			Handler(testBackend{}, []string{"example"}).ServeHTTP(w, r)
		})},
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
	r := httptest.NewRequestWithContext(ctx, "POST", "/v1/embed", strings.NewReader(`{"model":"example","inputs":[{"role":"query","text":"hello"}]}`))
	w := httptest.NewRecorder()
	Handler(canceledBackend{}, []string{"example"}).ServeHTTP(w, r)
	assert.Empty(t, w.Body.String())
}
func TestKitHandlerLenientJSON(t *testing.T) {
	backend := &captureBackend{}
	body := append([]byte(`{"model":"wrong","model":"example","inputs":[{"role":"query","text":"a`), 0xff)
	body = append(body, []byte(`b"}]}`)...)
	w := httptest.NewRecorder()
	Handler(backend, []string{"example"}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/embed", bytes.NewReader(body)))
	require.Equal(t, 200, w.Code)
	assert.Equal(t, "a\ufffdb", backend.inputs[0].Text)
}

type errorBackend struct{ err error }

func (b errorBackend) Encode(context.Context, int, []embedclient.Input) ([][]float32, error) {
	return nil, b.err
}
func TestKitBackendErrorClasses(t *testing.T) {
	cases := []struct {
		name                         string
		err                          error
		status                       int
		reason                       embedclient.Reason
		rejected, credentials, retry bool
		delay                        time.Duration
	}{
		{"policy", &embedclient.APIError{StatusCode: 400, Reason: embedclient.ReasonContentPolicy}, 400, embedclient.ReasonContentPolicy, true, false, false, 0},
		{"long", &embedclient.APIError{StatusCode: 413, Reason: embedclient.ReasonInputTooLong}, 413, embedclient.ReasonInputTooLong, true, false, false, 0},
		{"credentials", &embedclient.APIError{StatusCode: 401, Reason: embedclient.ReasonCredentials}, 502, embedclient.ReasonCredentials, false, true, false, 0},
		{"rate", &embedclient.APIError{StatusCode: 429, Reason: embedclient.ReasonRateLimited, RetryAfter: 3 * time.Second}, 429, embedclient.ReasonRateLimited, false, false, true, 3 * time.Second},
		{"timeout", context.DeadlineExceeded, 503, embedclient.ReasonUnavailable, false, false, true, 0},
		{"load", &modelLoadError{errors.New("missing model")}, 424, embedclient.ReasonInvalidRequest, false, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(Handler(errorBackend{tc.err}, []string{"example"}))
			t.Cleanup(server.Close)
			client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "example", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
			require.NoError(t, err)
			_, err = client.Encode(t.Context(), 2, []embedclient.Input{{Role: embedconfig.RoleQuery, Text: "hello"}})
			var got *embedclient.APIError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, tc.status, got.StatusCode)
			assert.Equal(t, tc.reason, got.Reason)
			assert.Equal(t, tc.rejected, got.InputRejected())
			assert.Equal(t, tc.credentials, got.CredentialsRejected())
			assert.Equal(t, tc.retry, got.Retryable())
			assert.Equal(t, tc.delay, got.RetryAfter)
		})
	}
}
func TestKitRejectsTruncatedLocalInput(t *testing.T) {
	tok := &tokenizer{vocab: map[string]int{"[CLS]": 101, "[SEP]": 102, "[UNK]": 100, "▁a": 1}, specials: map[string]int{}, maxPiece: 4}
	assert.Len(t, tok.encodeLimit(strings.Repeat("a ", 2046), 2049), 2048)
	assert.Len(t, tok.encodeLimit(strings.Repeat("a ", 2047), 2049), 2049)
	local := newLocal(&models{text: &model{tokenizer: tok}})
	input := embedclient.Input{Role: embedconfig.RoleDocument, Text: strings.Repeat("a ", 2047)}
	_, err := local.Encode(t.Context(), 2, []embedclient.Input{input})
	var api *embedclient.APIError
	require.ErrorAs(t, err, &api)
	assert.Equal(t, embedclient.ReasonInputTooLong, api.Reason)
	body, err := json.Marshal(map[string]any{"model": "example", "inputs": []embedclient.Input{input}})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	Handler(local, []string{"example"}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/embed", bytes.NewReader(body)))
	assert.Equal(t, 413, w.Code)
}

func TestKitLocalLongNative(t *testing.T) {
	dir, library := os.Getenv("KIT_EMBED_TEST_DIR"), os.Getenv("KIT_EMBED_TEST_LIBRARY")
	if dir == "" || library == "" {
		t.Skip("set KIT_EMBED_TEST_DIR and KIT_EMBED_TEST_LIBRARY")
	}
	local, err := NewLocal(t.Context(), dir, library)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, local.Close()) })
	input := embedclient.Input{Role: embedconfig.RoleDocument, Text: strings.Repeat("hello ", 2047)}
	body, err := json.Marshal(map[string]any{"model": "example", "inputs": []embedclient.Input{input}})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	Handler(local, []string{"example"}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/embed", bytes.NewReader(body)))
	assert.Equal(t, 413, w.Code)
	oldBody, err := json.Marshal(map[string]any{"model": "nomic-embed-text", "input": input.Text, "dimensions": 2})
	require.NoError(t, err)
	old := httptest.NewRecorder()
	local.h.ServeHTTP(old, httptest.NewRequest("POST", "/v1/embeddings", bytes.NewReader(oldBody)))
	assert.Equal(t, 200, old.Code)
}

func TestKitLogsModelLoadFailure(t *testing.T) {
	local := newLocal(&fakeEngine{run: func(context.Context, string) ([]float32, int, error) {
		return nil, 0, &modelLoadError{errors.New("missing model")}
	}})
	var log bytes.Buffer
	local.h.log = &log
	w := httptest.NewRecorder()
	Handler(local, []string{"example"}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/embed", strings.NewReader(`{"model":"example","inputs":[{"role":"query","text":"hello"}]}`)))
	assert.Equal(t, 424, w.Code)
	assert.Contains(t, log.String(), "missing model")
}

func TestKitUpstreamMissingModel(t *testing.T) {
	assert.True(t, (&embedclient.APIError{StatusCode: http.StatusUnauthorized}).CredentialsRejected())
	assert.True(t, (&embedclient.APIError{StatusCode: http.StatusForbidden}).CredentialsRejected())
	assert.False(t, (&embedclient.APIError{StatusCode: http.StatusForbidden, Reason: embedclient.ReasonInvalidRequest}).CredentialsRejected())
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"model_not_found","message":"model not found"}}`))
	}))
	t.Cleanup(provider.Close)
	model := embedconfig.Model{Name: "example", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}
	upstream, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: provider.URL}})
	require.NoError(t, err)
	server := httptest.NewServer(Handler(upstream, []string{"example"}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	_, err = client.Encode(t.Context(), 2, []embedclient.Input{{Role: embedconfig.RoleQuery, Text: "hello"}})
	var api *embedclient.APIError
	require.ErrorAs(t, err, &api)
	assert.Equal(t, http.StatusBadRequest, api.StatusCode)
	assert.Equal(t, embedclient.ReasonInvalidRequest, api.Reason)
	assert.False(t, api.CredentialsRejected())
	assert.False(t, api.Retryable())
}

type vectorBackend struct{ vectors [][]float32 }

func (b vectorBackend) Encode(context.Context, int, []embedclient.Input) ([][]float32, error) {
	return b.vectors, nil
}
func TestKitRejectsInvalidVectors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vectors [][]float32
	}{
		{"zero", [][]float32{{0, 0}}}, {"wrong count", nil}, {"wrong size", [][]float32{{1}}}, {"nan", [][]float32{{float32(math.NaN()), 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			Handler(vectorBackend{tc.vectors}, []string{"example"}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/embed", strings.NewReader(`{"model":"example","dimensions":2,"inputs":[{"role":"query","text":"a"}]}`)))
			assert.Equal(t, 500, w.Code)
			assert.Contains(t, w.Body.String(), "internal_error")
		})
	}
}

// independentService implements the wire contract without using Handler.
func independentService(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		fail(405, "method_not_allowed")
		return
	}
	var req struct {
		Model      string `json:"model"`
		Dimensions int    `json:"dimensions"`
		Encoding   string `json:"encoding_format"`
		Inputs     []struct {
			Role      string `json:"role"`
			Text      string `json:"text"`
			MediaType string `json:"media_type"`
			Data      []byte `json:"data"`
		} `json:"inputs"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Model == "" || len(req.Inputs) == 0 {
		fail(400, "invalid_request")
		return
	}
	if req.Model != "example" {
		fail(404, "model_not_found")
		return
	}
	for _, in := range req.Inputs {
		if in.Role != "query" && in.Role != "document" {
			fail(400, "invalid_request")
			return
		}
		if in.MediaType != "" {
			fail(415, "unsupported_content")
			return
		}
		if in.Text == "" {
			fail(400, "invalid_request")
			return
		}
	}
	data := make([]map[string]any, len(req.Inputs))
	for i := range data {
		var vec any = []float32{0.6, 0.8}
		if req.Encoding == "base64" {
			var raw [8]byte
			binary.LittleEndian.PutUint32(raw[:4], math.Float32bits(0.6))
			binary.LittleEndian.PutUint32(raw[4:], math.Float32bits(0.8))
			vec = base64.StdEncoding.EncodeToString(raw[:])
		}
		data[i] = map[string]any{"index": i, "embedding": vec}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "data": data})
}
func TestKitCheckIndependentService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(independentService))
	t.Cleanup(server.Close)
	assert.Empty(t, embedclient.Check(t.Context(), embedclient.CheckOptions{BaseURL: server.URL, Model: "example", Dimensions: 2}))
}

func TestKitCheckRejectsFaultyIndependentServices(t *testing.T) {
	for _, kind := range []string{"batch encoding", "zero", "index", "size"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body bytes.Buffer
				_, _ = body.ReadFrom(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
				var req struct {
					Encoding string            `json:"encoding_format"`
					Inputs   []json.RawMessage `json:"inputs"`
				}
				_ = json.Unmarshal(body.Bytes(), &req)
				response := httptest.NewRecorder()
				independentService(response, r)
				if response.Code != 200 {
					for k, values := range response.Header() {
						w.Header()[k] = values
					}
					w.WriteHeader(response.Code)
					_, _ = w.Write(response.Body.Bytes())
					return
				}
				var out struct {
					Model string           `json:"model"`
					Data  []map[string]any `json:"data"`
				}
				_ = json.Unmarshal(response.Body.Bytes(), &out)
				switch kind {
				case "batch encoding":
					if req.Encoding == "base64" && len(req.Inputs) > 1 {
						out.Data[1]["embedding"] = []float32{0.6, 0.8}
					}
				case "zero":
					out.Data[0]["embedding"] = []float32{0, 0}
				case "index":
					out.Data[0]["index"] = 99
				case "size":
					out.Data[0]["embedding"] = []float32{1}
				}
				_ = json.NewEncoder(w).Encode(out)
			}))
			t.Cleanup(server.Close)
			failures := embedclient.Check(t.Context(), embedclient.CheckOptions{BaseURL: server.URL, Model: "example", Dimensions: 2})
			assert.NotEmpty(t, failures)
		})
	}
}
