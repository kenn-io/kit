package vector_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

// memStore is an in-memory Store[int64, int] used to exercise the flows
// without any real backend. Documents are keyed by int64; generations by
// int. QueryGeneration ranks by cosine similarity over stored vectors.
// Setting revision enables optimistic-concurrency checks in SaveVectors.
type memStore struct {
	content  map[int64]string
	revision map[int64]int                          // nil disables revision tracking
	embedded map[int64]map[int]bool                 // doc -> gen -> done
	vectors  map[int]map[int64][]vector.ChunkVector // gen -> doc -> chunks
	live     []int                                  // descending preference
}

func newMemStore() *memStore {
	return &memStore{
		content:  map[int64]string{},
		embedded: map[int64]map[int]bool{},
		vectors:  map[int]map[int64][]vector.ChunkVector{},
	}
}

func (m *memStore) PendingForGeneration(_ context.Context, gen int, limit int) ([]vector.Pending[int64], error) {
	keys := make([]int64, 0, len(m.content))
	for doc := range m.content {
		if !m.embedded[doc][gen] {
			keys = append(keys, doc)
		}
	}
	slices.Sort(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]vector.Pending[int64], len(keys))
	for i, doc := range keys {
		out[i] = vector.Pending[int64]{Doc: doc, Content: m.content[doc]}
		if m.revision != nil {
			out[i].Revision = m.revision[doc]
		}
	}
	return out, nil
}

func (m *memStore) SaveVectors(_ context.Context, gen int, doc int64, revision any, vecs []vector.ChunkVector) error {
	if m.revision != nil && revision != any(m.revision[doc]) {
		return vector.ErrStale
	}
	if m.vectors[gen] == nil {
		m.vectors[gen] = map[int64][]vector.ChunkVector{}
	}
	m.vectors[gen][doc] = vecs
	if m.embedded[doc] == nil {
		m.embedded[doc] = map[int]bool{}
	}
	m.embedded[doc][gen] = true
	return nil
}

func (m *memStore) LiveGenerations(_ context.Context) ([]int, error) {
	return m.live, nil
}

func (m *memStore) QueryGeneration(_ context.Context, gen int, query vector.Vector, limit int) ([]vector.Hit[int64], error) {
	var hits []vector.Hit[int64]
	for doc, chunks := range m.vectors[gen] {
		for _, cv := range chunks {
			hits = append(hits, vector.Hit[int64]{Doc: doc, ChunkIndex: cv.ChunkIndex, Score: cosine(query, cv.Vector)})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func cosine(a, b vector.Vector) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// lenEncoder embeds each text as a 1-D vector of its rune length, enough
// to confirm Fill wired chunk content through to SaveVectors.
func lenEncoder() vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			out[i] = []float32{float32(len([]rune(txt)))}
		}
		return out, nil
	}
}

// textEncoder makes both chunk content and order visible in saved vectors.
func textEncoder() vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			runes := []rune(text)
			out[i] = []float32{float32(len(runes)), float32(runes[0])}
		}
		return out, nil
	}
}

func TestFillEmbedsAllPendingThenStops(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "alpha"
	store.content[2] = "beta gamma delta"

	stats, err := vector.Fill(ctx, store, 7, lenEncoder(), vector.FillOptions[int64]{
		ScanBatch: 1, // force multiple scan rounds
		Split:     vector.SplitOptions{MaxRunes: 4, Overlap: 0},
	})
	require.NoError(err)

	assert.Equal(2, stats.Documents)
	assert.True(store.embedded[1][7] && store.embedded[2][7], "both docs stamped for gen 7")
	require.Len(store.vectors[7][1], 2, "alpha -> 2 chunks of <=4 runes")
	assert.InDelta(4, store.vectors[7][1][0].Vector[0], 1e-6, "first chunk carries its rune length")

	// A second run finds nothing pending and embeds zero documents.
	again, err := vector.Fill(ctx, store, 7, lenEncoder(), vector.FillOptions[int64]{})
	require.NoError(err)
	assert.Equal(0, again.Documents)
}

func TestFillBatchesChunksAcrossDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	for doc := int64(1); doc <= 7; doc++ {
		store.content[doc] = strings.Repeat("x", int(doc))
	}

	var batchSizes []int
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		batchSizes = append(batchSizes, len(texts))
		return lenEncoder()(ctx, texts)
	}

	stats, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   7,
		Batch:       vector.BatchOptions{BatchSize: 3, Concurrency: 1},
		Concurrency: 1,
	})
	require.NoError(err)

	assert.Equal([]int{3, 3, 1}, batchSizes,
		"BatchSize should pack chunks from adjacent documents into each encode call")
	assert.Equal(7, stats.Documents)
	assert.Equal(7, stats.Chunks)
	for doc := int64(1); doc <= 7; doc++ {
		require.Len(store.vectors[7][doc], 1)
		assert.InDelta(float64(doc), float64(store.vectors[7][doc][0].Vector[0]), 1e-6,
			"doc %d keeps the vector for its own content", doc)
	}
}

func TestFillCrossDocumentBatchingMatchesPerDocumentVectors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	contents := map[int64]string{
		1: "abcde",
		2: "uvwxyz",
		3: "kit",
	}

	baseline := newMemStore()
	batched := newMemStore()
	for doc, content := range contents {
		baseline.content[doc] = content
		batched.content[doc] = content
	}
	options := vector.FillOptions[int64]{
		ScanBatch: 3,
		Split:     vector.SplitOptions{MaxRunes: 2},
	}
	baselineStats, err := vector.Fill(ctx, baseline, 7, textEncoder(), options)
	require.NoError(err)

	var batchSizes []int
	batchedEncoder := func(ctx context.Context, texts []string) ([][]float32, error) {
		batchSizes = append(batchSizes, len(texts))
		return textEncoder()(ctx, texts)
	}
	options.Batch = vector.BatchOptions{BatchSize: 3, Concurrency: 1}
	options.Concurrency = 1
	batchedStats, err := vector.Fill(ctx, batched, 7, batchedEncoder, options)
	require.NoError(err)

	assert.Equal([]int{3, 3, 2}, batchSizes,
		"document boundaries do not leave partially filled encode calls")
	assert.Equal(baselineStats, batchedStats)
	assert.Equal(baseline.vectors, batched.vectors,
		"cross-document scatter must preserve every document and chunk vector")
}

func TestFillCrossDocumentBatchingMatchesLegacyAcrossConfigurations(t *testing.T) {
	ctx := context.Background()
	contents := map[int64]string{
		1: "",
		2: "a",
		3: "βeta",
		4: "kit batches neighboring chunks",
		5: "世界世界世界",
	}

	baseline := newMemStore()
	for doc, content := range contents {
		baseline.content[doc] = content
	}
	baselineStats, err := vector.Fill(ctx, baseline, 7, textEncoder(), vector.FillOptions[int64]{
		ScanBatch: 5,
		Split:     vector.SplitOptions{MaxRunes: 3, Overlap: 1},
	})
	require.NoError(t, err)

	for _, scanBatch := range []int{1, 2, 5} {
		for _, batchSize := range []int{1, 2, 3, 8} {
			for _, batchConcurrency := range []int{0, 1, 2} {
				for _, fillConcurrency := range []int{0, 1, 3} {
					name := fmt.Sprintf("scan=%d/batch=%d/batch-concurrency=%d/fill-concurrency=%d",
						scanBatch, batchSize, batchConcurrency, fillConcurrency)
					t.Run(name, func(t *testing.T) {
						store := newMemStore()
						for doc, content := range contents {
							store.content[doc] = content
						}

						stats, err := vector.Fill(ctx, store, 7, textEncoder(), vector.FillOptions[int64]{
							ScanBatch: scanBatch,
							Split:     vector.SplitOptions{MaxRunes: 3, Overlap: 1},
							Batch: vector.BatchOptions{
								BatchSize:   batchSize,
								Concurrency: batchConcurrency,
							},
							Concurrency: fillConcurrency,
						})
						require.NoError(t, err)
						assert.Equal(t, baselineStats, stats)
						assert.Equal(t, baseline.vectors, store.vectors)
					})
				}
			}
		}
	}
}

func TestFillCrossDocumentBatchingIsolatesPoisonDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "fine one"
	store.content[2] = "poison"
	store.content[3] = "fine two"

	var batchSizes []int
	base := poisonEncoder()
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		batchSizes = append(batchSizes, len(texts))
		return base(ctx, texts)
	}
	var skipped []int64
	stats, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   3,
		Batch:       vector.BatchOptions{BatchSize: 3, Concurrency: 1},
		Concurrency: 1,
		OnEncodeError: func(doc int64, _ error) bool {
			skipped = append(skipped, doc)
			return true
		},
	})
	require.NoError(err)

	assert.Equal([]int{3, 1, 1, 1}, batchSizes,
		"only a failed cross-document batch is retried at document granularity")
	assert.Equal([]int64{2}, skipped, "the hook is consulted only for the poison document")
	assert.Equal(2, stats.Documents)
	assert.Equal(1, stats.Skipped)
	assert.True(store.embedded[1][7])
	assert.True(store.embedded[2][7])
	assert.Empty(store.vectors[7][2], "the poison document is stamp-only skipped")
	assert.True(store.embedded[3][7])
}

func TestFillCrossDocumentBatchingTranslatesInvalidVectorChunkIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "x"
	store.content[2] = "abc"
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = []float32{1}
			if text == "c" {
				out[i] = []float32{0}
			}
		}
		return out, nil
	}

	var gotInvalid *vector.InvalidVectorError
	stats, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   2,
		Split:       vector.SplitOptions{MaxRunes: 1},
		Batch:       vector.BatchOptions{BatchSize: 2, Concurrency: 1},
		Concurrency: 1,
		OnEncodeError: func(doc int64, err error) bool {
			assert.Equal(int64(2), doc)
			require.ErrorAs(err, &gotInvalid)
			return true
		},
	})
	require.NoError(err)
	require.NotNil(gotInvalid)

	assert.Equal(2, gotInvalid.Chunk,
		"the index is relative to the failed document, not the later encode batch")
	assert.Equal(1, stats.Documents)
	assert.Equal(1, stats.Skipped)
}

func TestFillCrossDocumentBatchingLeavesOnlyChangedDocumentPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "alpha"
	store.content[2] = "beta"
	store.revision = map[int64]int{1: 1, 2: 1}
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			if text == "alpha" {
				store.revision[1]++
			}
			out[i] = []float32{1}
		}
		return out, nil
	}

	stats, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   2,
		Batch:       vector.BatchOptions{BatchSize: 2, Concurrency: 1},
		Concurrency: 1,
	})
	require.NoError(err)

	assert.Equal(1, stats.Documents)
	assert.Equal(1, stats.Stale)
	assert.False(store.embedded[1][7])
	assert.True(store.embedded[2][7], "a stale batch mate does not affect this document")
}

func TestFillCrossDocumentBatchingStampsEmptyDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = ""
	store.content[2] = "alpha"
	store.content[3] = ""
	var calls atomic.Int64
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		calls.Add(1)
		return lenEncoder()(ctx, texts)
	}

	stats, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   3,
		Batch:       vector.BatchOptions{BatchSize: 3, Concurrency: 1},
		Concurrency: 1,
	})
	require.NoError(err)

	assert.Equal(int64(1), calls.Load())
	assert.Equal(3, stats.Documents)
	assert.Equal(1, stats.Chunks)
	for doc := int64(1); doc <= 3; doc++ {
		assert.True(store.embedded[doc][7], "doc %d is stamped", doc)
	}
	assert.Empty(store.vectors[7][1])
	assert.Empty(store.vectors[7][3])
}

func TestFillCrossDocumentBatchingEncodeErrorAbortsAtFailedDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "fine one"
	store.content[2] = "poison"
	store.content[3] = "fine two"
	var consulted []int64
	_, err := vector.Fill(ctx, store, 7, poisonEncoder(), vector.FillOptions[int64]{
		ScanBatch:   3,
		Batch:       vector.BatchOptions{BatchSize: 3, Concurrency: 1},
		Concurrency: 1,
		OnEncodeError: func(doc int64, _ error) bool {
			consulted = append(consulted, doc)
			return false
		},
	})
	require.ErrorContains(err, "encode document 2")

	assert.Equal([]int64{2}, consulted)
	assert.True(store.embedded[1][7], "the preceding good document is saved")
	assert.False(store.embedded[2][7], "the failed document stays pending")
	assert.False(store.embedded[3][7], "a later document stays pending after abort")
}

func TestFillCrossDocumentBatchingAbortsUnattributedBatchError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "fine one"
	store.content[2] = "fine two"
	store.content[3] = "fine three"
	var batchSizes []int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		batchSizes = append(batchSizes, len(texts))
		if len(texts) > 1 {
			return nil, errors.New("request shape rejected")
		}
		return [][]float32{{1}}, nil
	}
	called := false
	_, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   3,
		Batch:       vector.BatchOptions{BatchSize: 3, Concurrency: 1},
		Concurrency: 1,
		OnEncodeError: func(int64, error) bool {
			called = true
			return true
		},
	})
	require.ErrorContains(err, "no document failed in isolation")

	assert.Equal([]int{3, 1, 1, 1}, batchSizes)
	assert.False(called, "a batch-shape failure is not attributed to an arbitrary document")
	for doc := int64(1); doc <= 3; doc++ {
		assert.False(store.embedded[doc][7], "doc %d remains pending", doc)
	}
}

func TestFillCrossDocumentBatchingDoesNotSkipCancelledEncode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "alpha"
	store.content[2] = "beta"
	called := false
	enc := func(context.Context, []string) ([][]float32, error) {
		return nil, context.Canceled
	}
	_, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   2,
		Batch:       vector.BatchOptions{BatchSize: 2, Concurrency: 1},
		Concurrency: 1,
		OnEncodeError: func(int64, error) bool {
			called = true
			return true
		},
	})
	require.ErrorIs(err, context.Canceled)

	assert.False(called, "cancellation bypasses the permanent-error hook")
	assert.False(store.embedded[1][7])
	assert.False(store.embedded[2][7])
}

func TestFillLeavesChangedDocumentPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "alpha"
	store.content[2] = "beta"
	store.revision = map[int64]int{1: 1, 2: 1}

	// This encoder simulates a concurrent edit: doc 1's revision is bumped
	// after the scan read its content but before SaveVectors stamps it.
	racingEnc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			if txt == "alpha" {
				store.revision[1]++
			}
			out[i] = []float32{1}
		}
		return out, nil
	}

	stats, err := vector.Fill(ctx, store, 7, racingEnc, vector.FillOptions[int64]{})
	require.NoError(err)
	assert.Equal(1, stats.Documents, "the unchanged doc is embedded")
	assert.Equal(1, stats.Stale, "the changed doc is reported stale")
	assert.False(store.embedded[1][7], "a doc that changed mid-fill is not stamped")
	assert.True(store.embedded[2][7])

	// The next run re-reads the document at its new revision and succeeds.
	again, err := vector.Fill(ctx, store, 7, lenEncoder(), vector.FillOptions[int64]{})
	require.NoError(err)
	assert.Equal(1, again.Documents)
	assert.True(store.embedded[1][7])
}

func TestFillSkipHookStampsFailedDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "poison"
	store.content[2] = "fine"

	var skipped []int64
	stats, err := vector.Fill(ctx, store, 7, poisonEncoder(), vector.FillOptions[int64]{
		OnEncodeError: func(doc int64, err error) bool {
			skipped = append(skipped, doc)
			return true
		},
	})
	require.NoError(err)
	assert.Equal(1, stats.Documents)
	assert.Equal(1, stats.Skipped)
	assert.Equal([]int64{1}, skipped)
	assert.True(store.embedded[1][7], "skipped doc is stamped so it stops being pending")
	assert.Empty(store.vectors[7][1], "skipped doc has no vectors")
	assert.True(store.embedded[2][7])

	again, err := vector.Fill(ctx, store, 7, poisonEncoder(), vector.FillOptions[int64]{})
	require.NoError(err)
	assert.Equal(0, again.Documents, "a stamped skip does not reappear as pending")
}

func TestFillEncodeErrorAbortsWithoutSkip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "poison"

	_, err := vector.Fill(ctx, store, 7, poisonEncoder(), vector.FillOptions[int64]{})
	require.ErrorContains(err, "encode document")

	_, err = vector.Fill(ctx, store, 7, poisonEncoder(), vector.FillOptions[int64]{
		OnEncodeError: func(int64, error) bool { return false },
	})
	require.ErrorContains(err, "encode document")
	assert.False(store.embedded[1][7], "an aborted doc is neither embedded nor stamped")
}

func TestFillDoesNotSkipCancelledEncode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "alpha"

	called := false
	enc := func(context.Context, []string) ([][]float32, error) {
		return nil, context.Canceled
	}
	_, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		OnEncodeError: func(int64, error) bool {
			called = true
			return true
		},
	})
	require.ErrorIs(err, context.Canceled)
	assert.False(called, "cancellation bypasses the permanent-error skip hook")
	assert.False(store.embedded[1][7], "a cancelled document is not stamped as handled")
}

func TestFillConcurrencyEncodesDocumentsInParallel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	const workers = 4
	store := newMemStore()
	for doc := int64(1); doc <= workers; doc++ {
		store.content[doc] = strings.Repeat("x", int(doc))
	}

	// Barrier encoder: every call parks until all four documents are in
	// flight at once, so the test fails with the timeout error below
	// (instead of hanging) if Fill regresses to sequential encodes.
	release := make(chan struct{})
	var arrived atomic.Int32
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		if arrived.Add(1) == workers {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return nil, errors.New("barrier timed out: encodes did not overlap")
		}
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			out[i] = []float32{float32(len([]rune(txt)))}
		}
		return out, nil
	}

	stats, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		Concurrency: workers,
	})
	require.NoError(err)

	assert.Equal(workers, stats.Documents)
	assert.Equal(workers, stats.Chunks)
	for doc := int64(1); doc <= workers; doc++ {
		require.True(store.embedded[doc][7], "doc %d stamped", doc)
		require.Len(store.vectors[7][doc], 1)
		assert.InDelta(float64(doc), float64(store.vectors[7][doc][0].Vector[0]), 1e-6,
			"doc %d keeps its own vector under concurrent encodes", doc)
	}
}

// saveHookStore runs hook before delegating each SaveVectors to memStore,
// so a test can observe or stretch the save window.
type saveHookStore struct {
	*memStore
	hook func()
}

type firstSaveErrorStore struct {
	*memStore
	err   error
	saves int
}

func (s *firstSaveErrorStore) SaveVectors(
	ctx context.Context, gen int, doc int64, revision any, vecs []vector.ChunkVector,
) error {
	s.saves++
	if s.saves == 1 {
		return s.err
	}
	return s.memStore.SaveVectors(ctx, gen, doc, revision, vecs)
}

func (s *saveHookStore) SaveVectors(ctx context.Context, gen int, doc int64, revision any, vecs []vector.ChunkVector) error {
	s.hook()
	return s.memStore.SaveVectors(ctx, gen, doc, revision, vecs)
}

// TestFillDefaultConcurrencyIsSequential pins the Concurrency <= 0 contract:
// the next document's encode must not begin until the previous document's
// save has returned, so no encoder/API call is ever made ahead of a save
// that may abort the fill. The save window is stretched slightly so a
// pipelined implementation (one encode kept in flight during the save)
// reliably trips the overlap flag; a sequential one runs encode and save on
// one goroutine and can never overlap.
func TestFillDefaultConcurrencyIsSequential(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()

	store := newMemStore()
	for doc := int64(1); doc <= 6; doc++ {
		store.content[doc] = strings.Repeat("x", int(doc))
	}

	var inSave atomic.Bool
	var overlapped atomic.Bool
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		if inSave.Load() {
			overlapped.Store(true)
		}
		return lenEncoder()(ctx, texts)
	}
	hooked := &saveHookStore{memStore: store, hook: func() {
		inSave.Store(true)
		time.Sleep(5 * time.Millisecond)
		inSave.Store(false)
	}}

	stats, err := vector.Fill(ctx, hooked, 7, enc, vector.FillOptions[int64]{})
	require.NoError(err)
	require.Equal(6, stats.Documents)
	require.False(overlapped.Load(),
		"an encode began while a save was still running: Concurrency <= 0 must be strictly sequential")
}

func TestFillCrossDocumentBatchingDoesNotEncodeNextWindowAfterSaveFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	base := newMemStore()
	for doc := int64(1); doc <= 7; doc++ {
		base.content[doc] = strings.Repeat("x", int(doc))
	}
	sentinel := errors.New("save failed")
	store := &firstSaveErrorStore{memStore: base, err: sentinel}
	var calls atomic.Int64
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		calls.Add(1)
		return lenEncoder()(ctx, texts)
	}

	_, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:   7,
		Batch:       vector.BatchOptions{BatchSize: 3, Concurrency: 1},
		Concurrency: 1,
	})
	require.ErrorIs(err, sentinel)

	assert.Equal(int64(1), calls.Load(),
		"the next chunk window must not start after the first window reaches a failing save")
}

func TestFillCrossDocumentBatchingComposesConcurrencyBounds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	const maxCalls = 4
	store := newMemStore()
	for doc := int64(1); doc <= 8; doc++ {
		store.content[doc] = strings.Repeat("x", int(doc))
	}

	release := make(chan struct{})
	var inFlight, observedMax atomic.Int64
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		current := inFlight.Add(1)
		for {
			previous := observedMax.Load()
			if current <= previous || observedMax.CompareAndSwap(previous, current) {
				break
			}
		}
		if current == maxCalls {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return nil, errors.New("barrier timed out: batched encodes did not overlap")
		}
		defer inFlight.Add(-1)
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1}
		}
		return out, nil
	}

	stats, err := vector.Fill(ctx, store, 7, enc, vector.FillOptions[int64]{
		ScanBatch: 8,
		Batch: vector.BatchOptions{
			BatchSize:   2,
			Concurrency: 2,
		},
		Concurrency: 2,
	})
	require.NoError(err)

	assert.Equal(int64(maxCalls), observedMax.Load(),
		"Fill and batch concurrency compose without exceeding their product")
	assert.Equal(8, stats.Documents)
	assert.Equal(8, stats.Chunks)
}

func TestFillConcurrencySkipHookStampsFailedDocument(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "poison"
	store.content[2] = "fine"
	store.content[3] = "also fine"

	var skipped []int64
	stats, err := vector.Fill(ctx, store, 7, poisonEncoder(), vector.FillOptions[int64]{
		Concurrency: 3,
		OnEncodeError: func(doc int64, err error) bool {
			skipped = append(skipped, doc)
			return true
		},
	})
	require.NoError(err)
	assert.Equal(2, stats.Documents)
	assert.Equal(1, stats.Skipped)
	assert.Equal([]int64{1}, skipped)
	assert.True(store.embedded[1][7], "skipped doc is stamped so it stops being pending")
	assert.Empty(store.vectors[7][1], "skipped doc has no vectors")
	assert.True(store.embedded[2][7])
	assert.True(store.embedded[3][7])
}

func TestFillConcurrencyEncodeErrorAborts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	store := newMemStore()
	store.content[1] = "poison"
	for doc := int64(2); doc <= 8; doc++ {
		store.content[doc] = "fine"
	}

	_, err := vector.Fill(ctx, store, 7, poisonEncoder(), vector.FillOptions[int64]{
		Concurrency: 4,
	})
	require.ErrorContains(err, "encode document")
	assert.False(store.embedded[1][7], "the failed doc is neither embedded nor stamped")

	// The failed document stays pending: a later run with a working encoder
	// picks up everything the aborted page left behind.
	again, err := vector.Fill(ctx, store, 7, lenEncoder(), vector.FillOptions[int64]{
		Concurrency: 4,
	})
	require.NoError(err)
	assert.True(store.embedded[1][7])
	assert.Equal(0, again.Stale)
}

// poisonEncoder fails any batch containing the text "poison".
func poisonEncoder() vector.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, txt := range texts {
			if strings.Contains(txt, "poison") {
				return nil, errors.New("input rejected by model")
			}
			out[i] = []float32{1}
		}
		return out, nil
	}
}

func TestSearchRollsUpAndPrefersBuildingGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	const active, building = 7, 9
	store := newMemStore()
	store.live = []int{building, active} // descending preference

	// Doc 1 is shared; active stored it at chunk 0, building at chunk 5.
	store.SaveVectors(ctx, active, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}})
	store.SaveVectors(ctx, active, 2, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0, 1}}})
	store.SaveVectors(ctx, building, 1, nil, []vector.ChunkVector{{ChunkIndex: 5, Vector: vector.Vector{1, 0}}})
	store.SaveVectors(ctx, building, 3, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}) // new, building-only

	// Query vector [1,0] points at docs 1 and 3.
	queryEnc := func(int) vector.EncodeFunc {
		return func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range texts {
				out[i] = []float32{1, 0}
			}
			return out, nil
		}
	}

	got, err := vector.Search(ctx, store, "q", queryEnc, vector.SearchOptions{})
	require.NoError(err)

	byDoc := map[int64]vector.Hit[int64]{}
	for _, h := range got {
		byDoc[h.Doc] = h
	}
	assert.Contains(byDoc, int64(1))
	assert.Contains(byDoc, int64(2), "active-only doc is not dropped (union coverage)")
	assert.Contains(byDoc, int64(3), "building-only new doc is searchable mid-migration")
	assert.Equal(5, byDoc[1].ChunkIndex, "shared doc keeps the building generation's hit")
}

func TestSearchErrorsWhenNoEncoderForGeneration(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	store.live = []int{1}
	store.SaveVectors(ctx, 1, 1, nil, []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1}}})

	_, err := vector.Search(ctx, store, "q", func(int) vector.EncodeFunc { return nil }, vector.SearchOptions{})
	assert.ErrorContains(t, err, "no encoder")
}
