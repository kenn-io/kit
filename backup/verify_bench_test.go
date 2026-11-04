package backup

import (
	"crypto/rand"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

func BenchmarkQuickVerifyAll(b *testing.B) {
	for _, count := range []int{25, 100} {
		b.Run(fmt.Sprintf("snapshots=%d", count), func(b *testing.B) {
			r, err := Init(filepath.Join(b.TempDir(), "repo"))
			require.NoError(b, err)
			dbPath, attachmentsDir, dataDir, db := seedBackupFixture(b)
			opts := createOpts(dbPath, attachmentsDir, dataDir, b.TempDir())
			for i := range count {
				ref := writeLooseAttachment(b, attachmentsDir, []byte(fmt.Sprintf("attachment %d", i)))
				_, err := db.ExecContext(b.Context(),
					`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path) VALUES (?, ?, ?, '', '')`,
					ref.Hash, ref.Hash[:2]+"/"+ref.Hash, ref.Size)
				require.NoError(b, err)
				_, err = Create(b.Context(), r, newTestApp(), opts)
				require.NoError(b, err)
			}
			b.ReportAllocs()
			for b.Loop() {
				result, err := Verify(b.Context(), r, newTestApp(), VerifyOptions{All: true, Quick: true})
				require.NoError(b, err)
				require.Empty(b, result.Problems)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(count), "ns/snapshot")
		})
	}
}

func BenchmarkQuickVerifyBlobLookup(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("entries=%d", count), func(b *testing.B) {
			r, err := Init(filepath.Join(b.TempDir(), "repo"))
			require.NoError(b, err)
			known := map[pack.BlobID]IndexEntry{}
			appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
			ids := make([]pack.BlobID, count)
			for i := range ids {
				body := make([]byte, 512)
				_, err := rand.Read(body)
				require.NoError(b, err)
				ids[i], _, err = appender.Add(body)
				require.NoError(b, err)
			}
			packs, _, err := appender.Finish()
			require.NoError(b, err)
			require.Len(b, packs, 1)
			st := &verifyState{
				ctx: b.Context(), repo: r, app: newTestApp(), known: known, quick: true,
				readers: map[string]*cachedPack{}, readerErrs: map[string]error{},
				checked: map[pack.BlobID]bool{}, result: &VerifyResult{},
			}
			b.Cleanup(st.closeReaders)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				st.checked = make(map[pack.BlobID]bool, count)
				b.StartTimer()
				for _, id := range ids {
					_, ok := st.blob(id, "snapshot", false)
					require.Truef(b, ok, "blob %s: %v", id, st.result.Problems)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(count), "ns/blob")
		})
	}
}
