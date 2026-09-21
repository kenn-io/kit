package vector_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

func TestFillDocumentLimitStopsAfterStartedDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	for id := int64(1); id <= 5; id++ {
		store.content[id] = "doc"
	}

	stats, err := vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillScanBatch[int64](2),
		vector.WithFillDocumentLimit[int64](3),
	)
	require.NoError(err)
	assert.Equal(3, stats.Documents)
	assert.Len(store.embedded, 3)
	for id := int64(1); id <= 3; id++ {
		assert.True(store.embedded[id][1], "document %d was started", id)
	}
	assert.Nil(store.embedded[4])
	assert.Nil(store.embedded[5])

	stats, err = vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillDocumentLimit[int64](3),
	)
	require.NoError(err)
	assert.Equal(2, stats.Documents)
	assert.True(store.embedded[4][1])
	assert.True(store.embedded[5][1])
}

func TestFillDocumentLimitCountsAStaleDocumentAsStarted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.revision = map[int64]int{1: 1, 2: 1}
	store.content[1] = "one"
	store.content[2] = "two"
	var progressed []int64
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		store.revision[1] = 9
		return lenEncoder()(ctx, texts)
	}

	stats, err := vector.Fill(t.Context(), store, 1, enc,
		vector.WithFillDocumentLimit[int64](1),
		vector.WithFillProgress[int64](func(progress vector.FillProgress[int64]) {
			progressed = append(progressed, progress.Doc)
		}),
	)
	require.NoError(err)
	assert.Equal(1, stats.Stale)
	assert.Zero(stats.Documents)
	assert.Empty(progressed, "a stale save is not progress")
	assert.Nil(store.embedded[1])
	assert.Nil(store.embedded[2])
}

func TestFillProgressReportsSavedChunks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.content[1] = "cat"
	store.content[2] = "   "
	var got []vector.FillProgress[int64]

	stats, err := vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillProgress[int64](func(progress vector.FillProgress[int64]) {
			got = append(got, progress)
		}),
	)
	require.NoError(err)
	assert.Equal(2, stats.Documents)
	require.Len(got, 2)
	assert.Equal(int64(1), got[0].Doc)
	assert.Equal(1, got[0].Vectors)
	require.Len(got[0].Chunks, 1)
	assert.Equal("cat", got[0].Chunks[0].Text)
	assert.Nil(got[0].Chunks[0].Span)
	assert.Equal(int64(2), got[1].Doc)
	assert.Zero(got[1].Vectors)
	assert.Empty(got[1].Chunks)
}

func TestFillPreparedChunksKeepSourceSpanAndEncoderText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newMemStore()
	store.content[1] = "source"
	store.content[2] = "other"
	var encoded []string
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		return lenEncoder()(ctx, texts)
	}
	span := &vector.SourceSpan{Start: 0, End: 6}
	var progressed []vector.FillProgress[int64]

	stats, err := vector.Fill(t.Context(), store, 1, enc,
		vector.WithFillBatch[int64](vector.WithBatchSize(10)),
		vector.WithFillPrepared[int64](func(pending vector.Pending[int64]) ([]vector.PreparedChunk, error) {
			if pending.Doc == 2 {
				return nil, nil
			}
			return []vector.PreparedChunk{{
				Index:     4,
				Text:      "heading\n" + pending.Content,
				Span:      span,
				Truncated: true,
			}}, nil
		}),
		vector.WithFillProgress[int64](func(progress vector.FillProgress[int64]) {
			progressed = append(progressed, progress)
		}),
	)
	require.NoError(err)
	assert.Equal(2, stats.Documents)
	assert.Equal(1, stats.Chunks)
	assert.Equal([]string{"heading\nsource"}, encoded)
	require.Len(store.vectors[1][1], 1)
	assert.Equal(4, store.vectors[1][1][0].ChunkIndex)
	assert.Empty(store.vectors[1][2], "an empty prepared list is a stamp-only save")
	assert.True(store.embedded[2][1])

	require.Len(progressed, 2)
	assert.Equal(int64(1), progressed[0].Doc)
	require.Len(progressed[0].Chunks, 1)
	assert.Equal("heading\nsource", progressed[0].Chunks[0].Text)
	assert.Equal(span, progressed[0].Chunks[0].Span)
	assert.True(progressed[0].Chunks[0].Truncated)
	assert.Equal(4, progressed[0].Chunks[0].Index)
	assert.Zero(progressed[1].Vectors)
}

func TestFillPreparedErrorStampsNothing(t *testing.T) {
	require := require.New(t)
	store := newMemStore()
	store.content[1] = "one"
	store.content[2] = "two"
	prepareErr := errors.New("prepare failed")
	calls := 0
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return nil, nil
	}

	_, err := vector.Fill(t.Context(), store, 1, enc,
		vector.WithFillPrepared[int64](func(pending vector.Pending[int64]) ([]vector.PreparedChunk, error) {
			if pending.Doc == 2 {
				return nil, prepareErr
			}
			return []vector.PreparedChunk{{Index: 0, Text: pending.Content}}, nil
		}),
	)
	require.ErrorIs(err, prepareErr)
	assert.Zero(t, calls)
	assert.Empty(t, store.embedded)
}

func TestFillPreparedBlankTextIsAnEncodeError(t *testing.T) {
	require := require.New(t)
	store := newMemStore()
	store.content[1] = "source"
	_, err := vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillPrepared[int64](func(vector.Pending[int64]) ([]vector.PreparedChunk, error) {
			return []vector.PreparedChunk{{Index: 0, Text: " \n"}}, nil
		}),
	)
	require.ErrorIs(err, vector.ErrEmptyEmbeddingInput)
	assert.Empty(t, store.embedded)
}

func TestFillNegativeDocumentLimitDoesNotStopEarly(t *testing.T) {
	require := require.New(t)
	store := newMemStore()
	store.content[1] = "a"
	store.content[2] = "b"
	stats, err := vector.Fill(t.Context(), store, 1, lenEncoder(),
		vector.WithFillDocumentLimit[int64](-1),
	)
	require.NoError(err)
	assert.Equal(t, 2, stats.Documents)
}
