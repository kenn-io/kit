package s3store

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
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

func TestVerifyPackObjectEnforcesConfiguredDecoderWindowLimit(t *testing.T) {
	content := bytes.Repeat([]byte("window policy "), 1<<16)
	packID, packBytes := makeEncodedPack(
		t,
		content,
		uint64(len(content)),
		8<<20,
	)
	limits := packstore.DefaultLimits()
	limits.BlobBytes = 2 << 20
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
	assert.Equal(t, packstore.LimitBlobWindowBytes, limit.Dimension)
}

func TestVerifyPackObjectRejectsDecodedLengthMismatch(t *testing.T) {
	content := bytes.Repeat([]byte("remote decoded length authority "), 4096)
	tests := []struct {
		name   string
		rawLen uint64
	}{
		{name: "decoded content is longer", rawLen: uint64(len(content) - 1)},
		{name: "decoded content is shorter", rawLen: uint64(len(content) + 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packID, packBytes := makeEncodedPack(t, content, tt.rawLen, 0)
			backend := newHTTPBackend(
				packstore.DefaultLimits(),
				func(request *http.Request) (*http.Response, error) {
					header := make(http.Header)
					header.Set("Content-Length", strconv.Itoa(len(packBytes)))
					return &http.Response{
						StatusCode: http.StatusOK, Header: header,
						Body:          io.NopCloser(bytes.NewReader(packBytes)),
						ContentLength: int64(len(packBytes)), Request: request,
					}, nil
				},
			)

			_, _, err := backend.verifyPackObject(
				context.Background(),
				packID,
				int64(len(packBytes)),
			)

			require.ErrorIs(t, err, pack.ErrCorrupt)
			require.ErrorIs(t, err, packstore.ErrPhysicalCorrupt)
		})
	}
}

func TestPublishPackRejectsKnownConfiguredLimitBeforeMultipart(t *testing.T) {
	limits := packstore.DefaultLimits()
	limits.PackBytes = 8
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	var requests int
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 && request.Method == http.MethodGet {
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(marker)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(marker)),
				ContentLength: int64(len(marker)), Request: request,
			}, nil
		}
		return xmlResponse(
			request,
			http.StatusInternalServerError,
			`<Error><Code>UnexpectedRequest</Code></Error>`,
		), nil
	})
	backend.setOwnership(owner, `"owner-etag"`)
	source := &countingReadCloser{
		reader: bytes.NewReader(bytes.Repeat([]byte("x"), 9)),
	}

	_, err = backend.PublishPack(
		context.Background(),
		pack.NewPackID(),
		source,
		packstore.PublishOptions{
			ExpectedSize: 9, SizeKnown: true, MaxBytes: 100,
		},
	)

	require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
	var limit *packstore.LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, packstore.LimitPackContainerBytes, limit.Dimension)
	assert.Zero(t, source.read)
	assert.Equal(t, 1, requests)
}

func TestPublishPackCapsCallerLimitAndAbortsMultipart(t *testing.T) {
	limits := packstore.DefaultLimits()
	limits.PackBytes = 8
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	var uploads, completes, aborts int
	backend := newHTTPBackend(limits, func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodGet:
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(marker)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(marker)),
				ContentLength: int64(len(marker)), Request: request,
			}, nil
		case request.Method == http.MethodPost && query.Has("uploads"):
			return xmlResponse(request, http.StatusOK,
				`<InitiateMultipartUploadResult>`+
					`<Bucket>test-bucket</Bucket><Key>packs/test</Key>`+
					`<UploadId>upload-1</UploadId>`+
					`</InitiateMultipartUploadResult>`), nil
		case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
			uploads++
			response := xmlResponse(request, http.StatusOK, "")
			response.Header.Set("ETag", `"part-etag"`)
			return response, nil
		case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			completes++
			return xmlResponse(request, http.StatusOK, ""), nil
		case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
			aborts++
			return xmlResponse(request, http.StatusNoContent, ""), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})
	backend.setOwnership(owner, `"owner-etag"`)

	_, err = backend.PublishPack(
		context.Background(),
		pack.NewPackID(),
		bytes.NewReader(bytes.Repeat([]byte("x"), 9)),
		packstore.PublishOptions{MaxBytes: 100},
	)

	require.ErrorIs(t, err, packstore.ErrBlobTooLarge)
	var limit *packstore.LimitError
	require.ErrorAs(t, err, &limit)
	assert.Equal(t, packstore.LimitPackContainerBytes, limit.Dimension)
	assert.Equal(t, 1, uploads)
	assert.Zero(t, completes)
	assert.Equal(t, 1, aborts)
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

func TestRepairLooseCancelStopsBeforePut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &cancelAfterFirstRead{
		cancel: cancel,
		reader: strings.NewReader("hello"),
	}
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	var puts int
	backend := newHTTPBackend(packstore.DefaultLimits(), func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodGet:
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(marker)))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body:          io.NopCloser(bytes.NewReader(marker)),
				ContentLength: int64(len(marker)), Request: request,
			}, nil
		case http.MethodPut:
			puts++
		}
		return xmlResponse(request, http.StatusInternalServerError,
			`<Error><Code>UnexpectedRequest</Code></Error>`), nil
	})
	backend.setOwnership(owner, `"owner-etag"`)

	_, err = backend.RepairLoose(
		ctx,
		packstore.Hash("2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"),
		source,
		packstore.PublishOptions{ExpectedSize: 5, SizeKnown: true},
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, source.reads)
	assert.Zero(t, puts)
}

func TestMultipartPublishCancelAbortsWithBoundedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &cancelAfterFirstRead{
		cancel: cancel,
		reader: strings.NewReader("hello"),
	}
	var uploads, completes, aborts int
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
			uploads++
			return xmlResponse(request, http.StatusOK, ""), nil
		case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			completes++
			return xmlResponse(request, http.StatusOK, ""), nil
		case request.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
			assert.NoError(t, request.Context().Err())
			_, hasDeadline := request.Context().Deadline()
			assert.True(t, hasDeadline)
			aborts++
			return xmlResponse(request, http.StatusNoContent, ""), nil
		default:
			return xmlResponse(request, http.StatusInternalServerError,
				`<Error><Code>UnexpectedRequest</Code></Error>`), nil
		}
	})

	_, err := backend.multipartPublish(ctx, "packs/test", source, 5, "")

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, source.reads)
	assert.Zero(t, uploads)
	assert.Zero(t, completes)
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

type cancelAfterFirstRead struct {
	cancel context.CancelFunc
	reader io.Reader
	reads  int
}

func (r *cancelAfterFirstRead) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	n, err := r.reader.Read(p)
	r.reads++
	if n > 0 {
		r.cancel()
	}
	return n, err
}

func makeEncodedPack(
	t *testing.T,
	content []byte,
	rawLen uint64,
	windowBytes int,
) (string, []byte) {
	t.Helper()
	var frame bytes.Buffer
	options := []zstd.EOption{zstd.WithEncoderConcurrency(1)}
	if windowBytes > 0 {
		options = append(options, zstd.WithWindowSize(windowBytes))
	}
	encoder, err := zstd.NewWriter(&frame, options...)
	require.NoError(t, err)
	_, err = encoder.Write(content)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	staging := t.TempDir()
	writer, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err)
	_, err = writer.AppendEncoded(
		pack.ComputeBlobID(content),
		frame.Bytes(),
		rawLen,
		true,
	)
	require.NoError(t, err)
	packID := writer.ID()
	packPath := filepath.Join(staging, packID+".pack")
	_, err = writer.Seal(packPath)
	require.NoError(t, err)
	packBytes, err := os.ReadFile(packPath)
	require.NoError(t, err)
	return packID, packBytes
}

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
