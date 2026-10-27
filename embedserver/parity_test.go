package embedserver

import (
	"bytes"
	json "encoding/json/v2"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNativeParity uses public, synthetic Ollama outputs. Ordinary unit tests
// need no native runtime or network. CI sets both paths and runs this test on
// Linux, macOS, and Windows with cached, checksummed assets.
func TestNativeParity(t *testing.T) {
	dir, library := os.Getenv("KIT_EMBED_TEST_DIR"), os.Getenv("KIT_EMBED_TEST_LIBRARY")
	if dir == "" || library == "" {
		t.Skip("set KIT_EMBED_TEST_DIR and KIT_EMBED_TEST_LIBRARY for native parity")
	}
	data, err := os.ReadFile("testdata/ollama.json")
	require.NoError(t, err)
	var fixture struct {
		Samples []struct {
			Input  string    `json:"input"`
			Tokens int       `json:"tokens"`
			Vector []float32 `json:"vector"`
			IDs    []int     `json:"ids"`
		} `json:"samples"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.NotEmpty(t, fixture.Samples)
	e, err := loadModel(t.Context(), dir, library)
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	server := httptest.NewServer(&handler{engine: e, gate: make(chan struct{}, 1)})
	defer server.Close()
	for _, sample := range fixture.Samples {
		if len(sample.IDs) > 0 {
			assert.Equal(t, sample.IDs, e.(*model).tokenizer.encode(sample.Input), sample.Input)
		}
		body, err := json.Marshal(map[string]any{"model": "nomic-embed-text", "input": sample.Input})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/embeddings", bytes.NewReader(body))
		require.NoError(t, err)
		res, err := server.Client().Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)
		var out struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Usage usage `json:"usage"`
		}
		err = json.UnmarshalRead(res.Body, &out)
		res.Body.Close()
		require.NoError(t, err)
		assert.Equal(t, usage{sample.Tokens, sample.Tokens}, out.Usage)
		require.Len(t, out.Data, 1)
		actual := out.Data[0].Embedding
		require.Len(t, actual, 768)

		var dot, left, right float64
		for i, v := range actual {
			x, y := float64(v), float64(sample.Vector[i])
			dot += x * y
			left += x * x
			right += y * y
		}
		assert.GreaterOrEqual(t, dot/math.Sqrt(left*right), 0.999)
	}
}
