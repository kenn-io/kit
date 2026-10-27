package embedserver

import (
	"context"
	json "encoding/json/v2"
	"math"
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
			Input  string
			Tokens int
			Vector []float32
		}
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	e, err := loadModel(t.Context(), dir, library)
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	for _, sample := range fixture.Samples {
		actual, tokens, err := e.embed(t.Context(), sample.Input)
		require.NoError(t, err)
		assert.Equal(t, sample.Tokens, tokens)
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = e.embed(ctx, "search_query: cancelled")
	require.ErrorIs(t, err, context.Canceled)
}
