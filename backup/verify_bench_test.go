package backup

import (
	"crypto/rand"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

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
				for _, id := range ids {
					_, ok := st.blob(id, "snapshot", false)
					require.Truef(b, ok, "blob %s: %v", id, st.result.Problems)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(count), "ns/blob")
		})
	}
}
