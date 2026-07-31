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
	var limit *packstore.LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, packstore.LimitBlobRawBytes, limit.Dimension)
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
	health.Observe(primary, err)

	ordered := health.Order([]packstore.ReadLocation{primary, secondary})
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
