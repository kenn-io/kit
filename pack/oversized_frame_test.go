package pack

import (
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A blob larger than zstd.MaxWindowSize must survive a round trip.
//
// encodeFrame used to compress everything with WithSingleSegment(true). A
// single-segment frame carries no window descriptor, so a decoder derives the
// window from the frame content size — and both decoders in this package
// capped windows at zstd.MaxWindowSize (512 MiB). Blobs above that were
// therefore written in a form this package itself could not read, failing with
// "window size exceeded". Nothing was corrupt: the same bytes decode correctly
// once a larger window is permitted.
//
// Compressible input keeps the test's memory and runtime modest while still
// producing a frame whose declared content size crosses the limit.
func TestEncodeFrameRoundTripsBlobsOverMaxWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates more than 512 MiB")
	}

	tests := []struct {
		name string
		size int
	}{
		{"just under the window limit", zstd.MaxWindowSize - 1},
		{"exactly the window limit", zstd.MaxWindowSize},
		{"one byte over the window limit", zstd.MaxWindowSize + 1},
		{"well over the window limit", zstd.MaxWindowSize + (170 << 20)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := make([]byte, tt.size)
			for i := range raw {
				raw[i] = byte(i % 251) // compressible, but not a single run
			}

			stored, compressed := encodeFrame(raw, DefaultZstdLevel)
			require.True(t, compressed, "input should compress")

			var header zstd.Header
			require.NoError(t, header.Decode(stored))
			if tt.size > zstd.MaxWindowSize {
				assert.False(t, header.SingleSegment,
					"blobs over the window limit must not use single-segment framing")
			}

			decoded, err := decodeFrame(stored, compressed, uint64(len(raw)))
			require.NoError(t, err, "a blob this package wrote must be readable")
			assert.Equal(t, len(raw), len(decoded))
			assert.Equal(t, raw, decoded)
		})
	}
}
