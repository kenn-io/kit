package vector_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

type fillProviderError struct {
	code int
}

func (e *fillProviderError) Error() string {
	return fmt.Sprintf("provider status %d", e.code)
}

func TestFillSharedErrorClassifierFailsClosedWithoutProbes(t *testing.T) {
	providerErr := &fillProviderError{code: 503}
	for _, tc := range []struct {
		name            string
		classifier      func(error) bool
		wantClassifiers int
	}{
		{name: "nil classifier"},
		{name: "classifier false", wantClassifiers: 1, classifier: func(err error) bool {
			var got *fillProviderError
			require.ErrorAs(t, err, &got)
			return false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			store.content = map[int64]string{1: "one", 2: "two", 3: "three"}
			var calls, classifiers, hooks int
			enc := func(context.Context, []string) ([][]float32, error) {
				calls++
				return nil, providerErr
			}
			classifier := tc.classifier
			if classifier != nil {
				classifier = func(err error) bool {
					classifiers++
					return tc.classifier(err)
				}
			}
			_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
				ScanBatch:               3,
				Batch:                   vector.BatchOptions{BatchSize: 3},
				ShouldIsolateBatchError: classifier,
				OnEncodeError: func(int64, error) bool {
					hooks++
					return true
				},
			})
			require.Error(t, err)
			var got *fillProviderError
			require.ErrorAs(t, err, &got)
			assert.Same(t, providerErr, got)
			assert.Equal(t, 1, calls)
			assert.Equal(t, tc.wantClassifiers, classifiers)
			assert.Zero(t, hooks)
		})
	}
}

func TestFillSharedErrorRejectedFirstProbeStopsDiagnosis(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(int64, error) bool
	}{
		{name: "nil hook"},
		{name: "false hook", hook: func(int64, error) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			store.content = map[int64]string{1: "poison one", 2: "poison two"}
			var calls, classifiers, hooks int
			enc := func(context.Context, []string) ([][]float32, error) {
				calls++
				return nil, &fillProviderError{code: 400}
			}
			hook := tc.hook
			if hook != nil {
				hook = func(doc int64, err error) bool {
					hooks++
					return tc.hook(doc, err)
				}
			}
			_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
				ScanBatch: 2,
				Batch:     vector.BatchOptions{BatchSize: 2},
				ShouldIsolateBatchError: func(error) bool {
					classifiers++
					return true
				},
				OnEncodeError: hook,
			})
			require.Error(t, err)
			var providerErr *fillProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, 2, calls, "one shared call plus the first probe")
			assert.Equal(t, 1, classifiers)
			assert.Equal(t, map[bool]int{true: 1, false: 0}[tc.hook != nil], hooks)
		})
	}
}

func TestFillSharedErrorAllowsTwoPoisonDocuments(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "poison one", 2: "poison two"}
	var calls int
	hooks := map[int64]int{}
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return nil, &fillProviderError{code: 400}
	}
	stats, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:               2,
		Batch:                   vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { return true },
		OnEncodeError: func(doc int64, _ error) bool {
			hooks[doc]++
			return true
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
	assert.Equal(t, map[int64]int{1: 1, 2: 1}, hooks)
	assert.Equal(t, 2, stats.Skipped)
	assert.Zero(t, stats.Documents)
}

func TestFillSharedInvalidVectorRejectedWithoutProbe(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad", 3: "later"}
	var calls, classifiers, hooks int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		return [][]float32{{1}, {0}, {1}}, nil
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch: 3,
		Batch:     vector.BatchOptions{BatchSize: 3},
		ShouldIsolateBatchError: func(error) bool {
			classifiers++
			return true
		},
		OnEncodeError: func(doc int64, err error) bool {
			hooks++
			assert.Equal(t, int64(2), doc)
			var invalid *vector.InvalidVectorError
			require.ErrorAs(t, err, &invalid)
			assert.Equal(t, 0, invalid.Chunk)
			return false
		},
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Zero(t, classifiers)
	assert.Equal(t, 1, hooks)
}

func TestFillSharedInvalidVectorNilHookRejectsWithoutProbe(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad"}
	var calls, classifiers int
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return [][]float32{{1}, {0}}, nil
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:               2,
		Batch:                   vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { classifiers++; return true },
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Zero(t, classifiers)
}

func TestFillSharedInvalidVectorRecoversOnlyOtherSlices(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad", 3: "later"}
	var calls [][]string
	hooks := map[int64]int{}
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls = append(calls, append([]string(nil), texts...))
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = []float32{1}
			if text == "bad" {
				out[i] = []float32{0}
			}
		}
		return out, nil
	}
	stats, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch: 3,
		Batch:     vector.BatchOptions{BatchSize: 3},
		OnEncodeError: func(doc int64, _ error) bool {
			hooks[doc]++
			return true
		},
	})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"good", "bad", "later"}, {"good"}, {"later"}}, calls)
	assert.Equal(t, map[int64]int{2: 1}, hooks)
	assert.Equal(t, 2, stats.Documents)
	assert.Equal(t, 1, stats.Skipped)
}

func TestFillSharedInvalidRecoveryFailureUsesProbeRules(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "bad", 2: "neighbor"}
	var calls, classifiers int
	hooks := map[int64]int{}
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			return [][]float32{{0}, {1}}, nil
		}
		return nil, &fillProviderError{code: 400}
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:               2,
		Batch:                   vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { classifiers++; return true },
		OnEncodeError: func(doc int64, _ error) bool {
			hooks[doc]++
			return doc == 1
		},
	})
	require.Error(t, err)
	var providerErr *fillProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, 2, calls)
	assert.Zero(t, classifiers, "recovery failures are never reclassified")
	assert.Equal(t, map[int64]int{1: 1, 2: 1}, hooks)
}

func TestFillSharedInvalidVectorOutOfRangeIsFatal(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "one", 2: "two"}
	var calls, classifiers, hooks int
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return nil, &vector.InvalidVectorError{Chunk: 2, Component: -1, Reason: "zero norm"}
	}
	_, err := vector.Fill(context.Background(), store, 7, enc, vector.FillOptions[int64]{
		ScanBatch:               2,
		Batch:                   vector.BatchOptions{BatchSize: 2},
		ShouldIsolateBatchError: func(error) bool { classifiers++; return true },
		OnEncodeError:           func(int64, error) bool { hooks++; return true },
	})
	require.ErrorContains(t, err, "invalid vector chunk 2 outside batch of 2 chunks")
	var invalid *vector.InvalidVectorError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, 1, calls)
	assert.Zero(t, classifiers)
	assert.Zero(t, hooks)
}
