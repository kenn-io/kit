package vector

import (
	"context"
	"fmt"
	"sync"
)

// Vector is a single embedding.
type Vector []float32

// EncodeFunc turns a batch of texts into one vector each, in the same
// order. Implementations own the model or API client and any retry or
// backoff policy, since retryability is provider-specific.
type EncodeFunc func(ctx context.Context, texts []string) ([][]float32, error)

// BatchOptions controls how EncodeBatched groups and parallelizes calls.
type BatchOptions struct {
	// BatchSize is the maximum number of chunks passed to EncodeFunc in a
	// single call. Values <= 0 send every chunk in one call.
	BatchSize int
	// Concurrency bounds how many EncodeFunc calls run at once. Values
	// <= 0 mean one call at a time.
	Concurrency int
}

// EncodeBatched splits chunks into batches, invokes enc with bounded
// concurrency, and returns one Vector per input chunk in input order. It
// stops launching work at the first error or when ctx is cancelled, and
// reports the first error encountered.
func EncodeBatched(ctx context.Context, enc EncodeFunc, chunks []Chunk, o BatchOptions) ([]Vector, error) {
	if enc == nil {
		return nil, fmt.Errorf("encode func is nil")
	}
	if len(chunks) == 0 {
		return nil, nil
	}

	batchSize := o.BatchSize
	if batchSize <= 0 {
		batchSize = len(chunks)
	}
	concurrency := o.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	out := make([]Vector, len(chunks))
	sem := make(chan struct{}, concurrency)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

launch:
	for start := 0; start < len(chunks); start += batchSize {
		if ctx.Err() != nil {
			setErr(ctx.Err())
			break
		}
		if failed() {
			break
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			setErr(ctx.Err())
			break launch
		}
		if ctx.Err() != nil {
			<-sem
			setErr(ctx.Err())
			break
		}
		if failed() {
			<-sem
			break
		}

		end := min(start+batchSize, len(chunks))
		texts := make([]string, end-start)
		for i, c := range chunks[start:end] {
			texts[i] = c.Text
		}

		wg.Add(1)
		go func(start int, texts []string) {
			defer wg.Done()
			defer func() { <-sem }()

			vecs, err := enc(ctx, texts)
			if err != nil {
				setErr(fmt.Errorf("encode batch at %d: %w", start, err))
				return
			}
			if len(vecs) != len(texts) {
				setErr(fmt.Errorf("encode batch at %d: got %d vectors for %d texts", start, len(vecs), len(texts)))
				return
			}
			// Each batch owns a disjoint index range, so writes to out
			// never overlap across goroutines.
			for i, v := range vecs {
				out[start+i] = Vector(v)
			}
		}(start, texts)
	}

	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
