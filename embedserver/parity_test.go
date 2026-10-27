package embedserver

import (
	"context"
	json "encoding/json/v2"
	"math"
	"os"
	"testing"
	"time"

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
	var longInput string
	var longDuration time.Duration
	for _, sample := range fixture.Samples {
		if len(sample.IDs) > 0 {
			assert.Equal(t, sample.IDs, e.(*model).tokenizer.encode(sample.Input), sample.Input)
		}
		started := time.Now()
		actual, tokens, err := e.embed(t.Context(), sample.Input)
		if sample.Tokens == 2048 {
			longInput, longDuration = sample.Input, time.Since(started)
		}
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
	defer cancel()
	require.NotEmpty(t, longInput)
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	started := time.Now()
	vector, _, err := e.embed(ctx, longInput)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, vector)
	assert.Less(t, time.Since(started), longDuration/2, "cancellation must interrupt native inference")
}
