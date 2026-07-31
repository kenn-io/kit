package s3store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
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
