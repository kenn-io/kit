package s3store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestDownloadPackRangesRejectsOversizedObjectBeforeGET(t *testing.T) {
	limits := packstore.DefaultLimits()
	limits.PackBytes = 10
	var getRequests int
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodHead {
			header := make(http.Header)
			header.Set("Content-Length", strconv.FormatInt(limits.PackBytes+1, 10))
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        header,
				Body:          io.NopCloser(bytes.NewReader(nil)),
				ContentLength: limits.PackBytes + 1,
				Request:       request,
			}, nil
		}
		getRequests++
		return nil, errors.New("unexpected range GET")
	})

	_, _, err := backend.downloadPackRanges(
		context.Background(),
		"0123456789abcdef0123456789abcdef",
	)

	require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	var limit *packstore.LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, packstore.LimitPackContainerBytes, limit.Dimension)
	assert.Zero(t, getRequests)
}

func TestPackReaderOptionsUseConfiguredLimits(t *testing.T) {
	backend := &Backend{limits: packstore.Limits{
		BlobBytes: 4096, PackBytes: 8192, FooterBytes: 2048, PackEntries: 32,
	}}

	assert.Equal(t, pack.ReaderOptions{Limits: pack.ReaderLimits{
		ContainerBytes: 8192,
		FooterBytes:    2048,
		Entries:        32,
		RawBytes:       4096,
		StoredBytes:    4096,
		WindowBytes:    4096,
	}}, backend.packReaderOptions())
}

func TestOpenPackEnforcesConfiguredBlobLimit(t *testing.T) {
	content := []byte("blob exceeds configured S3 reader limit")
	_, packBytes, indexed := makePack(t, content)
	limits := packstore.DefaultLimits()
	limits.BlobBytes = int64(len(content) - 1)
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		if request.Method == http.MethodHead {
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(nil)),
				ContentLength: int64(len(packBytes)), Request: request,
			}, nil
		}
		header.Set(
			"Content-Range",
			"bytes 0-"+strconv.Itoa(len(packBytes)-1)+"/"+strconv.Itoa(len(packBytes)),
		)
		return &http.Response{
			StatusCode: http.StatusPartialContent, Header: header,
			Body:          io.NopCloser(bytes.NewReader(packBytes)),
			ContentLength: int64(len(packBytes)), Request: request,
		}, nil
	})

	_, _, err := backend.OpenPack(
		context.Background(),
		indexed.Hash,
		indexed,
	)

	require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
	require.NotErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	var limit *packstore.LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, packstore.LimitBlobRawBytes, limit.Dimension)
}

func TestOversizedS3ReplicaFallsBackToHealthyCandidate(t *testing.T) {
	content := []byte("healthy replica content")
	_, packBytes, indexed := makePack(t, content)
	limits := packstore.DefaultLimits()
	limits.PackBytes = int64(len(packBytes) - 1)
	oversized := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodHead, request.Method)
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		return &http.Response{
			StatusCode: http.StatusOK, Header: header,
			Body:          io.NopCloser(bytes.NewReader(nil)),
			ContentLength: int64(len(packBytes)), Request: request,
		}, nil
	})
	healthy := &memoryReadBackend{content: content}
	store, err := packstore.NewMultiStore(
		staticReadLocationResolver{resolution: packstore.Resolution{
			Member: true,
			Candidates: []packstore.ReadLocation{
				{StoreID: "oversized", Generation: "oversized-1", Pack: &indexed},
				{StoreID: "healthy", Generation: "healthy-1", Pack: &indexed},
			},
		}},
		staticReadBackendRegistry{
			"oversized": oversized,
			"healthy":   healthy,
		},
		packstore.MultiStoreOptions{},
	)
	require.NoError(t, err)

	stream, size, err := store.OpenStream(context.Background(), indexed.Hash)
	require.NoError(t, err)
	got, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())

	assert.Equal(t, int64(len(content)), size)
	assert.Equal(t, content, got)
	assert.Equal(t, 1, healthy.opens)
}

func TestS3PackRepresentationLimitsFallBackToHealthyCandidate(t *testing.T) {
	content := []byte("healthy S3 representation fallback")
	_, packBytes, entries := makePackEntries(t, content, []byte("second footer entry"))
	tests := []struct {
		name      string
		dimension packstore.LimitDimension
		limit     func(packstore.Limits) packstore.Limits
	}{
		{
			name:      "footer bytes",
			dimension: packstore.LimitPackFooterBytes,
			limit: func(limits packstore.Limits) packstore.Limits {
				limits.FooterBytes = 1
				return limits
			},
		},
		{
			name:      "entry count",
			dimension: packstore.LimitPackEntryCount,
			limit: func(limits packstore.Limits) packstore.Limits {
				limits.PackEntries = 1
				return limits
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limited := newHTTPBackend(tt.limit(packstore.DefaultLimits()), func(request *http.Request) (*http.Response, error) {
				header := make(http.Header)
				header.Set("Content-Length", strconv.Itoa(len(packBytes)))
				if request.Method == http.MethodHead {
					return &http.Response{
						StatusCode: http.StatusOK, Header: header,
						Body: io.NopCloser(bytes.NewReader(nil)), ContentLength: int64(len(packBytes)),
						Request: request,
					}, nil
				}
				header.Set(
					"Content-Range",
					"bytes 0-"+strconv.Itoa(len(packBytes)-1)+"/"+strconv.Itoa(len(packBytes)),
				)
				return &http.Response{
					StatusCode: http.StatusPartialContent, Header: header,
					Body: io.NopCloser(bytes.NewReader(packBytes)), ContentLength: int64(len(packBytes)),
					Request: request,
				}, nil
			})
			healthy := &memoryReadBackend{content: content}
			store, err := packstore.NewMultiStore(
				staticReadLocationResolver{resolution: packstore.Resolution{
					Member: true,
					Candidates: []packstore.ReadLocation{
						{StoreID: "limited", Generation: "limited-1", Pack: &entries[0]},
						{StoreID: "healthy", Generation: "healthy-1", Pack: &entries[0]},
					},
				}},
				staticReadBackendRegistry{"limited": limited, "healthy": healthy},
				packstore.MultiStoreOptions{},
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })

			stream, size, err := store.OpenStream(context.Background(), entries[0].Hash)
			require.NoError(t, err)
			got, err := io.ReadAll(stream)
			require.NoError(t, errors.Join(err, stream.Close()))
			assert.Equal(t, content, got)
			assert.Equal(t, int64(len(content)), size)

			_, _, err = limited.OpenPack(context.Background(), entries[0].Hash, entries[0])
			require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
			require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
			var limit *packstore.LimitError
			require.ErrorAs(t, err, &limit)
			assert.Equal(t, tt.dimension, limit.Dimension)
		})
	}
}

func TestPackBodyClassifiesTerminalReadCorruption(t *testing.T) {
	body, _ := newPackBody(t, true)

	_, err := io.ReadAll(body)

	require.ErrorIs(t, err, pack.ErrCorrupt)
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
}

func TestPackBodyClassifiesTerminalVerifyCorruption(t *testing.T) {
	body, _ := newPackBody(t, true)

	err := body.Verify()

	require.ErrorIs(t, err, pack.ErrCorrupt)
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
}

func TestPackBodyDoesNotClassifyIncompleteCloseAsCorrupt(t *testing.T) {
	body, _ := newPackBody(t, false)

	err := body.Close()

	require.ErrorIs(t, err, pack.ErrVerificationIncomplete)
	require.NotErrorIs(t, err, packstore.ErrPhysicalCorrupt)
}

func TestPackBodyPreservesVerifiedEOF(t *testing.T) {
	body, _ := newPackBody(t, false)

	got, err := io.ReadAll(body)

	require.NoError(t, err)
	assert.Equal(t, []byte("terminal S3 pack integrity"), got)
	assert.True(t, body.Verified())
}

func TestS3TerminalCorruptionDemotesGeneration(t *testing.T) {
	body, indexed := newPackBody(t, true)
	primary := packstore.ReadLocation{
		StoreID: "primary", Generation: "primary-1", Pack: &indexed,
	}
	secondary := primary
	secondary.StoreID = "secondary"
	secondary.Generation = "secondary-1"
	health := packstore.NewHealth()

	err := body.Verify()
	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	health.Observe(indexed.Hash, primary, err)

	ordered := health.Order(indexed.Hash, []packstore.ReadLocation{primary, secondary})
	require.Len(t, ordered, 2)
	assert.Equal(t, secondary, ordered[0])
	assert.Equal(t, primary, ordered[1])
}

func newPackBody(
	t *testing.T,
	corrupt bool,
) (*packBody, packstore.IndexEntry) {
	t.Helper()
	content := []byte("terminal S3 pack integrity")
	packID, packBytes, indexed := makePack(t, content)
	if corrupt {
		packBytes[indexed.Offset] ^= 0xff
	}
	path := filepath.Join(t.TempDir(), packID+".pack")
	require.NoError(t, os.WriteFile(path, packBytes, 0o600))
	reader, err := pack.OpenReader(path, nil)
	require.NoError(t, err)
	blob, err := reader.OpenBlob(context.Background(), reader.Entries()[0])
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = blob.Close()
		_ = reader.Close()
		_ = os.Remove(path)
	})
	return &packBody{blob: blob, reader: reader, path: path}, indexed
}

type staticReadLocationResolver struct {
	resolution packstore.Resolution
}

func (r staticReadLocationResolver) ResolveLocations(
	context.Context,
	packstore.Hash,
) (packstore.Resolution, error) {
	return r.resolution, nil
}

type staticReadBackendRegistry map[packstore.StoreID]packstore.ReadBackend

func (r staticReadBackendRegistry) Backend(
	id packstore.StoreID,
) (packstore.ReadBackend, bool) {
	backend, ok := r[id]
	return backend, ok
}

type memoryReadBackend struct {
	content []byte
	opens   int
}

func (b *memoryReadBackend) OpenLoose(
	context.Context,
	packstore.Hash,
	packstore.LooseLocation,
) (packstore.VerifiedReadCloser, int64, error) {
	return b.open()
}

func (b *memoryReadBackend) OpenPack(
	context.Context,
	packstore.Hash,
	packstore.IndexEntry,
) (packstore.VerifiedReadCloser, int64, error) {
	return b.open()
}

func (b *memoryReadBackend) open() (packstore.VerifiedReadCloser, int64, error) {
	b.opens++
	return &memoryVerifiedReader{
		Reader: bytes.NewReader(b.content),
	}, int64(len(b.content)), nil
}

type memoryVerifiedReader struct {
	*bytes.Reader
	verified bool
}

func (r *memoryVerifiedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.verified = true
	}
	return n, err
}

func (r *memoryVerifiedReader) Verify() error {
	_, err := io.Copy(io.Discard, r)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (r *memoryVerifiedReader) Verified() bool { return r.verified }

func (r *memoryVerifiedReader) Close() error {
	if r.verified {
		return nil
	}
	return pack.ErrVerificationIncomplete
}
