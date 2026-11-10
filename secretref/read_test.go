package secretref

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadSecretEnforcesLimitWithoutStat(t *testing.T) {
	for _, size := range []int{0, 1, 64 << 10, (64 << 10) + 1, 128 << 10} {
		reader := strings.NewReader(strings.Repeat("s", size))
		data, err := readSecret(t.Context(), reader)
		if size > 64<<10 {
			require.Error(t, err)
			assert.Nil(t, data)
			assert.Equal(t, size-((64<<10)+1), reader.Len())
		} else {
			require.NoError(t, err)
			assert.Equal(t, strings.Repeat("s", size), string(data))
		}
	}
}

func TestReadSecretDropsPartialDataOnError(t *testing.T) {
	reader := io.MultiReader(strings.NewReader("partial-secret"), iotest.ErrReader(errors.New("private-path")))
	data, err := readSecret(t.Context(), reader)
	require.EqualError(t, err, "file is unreadable")
	assert.Nil(t, data)
}

func TestContextReaderCancellationBetweenReads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := contextReader{ctx: ctx, reader: strings.NewReader("ab")}
	buf := make([]byte, 1)
	n, err := r.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	cancel()
	n, err = r.Read(buf)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, n)
	_, err = io.ReadAll(r.reader)
	require.NoError(t, err)
}
