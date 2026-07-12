package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

type benchmarkContentSource struct{ content []byte }

func (s benchmarkContentSource) Open(context.Context, ContentRef) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.content)), nil
}

func BenchmarkBackupCaptureStream(b *testing.B) {
	contents := map[string][]byte{
		"raw-1MiB":        backupBenchmarkNoise(1 << 20),
		"compressed-1MiB": bytes.Repeat([]byte("backup stream benchmark "), (1<<20)/24),
	}
	for name, content := range contents {
		b.Run(name, func(b *testing.B) {
			id := pack.ComputeBlobID(content)
			ref := ContentRef{Hash: id.String(), Size: int64(len(content))}
			root := b.TempDir()
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			b.ResetTimer()
			for i := range b.N {
				repo, err := Init(filepath.Join(root, fmt.Sprintf("repo-%d", i)))
				require.NoError(b, err)
				appender := NewPackAppender(repo, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, ".benchpack")
				_, err = CaptureAttachments(context.Background(), "", []ContentRef{ref}, map[string]bool{}, appender,
					CaptureOptions{Jobs: 1, Source: benchmarkContentSource{content: content}})
				require.NoError(b, err)
				_, _, err = appender.Finish()
				require.NoError(b, err)
			}
		})
	}
}

func BenchmarkRepoStreamingReads(b *testing.B) {
	contents := map[string][]byte{
		"raw-1MiB":        backupBenchmarkNoise(1 << 20),
		"compressed-1MiB": bytes.Repeat([]byte("backup stream benchmark "), (1<<20)/24),
	}
	for name, content := range contents {
		b.Run(name, func(b *testing.B) {
			repo, err := Init(filepath.Join(b.TempDir(), "repo"))
			require.NoError(b, err)
			known := map[pack.BlobID]IndexEntry{}
			appender := NewPackAppender(repo, known, pack.DefaultZstdLevel, nil, testPackExt)
			id, _, err := appender.Add(content)
			require.NoError(b, err)
			_, _, err = appender.Finish()
			require.NoError(b, err)
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			b.ResetTimer()
			for range b.N {
				reader, err := repo.OpenBlob(context.Background(), known, id, nil, testPackExt)
				require.NoError(b, err)
				_, copyErr := io.Copy(io.Discard, reader)
				require.NoError(b, errors.Join(copyErr, reader.Close()))
			}
		})
	}
}

func backupBenchmarkNoise(size int) []byte {
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
