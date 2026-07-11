package packstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestMaintenanceReadAllowsMinimumZstdWindowForSmallBlob(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("small bounded frame "), 16)
	encoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(zstd.MinWindowSize),
		zstd.WithSingleSegment(false))
	require.NoError(err)
	encoded := encoder.EncodeAll(content, nil)
	encoder.Close()

	layout := layoutForStoreTest(t)
	writer, err := pack.NewWriter(t.TempDir(), pack.WriterOptions{})
	require.NoError(err)
	id := pack.ComputeBlobID(content)
	_, err = writer.AppendEncoded(id, encoded, uint64(len(content)), true)
	require.NoError(err)
	path := layout.PackPath(writer.ID())
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	_, err = writer.Seal(path)
	require.NoError(err)

	reader, err := OpenMaintenancePack(path, DefaultLimits())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	hash, err := ParseHash(id.String())
	require.NoError(err)
	got, err := reader.ReadBlob(hash)
	require.NoError(err)
	require.Equal(content, got)
}

func TestReadBoundedEnforcesLooseAndPackedBlobLimits(t *testing.T) {
	for _, storage := range []string{"loose", "packed"} {
		t.Run(storage, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			layout := layoutForStoreTest(t)
			content := []byte("bounded content")
			hash := hashForTest(content)
			location := Location{Member: true}
			if storage == "packed" {
				entry := buildStoreTestPack(t, layout, content)
				location.Pack = &entry
			} else {
				require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
				require.NoError(os.WriteFile(layout.LoosePath(hash), content, 0o600))
			}
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{hash: location}}, layout)
			got, size, err := store.ReadBounded(context.Background(), hash, int64(len(content)))
			require.NoError(err)
			assert.Equal(content, got)
			assert.Equal(int64(len(content)), size)
			_, _, err = store.ReadBounded(context.Background(), hash, int64(len(content)-1))
			assert.ErrorIs(err, ErrBlobTooLarge)
		})
	}
}

func TestReadBoundedEnforcesConfiguredBlobCeiling(t *testing.T) {
	for _, storage := range []string{"loose", "packed"} {
		t.Run(storage, func(t *testing.T) {
			require := require.New(t)
			layout := layoutForStoreTest(t)
			content := []byte("ninebytes")
			hash := hashForTest(content)
			location := Location{Member: true}
			if storage == "packed" {
				entry := buildStoreTestPack(t, layout, content)
				location.Pack = &entry
			} else {
				require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
				require.NoError(os.WriteFile(layout.LoosePath(hash), content, 0o600))
			}
			limits := DefaultLimits()
			limits.BlobBytes = 8
			store, err := NewStore(&mapResolver{locations: map[Hash]Location{hash: location}}, layout,
				StoreOptions{Limits: limits})
			require.NoError(err)
			t.Cleanup(func() { require.NoError(store.Close()) })
			_, _, err = store.ReadBounded(context.Background(), hash, int64(len(content)))
			require.ErrorIs(err, ErrBlobTooLarge)
		})
	}
}

func TestPreflightRejectsContainerFooterAndDuplicateIDs(t *testing.T) {
	limits := DefaultLimits()
	t.Run("container", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		entry := buildStoreTestPack(t, layout, []byte("container"))
		require.NoError(t, os.Truncate(layout.PackPath(entry.PackID), limits.PackBytes+1))
		_, err := OpenMaintenancePack(layout.PackPath(entry.PackID), limits)
		assert.ErrorIs(t, err, ErrBlobTooLarge)
	})
	t.Run("footer", func(t *testing.T) {
		require := require.New(t)
		layout := layoutForStoreTest(t)
		entry := buildStoreTestPack(t, layout, []byte("footer"))
		path := layout.PackPath(entry.PackID)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(err)
		info, err := f.Stat()
		require.NoError(err)
		var encoded [4]byte
		binary.LittleEndian.PutUint32(encoded[:], uint32(limits.FooterBytes+1))
		_, err = f.WriteAt(encoded[:], info.Size()-plainPackTrailerSize)
		require.NoError(err)
		require.NoError(f.Close())
		_, err = OpenMaintenancePack(path, limits)
		assert.ErrorIs(t, err, ErrBlobTooLarge)
	})
	t.Run("duplicate id", func(t *testing.T) {
		require := require.New(t)
		layout := layoutForStoreTest(t)
		writer, err := pack.NewWriter(t.TempDir(), pack.WriterOptions{})
		require.NoError(err)
		_, err = writer.Append([]byte("duplicate"))
		require.NoError(err)
		_, err = writer.Append([]byte("duplicate"))
		require.NoError(err)
		path := layout.PackPath(writer.ID())
		require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
		_, err = writer.Seal(path)
		require.NoError(err)
		_, err = OpenMaintenancePack(path, limits)
		assert.ErrorIs(t, err, pack.ErrCorrupt)
	})
}

func TestPreflightRejectsSymlinkToValidPack(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	entry := buildStoreTestPack(t, layout, []byte("symlink target"))
	link := filepath.Join(t.TempDir(), "pack-link")
	if err := os.Symlink(layout.PackPath(entry.PackID), link); err != nil {
		t.Skip("symlinks unavailable: " + err.Error())
	}

	reader, err := OpenMaintenancePack(link, DefaultLimits())
	if reader != nil {
		require.NoError(reader.Close())
	}
	require.Error(err)
}

func TestMaintenanceAllocationsRespectPlatformInt(t *testing.T) {
	originalMax := maxPlatformInt
	maxPlatformInt = 8
	t.Cleanup(func() { maxPlatformInt = originalMax })

	t.Run("verified loose content", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		content := []byte("ninebytes")
		hash := writeMaintenanceLoose(t, layout, content)

		_, err := readVerifiedLoosePath(layout.LoosePath(hash), hash, int64(len(content)))
		require.ErrorIs(t, err, ErrBlobTooLarge)
		var limitErr *LimitError
		require.ErrorAs(t, err, &limitErr)
		assert.Equal(t, LimitBlobRawBytes, limitErr.Dimension)
		assert.Equal(t, uint64(8), limitErr.Limit)
	})

	t.Run("pack footer", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		entry := buildStoreTestPack(t, layout, []byte("footer allocation"))

		reader, err := OpenMaintenancePack(layout.PackPath(entry.PackID), DefaultLimits())
		if reader != nil {
			require.NoError(t, reader.Close())
		}
		require.ErrorIs(t, err, ErrBlobTooLarge)
		var limitErr *LimitError
		require.ErrorAs(t, err, &limitErr)
		assert.Equal(t, LimitPackFooterBytes, limitErr.Dimension)
		assert.Equal(t, uint64(8), limitErr.Limit)
	})
}

func TestLimitErrorIsTyped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	err := newLimitError(LimitBlobRawBytes, 11, 10)
	require.ErrorIs(err, ErrBlobTooLarge)
	var limitErr *LimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(LimitBlobRawBytes, limitErr.Dimension)
	assert.Equal(uint64(11), limitErr.Actual)
	assert.Equal(uint64(10), limitErr.Limit)
}
