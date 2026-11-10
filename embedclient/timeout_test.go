package embedclient_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jsonv2 "encoding/json/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

func timeoutClient(t *testing.T, handler http.HandlerFunc, retry embedclient.Retry) *embedclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	model := unitModel()
	model.Normalization = embedconfig.NormalizationNone
	client, err := embedclient.New(embedclient.Options{
		Model: model, Deployment: embedconfig.Deployment{BaseURL: srv.URL},
		HTTP: &http.Client{Timeout: 40 * time.Millisecond}, Retry: retry, //nolint:kennlint // deadlinetest: the client timeout drives batch splitting
	})
	require.NoError(t, err)
	return client
}

func TestTimeoutSplitsBatches(t *testing.T) {
	for _, method := range []string{"Embed", "EmbedTexts", "EncodeFunc"} {
		t.Run(method, func(t *testing.T) {
			var large atomic.Int32
			client := timeoutClient(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Input []string `json:"input"`
				}
				if !assert.NoError(t, jsonv2.UnmarshalRead(r.Body, &body)) {
					return
				}
				if len(body.Input) > 2 {
					large.Add(1)
					time.Sleep(120 * time.Millisecond) //nolint:kennlint // sleeptest: the handler exceeds the client timeout
					return
				}
				data := make([]map[string]any, len(body.Input))
				for i, input := range body.Input {
					var n int
					_, err := fmt.Sscanf(input, "item%d", &n)
					if !assert.NoError(t, err) {
						return
					}
					data[i] = map[string]any{"index": i, "embedding": []float32{float32(n + 1), 1}}
				}
				writeJSON(t, w, map[string]any{"data": data})
			}, embedclient.Retry{MaxAttempts: 2, InitialBackoff: time.Millisecond})
			texts := []string{"item0", "item1", "item2", "item3", "item4", "item5", "item6", "item7"}
			var vectors [][]float32
			var err error
			switch method {
			case "Embed":
				inputs := make([]embedmodel.Content, len(texts))
				for i, text := range texts {
					inputs[i] = embedmodel.Content{Role: embedconfig.RoleDocument, Text: text}
				}
				vectors, err = client.Embed(t.Context(), inputs)
			case "EmbedTexts":
				vectors, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, texts)
			case "EncodeFunc":
				vectors, err = client.EncodeFunc(embedconfig.RoleDocument)(t.Context(), texts)
			}
			require.NoError(t, err)
			require.Len(t, vectors, len(texts))
			for i, vec := range vectors {
				assert.Equal(t, []float32{float32(i + 1), 1}, vec)
			}
			assert.Equal(t, int32(3), large.Load())
		})
	}
}

func TestSingleTimeoutUsesRetryAndReportsTimeout(t *testing.T) {
	var calls atomic.Int32
	client := timeoutClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(120 * time.Millisecond) //nolint:kennlint // sleeptest: the handler exceeds the client timeout
	}, embedclient.Retry{MaxAttempts: 2, InitialBackoff: time.Millisecond})
	_, err := client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"slow"})
	var transport *embedclient.TransportError
	require.ErrorAs(t, err, &transport)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var timeout interface{ Timeout() bool }
	require.ErrorAs(t, err, &timeout)
	assert.True(t, timeout.Timeout())
	assert.Equal(t, "embed request timed out", transport.Error())
	assert.Equal(t, int32(2), calls.Load())
}

func TestCallerDeadlineDoesNotSplit(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	client, err := embedclient.New(embedclient.Options{
		Model:      unitModel(),
		Deployment: embedconfig.Deployment{BaseURL: "http://embed.test/v1", TrustPrivateNetwork: true},
		HTTP:       &http.Client{Transport: transport},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond) //nolint:kennlint // deadlinetest: the caller's deadline is the expected result; the transport blocks until it expires
	defer cancel()
	_, err = client.EmbedTexts(ctx, embedconfig.RoleDocument, []string{"a", "b"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int32(1), calls.Load(), "an expired caller deadline must not split the batch")
}

func TestTimeoutSplitMapsBadVectorIndex(t *testing.T) {
	client := timeoutClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, jsonv2.UnmarshalRead(r.Body, &body)) {
			return
		}
		if len(body.Input) > 2 {
			time.Sleep(120 * time.Millisecond) //nolint:kennlint // sleeptest: the handler exceeds the client timeout
			return
		}
		data := make([]map[string]any, len(body.Input))
		for i, input := range body.Input {
			vec := []float32{1, 0}
			if input == "g" {
				vec = []float32{0, 0}
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		writeJSON(t, w, map[string]any{"data": data})
	}, embedclient.Retry{})
	inputs := []embedmodel.Content{{Role: embedconfig.RoleQuery, Text: "query"}}
	for _, text := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		inputs = append(inputs, embedmodel.Content{Role: embedconfig.RoleDocument, Text: text})
	}
	_, err := client.Embed(t.Context(), inputs)
	var vectorErr *embedclient.VectorError
	require.ErrorAs(t, err, &vectorErr)
	assert.Equal(t, 7, vectorErr.Index)
}

func TestOllamaRecoveryTimeoutDoesNotSplitTheBatch(t *testing.T) {
	var primary, native atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/embeddings":
			primary.Add(1)
			body := `{"data":[{"index":0,"embedding":[3,4]},{"index":1,"embedding":[null,1]}]}`
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case "/api/ps":
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"models":[]}`)), Request: r}, nil
		default:
			native.Add(1)
			return nil, os.ErrDeadlineExceeded
		}
	})
	client, err := embedclient.New(embedclient.Options{
		Model:               unitModel(),
		Deployment:          embedconfig.Deployment{BaseURL: "http://ollama.test/v1", TrustPrivateNetwork: true},
		HTTP:                &http.Client{Transport: transport},
		Batch:               embedconfig.Batch{Items: 4},
		OllamaMetalRecovery: true,
	})
	require.NoError(t, err)
	_, err = client.EmbedTexts(t.Context(), embedconfig.RoleDocument, []string{"alpha", "beta"})
	var timeout interface{ Timeout() bool }
	require.ErrorAs(t, err, &timeout)
	assert.Equal(t, int32(1), primary.Load(), "a recovery timeout must not resend the batch")
	assert.Equal(t, int32(3), native.Load(), "recovery runs its own unload, reload, and CPU attempts once")
}
