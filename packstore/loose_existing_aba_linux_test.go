//go:build linux

package packstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLooseWriteFullHashRejectsEqualSizeReplacementAfterVerification(t *testing.T) {
	content := bytes.Repeat([]byte("verified existing content\n"), 256)
	for _, encoding := range []struct {
		name        string
		compression LooseCompressionOptions
		want        LooseEncoding
	}{
		{name: "raw", want: LooseEncodingRaw},
		{
			name:        "compressed",
			compression: LooseCompressionOptions{Enabled: true},
			want:        LooseEncodingZstd,
		},
	} {
		for _, api := range []struct {
			name  string
			write func(context.Context, *LooseStore, []byte, WriteOptions) (WriteResult, error)
		}{
			{
				name: "Write",
				write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) (WriteResult, error) {
					return store.Write(ctx, bytes.NewReader(content), opts)
				},
			},
			{
				name: "WriteBytes",
				write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) (WriteResult, error) {
					return store.WriteBytes(ctx, content, opts)
				},
			},
		} {
			t.Run(encoding.name+"/"+api.name, func(t *testing.T) {
				store := newLooseStoreForTest(t, StagingSameDirectory)
				opts := WriteOptions{
					Durability:   AtomicPublication,
					Dedup:        VerifyFullHash,
					ExpectedHash: hashForTest(content),
					ExpectedSize: int64(len(content)),
					SizeKnown:    true,
					Compression:  encoding.compression,
				}
				created, err := store.WriteBytes(context.Background(), content, opts)
				require.NoError(t, err)
				require.Equal(t, encoding.want, created.Encoding)
				physical, err := os.ReadFile(created.Path)
				require.NoError(t, err)
				replacement := bytes.Repeat([]byte{0xa5}, len(physical))

				replacementInstalled := installEqualSizeReplacementAtFinalSnapshot(t, created.Path, replacement)

				result, err := api.write(context.Background(), store, content, opts)

				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrContentMismatch) || errors.Is(err, errIdentityChanged), err)
				assert.False(t, result.Created)
				assert.True(t, *replacementInstalled)
				assert.Equal(t, replacement, mustReadFile(t, created.Path))
			})
		}
	}
}

func TestLooseDurableTypeAndSizeRejectsEqualSizeReplacementAfterSync(t *testing.T) {
	content := bytes.Repeat([]byte("durably synced existing content\n"), 256)
	for _, encoding := range []struct {
		name        string
		compression LooseCompressionOptions
		want        LooseEncoding
	}{
		{name: "raw", want: LooseEncodingRaw},
		{
			name:        "compressed",
			compression: LooseCompressionOptions{Enabled: true},
			want:        LooseEncodingZstd,
		},
	} {
		t.Run(encoding.name, func(t *testing.T) {
			store := newLooseStoreForTest(t, StagingSameDirectory)
			createOpts := WriteOptions{
				Durability:   AtomicPublication,
				Dedup:        VerifyFullHash,
				ExpectedHash: hashForTest(content),
				ExpectedSize: int64(len(content)),
				SizeKnown:    true,
				Compression:  encoding.compression,
			}
			created, err := store.WriteBytes(context.Background(), content, createOpts)
			require.NoError(t, err)
			require.Equal(t, encoding.want, created.Encoding)
			physical, err := os.ReadFile(created.Path)
			require.NoError(t, err)
			replacement := bytes.Repeat([]byte{0x5a}, len(physical))
			replacementInstalled := installEqualSizeReplacementAtFinalSnapshot(t, created.Path, replacement)

			result, err := store.WriteBytes(context.Background(), content, WriteOptions{
				Durability:   DurablePublication,
				Dedup:        VerifyTypeAndSize,
				ExpectedHash: createOpts.ExpectedHash,
				ExpectedSize: createOpts.ExpectedSize,
				SizeKnown:    true,
				Compression:  encoding.compression,
			})

			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrContentMismatch) || errors.Is(err, errIdentityChanged), err)
			assert.False(t, result.Created)
			assert.True(t, *replacementInstalled)
			assert.Equal(t, replacement, mustReadFile(t, created.Path))
		})
	}
}

func installEqualSizeReplacementAtFinalSnapshot(t *testing.T, path string, replacement []byte) *bool {
	t.Helper()
	originalSnapshot := snapshotLoosePathIdentity
	var snapshots int
	var installed bool
	snapshotLoosePathIdentity = func(gotPath string) (fs.FileInfo, error) {
		info, snapshotErr := originalSnapshot(gotPath)
		if snapshotErr != nil || filepath.Clean(gotPath) != filepath.Clean(path) {
			return info, snapshotErr
		}
		snapshots++
		if snapshots != 2 {
			return info, nil
		}
		installed = true
		require.NoError(t, os.Remove(gotPath))
		var replacementInfo fs.FileInfo
		for attempt := 0; attempt < 256; attempt++ {
			require.NoError(t, os.WriteFile(gotPath, replacement, 0o600))
			replacementInfo, snapshotErr = originalSnapshot(gotPath)
			require.NoError(t, snapshotErr)
			if os.SameFile(info, replacementInfo) {
				break
			}
			require.NoError(t, os.Remove(gotPath))
		}
		if _, statErr := os.Stat(gotPath); errors.Is(statErr, fs.ErrNotExist) {
			require.NoError(t, os.WriteFile(gotPath, replacement, 0o600))
			replacementInfo, snapshotErr = originalSnapshot(gotPath)
		}
		return replacementInfo, snapshotErr
	}
	t.Cleanup(func() { snapshotLoosePathIdentity = originalSnapshot })
	return &installed
}
