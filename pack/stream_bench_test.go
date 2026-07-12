package pack

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkPrepareAndAppendStream(b *testing.B) {
	contents := map[string][]byte{
		"raw-1MiB":        benchmarkNoise(1 << 20),
		"compressed-1MiB": bytes.Repeat([]byte("stream benchmark "), (1<<20)/17),
	}
	for name, content := range contents {
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			var scratch, stored uint64
			for range b.N {
				writer, err := NewWriter(dir, WriterOptions{})
				require.NoError(b, err)
				prepared, err := PrepareBlob(context.Background(), bytes.NewReader(content), uint64(len(content)), DefaultZstdLevel, AppendStreamOptions{ScratchDir: dir})
				require.NoError(b, err)
				scratch += prepared.ScratchBytes()
				stored += prepared.StoredLen()
				_, err = writer.AppendPrepared(context.Background(), prepared)
				require.NoError(b, err)
				require.NoError(b, writer.Abort())
			}
			if b.N > 0 {
				b.ReportMetric(float64(scratch)/float64(b.N), "scratch-bytes/op")
				raw := uint64(len(content)) * uint64(b.N)
				b.ReportMetric(float64(raw+scratch+2*stored)/float64(raw), "copy-amplification")
			}
		})
	}
}

func benchmarkNoise(size int) []byte {
	result := make([]byte, size)
	var state uint32 = 1
	for i := range result {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		result[i] = byte(state)
	}
	return result
}
