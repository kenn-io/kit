package vector_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

func chunks(texts ...string) []vector.Chunk {
	out := make([]vector.Chunk, len(texts))
	for i, txt := range texts {
		out[i] = vector.Chunk{Index: i, Text: txt}
	}
	return out
}

// echoEncoder returns one vector per text whose single component encodes
// the text length, so results can be matched back to their input order.
func echoEncoder(record func(batch []string)) vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		if record != nil {
			record(texts)
		}
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			out[i] = []float32{float32(len(txt))}
		}
		return out, nil
	}
}

func TestEncodeBatchedPreservesOrderAcrossBatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var mu sync.Mutex
	var sizes []int
	enc := echoEncoder(func(batch []string) {
		mu.Lock()
		sizes = append(sizes, len(batch))
		mu.Unlock()
	})

	in := chunks("a", "bb", "ccc", "dddd", "eeeee")
	out, err := vector.EncodeBatched(context.Background(), enc, in, vector.BatchOptions{BatchSize: 2, Concurrency: 3})
	require.NoError(err)
	require.Len(out, len(in))
	for i, c := range in {
		assert.Equal(float32(len(c.Text)), out[i][0], "vector %d matches its input", i)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.ElementsMatch([]int{2, 2, 1}, sizes, "batches are sized by BatchSize")
}

func TestEncodeBatchedRespectsConcurrencyBound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var inFlight, maxInFlight atomic.Int64
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		out := make([][]float32, len(texts))
		return out, nil
	}

	in := chunks("a", "b", "c", "d", "e", "f", "g", "h")
	_, err := vector.EncodeBatched(context.Background(), enc, in, vector.BatchOptions{BatchSize: 1, Concurrency: 2})
	require.NoError(err)
	assert.LessOrEqual(maxInFlight.Load(), int64(2), "never exceeds the concurrency bound")
}

func TestEncodeBatchedSurfacesEncodeError(t *testing.T) {
	assert := assert.New(t)
	sentinel := errors.New("boom")
	enc := func(_ context.Context, _ []string) ([][]float32, error) { return nil, sentinel }

	_, err := vector.EncodeBatched(context.Background(), enc, chunks("a", "b"), vector.BatchOptions{BatchSize: 1})
	assert.ErrorIs(err, sentinel)
}

func TestEncodeBatchedRejectsCountMismatch(t *testing.T) {
	assert := assert.New(t)
	enc := func(_ context.Context, _ []string) ([][]float32, error) {
		return [][]float32{{1}}, nil // one vector for two texts
	}

	_, err := vector.EncodeBatched(context.Background(), enc, chunks("a", "b"), vector.BatchOptions{})
	assert.ErrorContains(err, "vectors for")
}

func TestEncodeBatchedNilEncoder(t *testing.T) {
	_, err := vector.EncodeBatched(context.Background(), nil, chunks("a"), vector.BatchOptions{})
	assert.Error(t, err)
}

func TestEncodeBatchedEmptyInput(t *testing.T) {
	assert := assert.New(t)
	out, err := vector.EncodeBatched(context.Background(), echoEncoder(nil), nil, vector.BatchOptions{})
	assert.NoError(err)
	assert.Empty(out)
}

func TestEncodeBatchedStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := vector.EncodeBatched(ctx, echoEncoder(nil), chunks("a", "b"), vector.BatchOptions{BatchSize: 1})
	assert.ErrorIs(t, err, context.Canceled)
}
