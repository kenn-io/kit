package packstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestStoreOpenStreamLoosePackedParity(t *testing.T) {
	content := bytes.Repeat([]byte("stream parity "), 4096)
	for _, representation := range []string{"loose", "packed"} {
		t.Run(representation, func(t *testing.T) {
			store, hash := streamStoreForTest(t, representation, content)
			stream, size, err := store.OpenStream(context.Background(), hash)
			require.NoError(t, err)
			assert.Equal(t, int64(len(content)), size)
			prefix := make([]byte, 17)
			_, err = io.ReadFull(stream, prefix)
			require.NoError(t, err)
			assert.False(t, stream.Verified())
			rest, err := io.ReadAll(stream)
			require.NoError(t, err)
			assert.Equal(t, content, append(prefix, rest...))
			assert.True(t, stream.Verified())
			require.NoError(t, stream.Verify())
			require.NoError(t, stream.Close())
			require.NoError(t, stream.Close())
		})
	}
}

func TestStoreStreamsLooseObjectAboveMaintenanceLimit(t *testing.T) {
	content := bytes.Repeat([]byte("oversized loose content "), 16)
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
	require.NoError(t, os.WriteFile(layout.LoosePath(hash), content, 0o600))
	limits := DefaultLimits()
	limits.BlobBytes = int64(len(content) - 1)
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		hash: {Member: true},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	stream, size, err := store.OpenStream(context.Background(), hash)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), size)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.Equal(t, content, got)
	require.NoError(t, stream.Close())

	var copied bytes.Buffer
	written, err := store.CopyVerified(context.Background(), hash, &copied)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), written)
	assert.Equal(t, content, copied.Bytes())

	_, _, err = store.ReadBounded(context.Background(), hash, limits.BlobBytes)
	var limitErr *LimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, LimitBlobRawBytes, limitErr.Dimension)
}

func TestStoreOpenStreamEarlyCloseLoosePackedParity(t *testing.T) {
	content := []byte("early close content")
	for _, representation := range []string{"loose", "packed"} {
		t.Run(representation, func(t *testing.T) {
			store, hash := streamStoreForTest(t, representation, content)
			stream, _, err := store.OpenStream(context.Background(), hash)
			require.NoError(t, err)
			buf := make([]byte, 2)
			_, err = stream.Read(buf)
			require.NoError(t, err)
			require.ErrorIs(t, stream.Close(), pack.ErrVerificationIncomplete)
			require.ErrorIs(t, stream.Close(), pack.ErrVerificationIncomplete)
			assert.False(t, stream.Verified())
			assertPackedLeases(t, store, 0)
		})
	}
}

func TestStoreRejectsUnknownPackFlags(t *testing.T) {
	tests := []struct {
		name string
		read func(*Store, Hash) error
	}{
		{
			name: "bounded",
			read: func(store *Store, hash Hash) error {
				_, _, err := store.ReadBounded(context.Background(), hash, 1<<20)
				return err
			},
		},
		{
			name: "streaming",
			read: func(store *Store, hash Hash) error {
				stream, _, err := store.OpenStream(context.Background(), hash)
				if err != nil {
					return err
				}
				return errors.Join(stream.Verify(), stream.Close())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout := layoutForStoreTest(t)
			entry := buildStoreTestPack(t, layout, []byte("unknown pack flags"))
			f, err := os.OpenFile(layout.PackPath(entry.PackID), os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte{0x80}, 5)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
				entry.Hash: {Member: true, Pack: &entry},
			}}, layout)

			err = tt.read(store, entry.Hash)
			require.ErrorIs(t, err, pack.ErrCorrupt)
			require.ErrorContains(t, err, "unknown pack flags 0x80")
		})
	}
}

func TestStoreRejectsUnknownFlagsOnUnselectedEntry(t *testing.T) {
	tests := []struct {
		name string
		read func(*Store, Hash) error
	}{
		{
			name: "bounded",
			read: func(store *Store, hash Hash) error {
				_, _, err := store.ReadBounded(context.Background(), hash, 1<<20)
				return err
			},
		},
		{
			name: "streaming",
			read: func(store *Store, hash Hash) error {
				stream, _, err := store.OpenStream(context.Background(), hash)
				if err != nil {
					return err
				}
				return errors.Join(stream.Verify(), stream.Close())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout := layoutForStoreTest(t)
			staging := t.TempDir()
			writer, err := pack.NewWriter(staging, pack.WriterOptions{})
			require.NoError(t, err)
			selected, err := writer.Append([]byte("selected entry"))
			require.NoError(t, err)
			_, err = writer.Append([]byte("unselected entry"))
			require.NoError(t, err)
			packID := writer.ID()
			path := layout.PackPath(packID)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			_, err = writer.Seal(path)
			require.NoError(t, err)
			mutateImportFooterEntry(t, path, 1, func(entry []byte) { entry[56] |= 0x80 })

			hash, err := ParseHash(selected.ID.String())
			require.NoError(t, err)
			indexed := IndexEntry{
				Hash: hash, PackID: packID, Offset: int64(selected.Offset),
				StoredLen: int64(selected.StoredLen), RawLen: int64(selected.RawLen),
				Flags: uint8(selected.Flags), CRC32C: selected.CRC32C,
			}
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
				hash: {Member: true, Pack: &indexed},
			}}, layout)

			err = tt.read(store, hash)
			require.ErrorIs(t, err, pack.ErrCorrupt)
			require.ErrorContains(t, err, "entry 1 has unknown flags 0x80")
		})
	}
}

func TestStoreCopyVerifiedLoosePackedParity(t *testing.T) {
	content := bytes.Repeat([]byte("verified copy "), 2048)
	for _, representation := range []string{"loose", "packed"} {
		t.Run(representation, func(t *testing.T) {
			store, hash := streamStoreForTest(t, representation, content)
			var dst bytes.Buffer
			written, err := store.CopyVerified(context.Background(), hash, &dst)
			require.NoError(t, err)
			assert.Equal(t, int64(len(content)), written)
			assert.Equal(t, content, dst.Bytes())
			assertPackedLeases(t, store, 0)
		})
	}
}

func TestStoreCopyVerifiedDestinationFailureReleasesSource(t *testing.T) {
	content := bytes.Repeat([]byte("destination failure "), 2048)
	store, hash := streamStoreForTest(t, "packed", content)
	destinationErr := errors.New("destination failed")
	dst := &failAfterWriter{remaining: 32, err: destinationErr}
	written, err := store.CopyVerified(context.Background(), hash, dst)
	require.ErrorIs(t, err, destinationErr)
	assert.Equal(t, int64(32), written)
	assertPackedLeases(t, store, 0)
}

type failAfterWriter struct {
	remaining int
	err       error
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, w.err
	}
	n := min(len(p), w.remaining)
	w.remaining -= n
	if n < len(p) {
		return n, w.err
	}
	return n, nil
}

func TestStoreOpenStreamTerminalIntegrityErrors(t *testing.T) {
	content := []byte("terminal integrity content")
	tests := []struct {
		name           string
		representation string
		want           error
	}{
		{name: "loose", representation: "loose", want: ErrContentMismatch},
		{name: "packed", representation: "packed", want: pack.ErrCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout := layoutForStoreTest(t)
			hash := hashForTest(content)
			var entry IndexEntry
			if tt.representation == "loose" {
				require.NoError(t, os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
				corrupt := append([]byte(nil), content...)
				corrupt[0] ^= 0xff
				require.NoError(t, os.WriteFile(layout.LoosePath(hash), corrupt, 0o600))
			} else {
				entry = buildStoreTestPack(t, layout, content)
				hash = entry.Hash
				f, err := os.OpenFile(layout.PackPath(entry.PackID), os.O_RDWR, 0)
				require.NoError(t, err)
				_, err = f.WriteAt([]byte{'X'}, entry.Offset)
				require.NoError(t, err)
				require.NoError(t, f.Close())
			}
			location := Location{Member: true}
			if tt.representation == "packed" {
				location.Pack = &entry
			}
			store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{hash: location}}, layout)
			stream, _, err := store.OpenStream(context.Background(), hash)
			require.NoError(t, err)
			got, err := io.ReadAll(stream)
			require.ErrorIs(t, err, tt.want)
			assert.Len(t, got, len(content))
			assert.False(t, stream.Verified())
			require.ErrorIs(t, stream.Verify(), tt.want)
			require.ErrorIs(t, stream.Close(), tt.want)
			assertPackedLeases(t, store, 0)
		})
	}
}

func TestStoreOpenStreamCancellationReleasesPackedLease(t *testing.T) {
	content := bytes.Repeat([]byte("cancel packed stream "), 4096)
	store, hash := streamStoreForTest(t, "packed", content)
	ctx, cancel := context.WithCancel(context.Background())
	stream, _, err := store.OpenStream(ctx, hash)
	require.NoError(t, err)
	buf := make([]byte, 32)
	_, err = stream.Read(buf)
	require.NoError(t, err)
	cancel()
	_, err = stream.Read(buf)
	require.ErrorIs(t, err, context.Canceled)
	assertPackedLeases(t, store, 0)
	require.ErrorIs(t, stream.Close(), context.Canceled)
}

func TestStoreOpenStreamRetriesAuthorityMoves(t *testing.T) {
	content := []byte("stream migration race")
	hash := hashForTest(content)

	t.Run("loose to pack", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		entry := buildStoreTestPack(t, layout, content)
		loosePath := layout.LoosePath(hash)
		require.NoError(t, os.MkdirAll(filepath.Dir(loosePath), 0o700))
		require.NoError(t, os.WriteFile(loosePath, content, 0o600))
		resolver := &sequenceResolver{locations: []Location{{Member: true}, {Member: true, Pack: &entry}}}
		resolver.beforeFirstReturn = func() { require.NoError(t, os.Remove(loosePath)) }
		store := newStoreForTest(t, resolver, layout)
		assertStreamContent(t, store, hash, content)
		assert.Equal(t, 2, resolver.calls)
	})

	t.Run("pack to loose", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		entry := buildStoreTestPack(t, layout, content)
		loosePath := layout.LoosePath(hash)
		require.NoError(t, os.MkdirAll(filepath.Dir(loosePath), 0o700))
		require.NoError(t, os.WriteFile(loosePath, content, 0o600))
		require.NoError(t, os.Remove(layout.PackPath(entry.PackID)))
		resolver := &sequenceResolver{locations: []Location{{Member: true, Pack: &entry}, {Member: true}}}
		store := newStoreForTest(t, resolver, layout)
		assertStreamContent(t, store, hash, content)
		assert.Equal(t, 2, resolver.calls)
	})

	t.Run("pack to pack", func(t *testing.T) {
		layout := layoutForStoreTest(t)
		first := buildStoreTestPack(t, layout, content)
		second := buildStoreTestPack(t, layout, content)
		require.NotEqual(t, first.PackID, second.PackID)
		resolver := &sequenceResolver{locations: []Location{{Member: true, Pack: &first}, {Member: true, Pack: &second}}}
		resolver.beforeFirstReturn = func() { require.NoError(t, os.Remove(layout.PackPath(first.PackID))) }
		store := newStoreForTest(t, resolver, layout)
		assertStreamContent(t, store, hash, content)
		assert.Equal(t, 2, resolver.calls)
	})
}

func TestStoreConcurrentPackedStreamsShareLeasedReader(t *testing.T) {
	content := bytes.Repeat([]byte("shared packed stream "), 1<<14)
	store, hash := streamStoreForTest(t, "packed", content)
	const streams = 16
	readers := make([]VerifiedReadCloser, streams)
	for i := range readers {
		reader, _, err := store.OpenStream(context.Background(), hash)
		require.NoError(t, err)
		readers[i] = reader
	}
	store.mu.Lock()
	require.Len(t, store.packReaders, 1)
	for _, slot := range store.packReaders {
		assert.Equal(t, streams, slot.leases)
	}
	store.mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for _, reader := range readers {
		wg.Go(func() { errs <- reader.Verify() })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assertPackedLeases(t, store, 0)
}

func TestStoreEvictionAndClosePreserveActiveStreams(t *testing.T) {
	layout := layoutForStoreTest(t)
	firstContent := bytes.Repeat([]byte("first stream "), 4096)
	secondContent := bytes.Repeat([]byte("second stream "), 4096)
	first := buildStoreTestPack(t, layout, firstContent)
	second := buildStoreTestPack(t, layout, secondContent)
	resolver := &mapResolver{locations: map[Hash]Location{
		first.Hash: {Member: true, Pack: &first}, second.Hash: {Member: true, Pack: &second},
	}}
	store, err := NewStore(resolver, layout, StoreOptions{Limits: DefaultLimits(), ReaderSlots: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	firstStream, _, err := store.OpenStream(context.Background(), first.Hash)
	require.NoError(t, err)
	secondStream, _, err := store.OpenStream(context.Background(), second.Hash)
	require.NoError(t, err)
	require.Len(t, store.packReaders, 1)
	require.NoError(t, secondStream.Verify())
	require.NoError(t, store.Close())
	assert.Empty(t, store.packReaders)
	require.NoError(t, firstStream.Verify())
	require.NoError(t, firstStream.Close())
}

func TestStoreRetirePackKeepsActiveStreamReadable(t *testing.T) {
	content := bytes.Repeat([]byte("retired active stream "), 4096)
	store, hash := streamStoreForTest(t, "packed", content)
	location := store.resolver.(*mapResolver).locations[hash]
	require.NotNil(t, location.Pack)
	stream, _, err := store.OpenStream(context.Background(), hash)
	require.NoError(t, err)
	require.NoError(t, store.RetirePack(location.Pack.PackID))
	_, err = os.Stat(store.layout.PackPath(location.Pack.PackID))
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
}

func TestStoreRetirePackErrorsAreTyped(t *testing.T) {
	content := []byte("typed retirement")
	store, hash := streamStoreForTest(t, "packed", content)
	location := store.resolver.(*mapResolver).locations[hash]
	path := store.layout.PackPath(location.Pack.PackID)
	stream, _, err := store.OpenStream(context.Background(), hash)
	require.NoError(t, err)
	orphan := path + ".open"
	require.NoError(t, os.Rename(path, orphan))
	require.NoError(t, os.Mkdir(path, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o600))
	err = store.RetirePack(location.Pack.PackID)
	require.ErrorIs(t, err, ErrPackRetirementDeferred)
	var retireErr *PackRetirementError
	require.ErrorAs(t, err, &retireErr)
	assert.Equal(t, location.Pack.PackID, retireErr.PackID)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
}

func TestStoreOpenStreamPreservesBufferedContractAndAppliesPolicy(t *testing.T) {
	content := []byte("container policy")
	layout := layoutForStoreTest(t)
	entry := buildStoreTestPack(t, layout, content)
	limits := DefaultLimits()
	limits.PackBytes = 1
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	buffered, _, err := store.Open(context.Background(), entry.Hash)
	require.NoError(t, err)
	require.NoError(t, buffered.Close())
	_, _, err = store.OpenStream(context.Background(), entry.Hash)
	var limitErr *LimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, LimitPackContainerBytes, limitErr.Dimension)

	strictStore, err := NewStore(&mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, strictStore.Close()) })
	_, _, err = strictStore.OpenStream(context.Background(), entry.Hash)
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, LimitPackContainerBytes, limitErr.Dimension)
}

func TestStoreOpenStreamRejectsNonMemberBeforePhysicalRead(t *testing.T) {
	content := []byte("physical but unauthorized")
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
	require.NoError(t, os.WriteFile(layout.LoosePath(hash), content, 0o600))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{hash: {}}}, layout)
	_, _, err := store.OpenStream(context.Background(), hash)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestStoreOpenStreamMapsZstdWindowPolicy(t *testing.T) {
	content := bytes.Repeat([]byte("window policy "), 1<<16)
	var frame bytes.Buffer
	encoder, err := zstd.NewWriter(&frame, zstd.WithWindowSize(8<<20), zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	_, err = encoder.Write(content)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())

	layout := layoutForStoreTest(t)
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err)
	id := pack.ComputeBlobID(content)
	entry, err := w.AppendEncoded(id, frame.Bytes(), uint64(len(content)), true)
	require.NoError(t, err)
	packID := w.ID()
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.PackPath(packID)), 0o700))
	_, err = w.Seal(layout.PackPath(packID))
	require.NoError(t, err)
	hash, err := ParseHash(id.String())
	require.NoError(t, err)
	indexed := IndexEntry{Hash: hash, PackID: packID, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
	limits := DefaultLimits()
	limits.BlobBytes = 2 << 20
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		hash: {Member: true, Pack: &indexed},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, _, err = store.OpenStream(context.Background(), hash)
	var limitErr *LimitError
	require.ErrorAs(t, err, &limitErr)
	assert.Equal(t, LimitBlobWindowBytes, limitErr.Dimension)
}

func TestStoreStreamsPackedObjectAboveDefaultCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a blob above the default 64 MiB policy ceiling")
	}
	size := largeStoreStreamTestBytes(t, 64<<20+1)
	layout := layoutForStoreTest(t)
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err)
	entry, err := w.AppendStream(context.Background(), io.LimitReader(streamZeroReader{}, size), uint64(size), pack.AppendStreamOptions{
		ScratchDir: staging, ScratchBytes: uint64(size)*2 + 64<<20, //nolint:gosec // helper requires positive size
	})
	require.NoError(t, err)
	packID := w.ID()
	require.NoError(t, os.MkdirAll(filepath.Dir(layout.PackPath(packID)), 0o700))
	_, err = w.Seal(layout.PackPath(packID))
	require.NoError(t, err)
	hash, err := ParseHash(entry.ID.String())
	require.NoError(t, err)
	indexed := IndexEntry{Hash: hash, PackID: packID, Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
	limits := DefaultLimits()
	limits.BlobBytes = size
	store, err := NewStore(&mapResolver{locations: map[Hash]Location{
		hash: {Member: true, Pack: &indexed},
	}}, layout, StoreOptions{Limits: limits})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	stream, gotSize, err := store.OpenStream(context.Background(), hash)
	require.NoError(t, err)
	assert.Equal(t, size, gotSize)
	require.NoError(t, stream.Verify())
	require.NoError(t, stream.Close())
}

func largeStoreStreamTestBytes(t *testing.T, fallback int64) int64 {
	t.Helper()
	value := os.Getenv("KIT_STREAM_TEST_BYTES")
	if value == "" {
		return fallback
	}
	size, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err)
	require.Positive(t, size)
	return size
}

type streamZeroReader struct{}

func (streamZeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func streamStoreForTest(t *testing.T, representation string, content []byte) (*Store, Hash) {
	t.Helper()
	layout := layoutForStoreTest(t)
	hash := hashForTest(content)
	location := Location{Member: true}
	switch representation {
	case "loose":
		require.NoError(t, os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
		require.NoError(t, os.WriteFile(layout.LoosePath(hash), content, 0o600))
	case "packed":
		entry := buildStoreTestPack(t, layout, content)
		hash = entry.Hash
		location.Pack = &entry
	default:
		require.FailNow(t, "unknown representation", representation)
	}
	return newStoreForTest(t, &mapResolver{locations: map[Hash]Location{hash: location}}, layout), hash
}

func assertStreamContent(t *testing.T, store *Store, hash Hash, want []byte) {
	t.Helper()
	stream, size, err := store.OpenStream(context.Background(), hash)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.Equal(t, int64(len(want)), size)
	assert.Equal(t, want, got)
	require.NoError(t, stream.Close())
}

func assertPackedLeases(t *testing.T, store *Store, want int) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, slot := range store.packReaders {
		assert.Equal(t, want, slot.leases)
	}
}
