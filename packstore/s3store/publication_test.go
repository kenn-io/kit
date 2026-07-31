package s3store

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestVerifyPackObjectRejectsMismatchedContentLengthBeforeRead(t *testing.T) {
	body := &countingReadCloser{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 100))}
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", "100")
		return &http.Response{
			StatusCode: http.StatusOK, Header: header, Body: body,
			ContentLength: 100, Request: request,
		}, nil
	})

	_, _, err := backend.verifyPackObject(
		context.Background(),
		"0123456789abcdef0123456789abcdef",
		1,
	)

	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	assert.Zero(t, body.read)
}

func TestVerifyPackObjectBoundsReadWhenContentLengthMissing(t *testing.T) {
	const expectedSize = int64(1)
	body := &countingReadCloser{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 100))}
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: body,
			ContentLength: -1, Request: request,
		}, nil
	})

	_, _, err := backend.verifyPackObject(
		context.Background(),
		"0123456789abcdef0123456789abcdef",
		expectedSize,
	)

	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	assert.LessOrEqual(t, body.read, expectedSize+1)
}

func TestVerifyRawObjectBoundsReadWhenContentLengthMissing(t *testing.T) {
	const expectedSize = int64(1)
	body := &countingReadCloser{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 100))}
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: body,
			ContentLength: -1, Request: request,
		}, nil
	})

	err := backend.verifyRawObject(
		context.Background(),
		"loose/test",
		packstore.Hash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		expectedSize,
	)

	require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
	assert.LessOrEqual(t, body.read, expectedSize+1)
}

func TestVerifyPackObjectEnforcesConfiguredBlobLimit(t *testing.T) {
	content := []byte("published blob exceeds configured verification limit")
	packID, packBytes, _ := makePack(t, content)
	limits := packstore.DefaultLimits()
	limits.BlobBytes = int64(len(content) - 1)
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(packBytes)))
		return &http.Response{
			StatusCode: http.StatusOK, Header: header,
			Body:          io.NopCloser(bytes.NewReader(packBytes)),
			ContentLength: int64(len(packBytes)), Request: request,
		}, nil
	})

	_, _, err := backend.verifyPackObject(
		context.Background(),
		packID,
		int64(len(packBytes)),
	)

	require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
	var limit *packstore.LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, packstore.LimitBlobRawBytes, limit.Dimension)
}

func TestMultipartPublishAbortsDeduplicatedUpload(t *testing.T) {
	var aborts int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodPost && query.Has("uploads"):
			return xmlResponse(request, http.StatusOK,
				`<InitiateMultipartUploadResult>`+
					`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
					`<UploadId>upload-1</UploadId>`+
					`</InitiateMultipartUploadResult>`), nil
		case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
			response := xmlResponse(request, http.StatusOK, "")
			response.Header.Set("ETag", `"part-etag"`)
			return response, nil
		case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			return xmlResponse(request, http.StatusPreconditionFailed,
				`<Error><Code>PreconditionFailed</Code>`+
					`<Message>object already exists</Message></Error>`), nil
		case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
			aborts++
			return xmlResponse(request, http.StatusNoContent, ""), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})

	result, err := backend.multipartPublish(
		context.Background(),
		"packs/test",
		strings.NewReader("hello"),
		5,
		"",
	)

	require.NoError(t, err)
	assert.False(t, result.created)
	assert.Equal(t, 1, aborts)
}

type countingReadCloser struct {
	reader io.Reader
	read   int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func (r *countingReadCloser) Close() error { return nil }

func xmlResponse(
	request *http.Request,
	statusCode int,
	body string,
) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/xml")
	return &http.Response{
		StatusCode: statusCode,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
