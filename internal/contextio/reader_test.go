package contextio

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaderStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &cancelingReader{cancel: cancel}
	_, err := io.Copy(io.Discard, &Reader{Context: ctx, Reader: source})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, source.reads)
}

type cancelingReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	r.reads++
	p[0] = '#'
	r.cancel()
	return 1, nil
}
