package packstore

import (
	"encoding/hex"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkRepackMergeSmallPacks measures one merge pass over many old
// one-blob packs of compressible text, the shape frequent small writes leave.
func BenchmarkRepackMergeSmallPacks(b *testing.B) {
	const (
		sources = 490
		rawSize = 200 << 10
	)
	require := require.New(b)
	words := strings.Fields("archive backup blob catalog commit content daemon digest entry frame hash index " +
		"journal layout loose manifest member merge object offset pack payload record repack retire " +
		"segment snapshot source stored stream target verify window writer")
	rng := rand.New(rand.NewPCG(1, 2))
	template := b.TempDir()
	templateLayout, err := NewLayout(template, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(err)
	created := time.Now().Add(-48 * time.Hour)
	entries := make(map[Hash]IndexEntry, sources)
	packs := make(map[string]PackRecord, sources)
	for i := range sources {
		var content strings.Builder
		token := make([]byte, 8)
		for content.Len() < rawSize {
			if rng.IntN(4) == 0 {
				for j := range token {
					token[j] = byte(rng.Uint32())
				}
				content.WriteString(hex.EncodeToString(token))
			} else {
				content.WriteString(words[rng.IntN(len(words))])
			}
			content.WriteByte(' ')
		}
		entry := buildStoreBenchmarkPack(b, templateLayout, [][]byte{[]byte(content.String()[:rawSize])})
		entries[entry.Hash] = entry
		packs[entry.PackID] = PackRecord{
			PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen,
			CreatedAt: created.Add(time.Duration(i) * time.Second),
		}
	}
	opts := RepackOptions{
		TargetSize: 32 << 20, MaxBytes: 1 << 30, Now: time.Now(),
		Selection: RepackSelection{MergeBelowStored: 8 << 20},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		layout, err := NewLayout(b.TempDir(), LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
		require.NoError(err)
		catalog := newMaintenanceCatalog()
		catalog.entries = maps.Clone(entries)
		catalog.packs = maps.Clone(packs)
		for hash, entry := range entries {
			catalog.members[hash] = Reference{Hash: hash}
			path := layout.PackPath(entry.PackID)
			require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(os.Link(templateLayout.PackPath(entry.PackID), path))
		}
		maintainer, err := NewMaintainer(catalog, layout, MaintainerOptions{Limits: DefaultLimits()})
		require.NoError(err)
		b.StartTimer()

		stats, err := maintainer.Repack(b.Context(), opts)
		require.NoError(err)
		require.Equal(sources, stats.PacksMerged)

		b.StopTimer()
		require.NoError(maintainer.Close())
		b.StartTimer()
	}
}
