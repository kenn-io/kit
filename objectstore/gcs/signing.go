package gcs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"go.kenn.io/kit/objectstore"
)

// request uses Google's documented AWS4 interoperability scope. Resumable
// session URLs are capabilities: neither transport errors nor XML messages may
// expose them. Keep status and error-code evidence without provider text.
func (s *Store) request(ctx context.Context, method string, u *url.URL, body io.Reader, size int64, headers http.Header) (*http.Response, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if size == 0 {
		body = nil
	}
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, u.String(), body)
	if err != nil {
		return nil, false, errors.New("gcs: cannot construct request")
	}
	request.ContentLength = size
	if headers != nil {
		request.Header = headers.Clone()
	}
	// GCS accepts UNSIGNED-PAYLOAD for XML signed headers. Byte integrity is
	// independently checked against the stored generation after completion.
	payload := "UNSIGNED-PAYLOAD"
	if size == 0 && method != http.MethodPost {
		empty := sha256.Sum256(nil)
		payload = hex.EncodeToString(empty[:])
	}
	request.Header.Set("x-amz-content-sha256", payload)
	if err := s.signer.SignHTTP(ctx, s.credentials, request, payload, "s3", "auto", time.Now().UTC(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }); err != nil {
		return nil, false, errors.New("gcs: cannot sign request")
	}
	// Google's front end may rewrite this header before signature verification.
	request.Header.Set("Accept-Encoding", "identity")
	response, err := s.client.Do(request)
	if err != nil {
		// url.Error includes the session URL; unwrap only that envelope.
		if e, ok := errors.AsType[*url.Error](err); ok {
			err = e.Err
		}
		return nil, sent.Load(), fmt.Errorf("gcs: request failed: %w", err)
	}
	return response, sent.Load(), nil
}

type statusError struct {
	status int
	code   string
}

func (e *statusError) Error() string { return fmt.Sprintf("gcs: HTTP %d (%s)", e.status, e.code) }
func responseError(response *http.Response) error {
	if err := responseStatusError(response); err != nil {
		return errors.Join(err, response.Body.Close())
	}
	return nil
}

// responseStatusError consumes an error body; the caller closes it.
func responseStatusError(response *http.Response) error {
	if response.StatusCode/100 == 2 {
		return nil
	}
	var result struct{ Code string }
	_ = xml.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result)
	// Whitelist code labels; unknown provider text can include session secrets.
	code := "request rejected"
	var sentinel error
	switch response.StatusCode {
	case http.StatusNotFound:
		switch result.Code {
		case "", "NoSuchKey", "NotFound":
			sentinel = objectstore.ErrNotFound
		case "NoSuchBucket":
			code = "bucket not found"
		}
	case http.StatusPreconditionFailed:
		sentinel = objectstore.ErrRevisionChanged
	}
	if result.Code == "BadDigest" || result.Code == "InvalidDigest" {
		code = "invalid checksum"
		sentinel = objectstore.ErrChecksumMismatch
	}
	return errors.Join(sentinel, &statusError{status: response.StatusCode, code: code})
}
