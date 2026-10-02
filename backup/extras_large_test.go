package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

// KIT_STREAM_TEST_BYTES can exercise the same path above the pack frame limit.
func TestLargeExtrasCaptureVerifyPruneRestore(t *testing.T) {
	r := initTestRepo(t)
	dbPath, contentDir, dataDir, _ := seedBackupFixture(t)
	size := largeBackupStreamTestBytes(t, 64<<20+1)
	extraDir := t.TempDir()
	extra := writeLargeAttachment(t, extraDir, size, true)
	opts := createOpts(dbPath, contentDir, dataDir, t.TempDir())
	opts.Extras = ExtrasSpec{Files: []ExtrasFileSpec{{
		Path: filepath.Join(extraDir, extra.StoragePath), RecordAs: "recovery/history.db",
	}}}
	first, err := Create(t.Context(), r, newTestApp(), opts)
	require.NoError(t, err)
	require.Equal(t, 6, first.MinReaderVersion, "older readers cannot restore chunked extras")
	second, err := Create(t.Context(), r, newTestApp(), opts)
	require.NoError(t, err)
	require.Less(t, second.BytesAdded, size, "unchanged extras must reuse stored chunks")
	_, err = Forget(t.Context(), r, ForgetOptions{SnapshotIDs: []string{second.SnapshotID}})
	require.NoError(t, err)
	// Force live chunks through prune's repacking path, not just a no-op walk.
	consolidatePruneFixture(t, r, 1<<20)
	pruned, err := Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, pruned.PacksToRepack)
	verified, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{SnapshotID: first.SnapshotID})
	require.NoError(t, err)
	require.Empty(t, verified.Problems)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = Restore(t.Context(), r, newTestApp(), RestoreOptions{SnapshotID: first.SnapshotID, TargetDir: target})
	require.NoError(t, err)
	restored := filepath.Join(target, "recovery", "history.db")
	gotSize, gotHash := hashFileStream(t, restored)
	require.Equal(t, size, gotSize)
	require.Equal(t, extra.Hash, gotHash)
	info, err := os.Stat(restored)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestChunkedExtrasRejectCorruptObjects(t *testing.T) {
	for _, fault := range []string{"hash", "size", "missing chunk"} {
		t.Run(fault, func(t *testing.T) {
			r, m := buildVerifyFixture(t)
			known, err := r.LoadBlobIndex()
			require.NoError(t, err)
			a := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
			t.Cleanup(a.Abort)
			first, _, err := a.Add([]byte("first"))
			require.NoError(t, err)
			second, _, err := a.Add([]byte("second"))
			require.NoError(t, err)
			whole := pack.ComputeBlobID([]byte("firstsecond"))
			recipe := objectRecipe{
				Version: 1, Blob: whole.String(), Bytes: 11,
				Chunks: objectChunks{{Blob: first.String(), Bytes: 5}, {Blob: second.String(), Bytes: 6}},
			}
			size := int64(11)
			switch fault {
			case "hash":
				recipe.Chunks[0], recipe.Chunks[1] = recipe.Chunks[1], recipe.Chunks[0]
			case "size":
				size++
			case "missing chunk":
				recipe.Chunks[1].Blob = pack.ComputeBlobID([]byte("absent")).String()
			}
			raw, err := json.Marshal(recipe)
			require.NoError(t, err)
			recipeID, _, err := a.Add(raw)
			require.NoError(t, err)
			raw, err = json.Marshal(ExtrasTree{Entries: []ExtrasEntry{{
				Path: "recovery/history.db", Blob: whole.String(), Size: size, Mode: 0o600,
			}}})
			require.NoError(t, err)
			treeID, _, err := a.Add(raw)
			require.NoError(t, err)
			_, entries, err := a.Finish()
			require.NoError(t, err)
			_, err = r.WriteIndex(entries)
			require.NoError(t, err)
			m.FormatVersion, m.MinReaderVersion = 6, 6
			m.Extras = ManifestExtras{Tree: treeID.String(), Recipes: []string{recipeID.String()}}
			id, err := r.WriteManifest(m)
			require.NoError(t, err)
			verified, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{SnapshotID: id})
			require.NoError(t, err)
			require.NotEmpty(t, verified.Problems)
			target := filepath.Join(t.TempDir(), "restore")
			_, err = Restore(t.Context(), r, newTestApp(), RestoreOptions{SnapshotID: id, TargetDir: target})
			require.Error(t, err)
			require.NoFileExists(t, filepath.Join(target, "app.db"))
			require.NoFileExists(t, filepath.Join(target, "recovery", "history.db"))
			staged, err := filepath.Glob(filepath.Join(target, "recovery", ".restore-*"))
			require.NoError(t, err)
			require.Empty(t, staged)
		})
	}
}
