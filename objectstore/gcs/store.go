package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go/encoding/httpbinding"
	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/kit/objectstore"
)

// Config binds an existing bucket namespace and explicit GCS HMAC credentials.
// Endpoint defaults to the GCS XML API. The default client has a two-minute
// request timeout.
// Custom HTTP endpoints are intended for caller-owned contract fixtures.
type Config struct {
	Bucket, Prefix string
	KeyID, Secret  string
	Endpoint       string
	Client         *http.Client
}

// Store provides immutable byte operations through the GCS XML API.
type Store struct {
	bucket, prefix string
	endpoint       *url.URL
	client         *http.Client
	credentials    aws.Credentials
	signer         *v4.Signer
}

// New validates configuration without contacting the service or loading ambient
// credentials. GCS HMAC keys are distinct from OAuth and service-account JSON.
func New(cfg Config) (*Store, error) {
	if cfg.Bucket == "" || strings.ContainsAny(cfg.Bucket, "/\\?#") || cfg.KeyID == "" || cfg.Secret == "" {
		return nil, errors.New("gcs: bucket and explicit HMAC credentials are required")
	}
	if err := objectstore.ValidatePrefix(cfg.Prefix); err != nil {
		return nil, err
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://storage.googleapis.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("gcs: endpoint must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	// A resumable response is a protocol message, including 308. Never let the
	// HTTP client follow redirects carrying credentials or a session capability.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	prefix := strings.TrimSuffix(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Store{bucket: cfg.Bucket, prefix: prefix, endpoint: u, client: &copyClient, credentials: aws.Credentials{AccessKeyID: cfg.KeyID, SecretAccessKey: cfg.Secret}, signer: v4.NewSigner()}, nil
}

func (s *Store) key(key string) (string, error) {
	if err := objectstore.ValidateKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

func (s *Store) objectURL(key string) *url.URL {
	u := *s.endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + s.bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = httpbinding.EscapePath(u.Path, false)
	return &u
}

// OpenRange returns exactly length bytes at offset from the current generation,
// guarded by revision when supplied. The caller must close the returned body.
func (s *Store) OpenRange(ctx context.Context, key, revision string, offset, length int64) (io.ReadCloser, error) {
	full, err := s.key(key)
	if err != nil {
		return nil, err
	}
	if offset < 0 || length <= 0 || offset > math.MaxInt64-length {
		return nil, errors.New("gcs: invalid range")
	}
	headers := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)}}
	if revision != "" {
		headers.Set("x-goog-if-generation-match", revision)
	}
	response, _, err := s.request(ctx, http.MethodGet, s.objectURL(full), nil, 0, headers)
	if err != nil {
		return nil, err
	}
	if err := responseError(response); err != nil {
		return nil, err
	}
	var start, end, total int64
	_, parseErr := fmt.Sscanf(response.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
	if response.StatusCode != http.StatusPartialContent || parseErr != nil || start != offset || end != offset+length-1 || total <= end || response.ContentLength != length {
		return nil, errors.Join(errors.New("gcs: response does not match requested range"), response.Body.Close())
	}
	if revision != "" && response.Header.Get("x-goog-generation") != revision {
		return nil, errors.Join(objectstore.ErrRevisionChanged, response.Body.Close())
	}
	return &rangeBody{ctx: ctx, body: response.Body, remaining: length}, nil
}

type rangeBody struct {
	ctx       context.Context
	body      io.ReadCloser
	remaining int64
}

func (r *rangeBody) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.body.Read(p)
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) && r.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
func (r *rangeBody) Close() error { return r.body.Close() }

func metadata(key string, r *http.Response) (objectstore.Metadata, error) {
	if r.ContentLength < 0 || r.Header.Get("x-goog-generation") == "" {
		return objectstore.Metadata{}, errors.New("gcs: missing object length or generation")
	}
	m := objectstore.Metadata{Key: key, Size: r.ContentLength, Revision: r.Header.Get("x-goog-generation")}
	if modified := r.Header.Get("Last-Modified"); modified != "" {
		parsed, err := http.ParseTime(modified)
		if err != nil {
			return objectstore.Metadata{}, errors.New("gcs: invalid modification time")
		}
		m.LastModified = parsed.UTC()
	}
	for _, value := range r.Header.Values("x-goog-hash") {
		for part := range strings.SplitSeq(value, ",") {
			algorithm, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && (algorithm == "crc32c" || algorithm == "md5") {
				m.Checksums = append(m.Checksums, objectstore.Checksum{Algorithm: algorithm, Value: value, Scope: "full"})
			}
		}
	}
	return m, nil
}

// Inspect observes the current object's length, generation, and provider checksums.
func (s *Store) Inspect(ctx context.Context, key string) (objectstore.Metadata, error) {
	return s.inspect(ctx, key, "")
}

func (s *Store) inspect(ctx context.Context, key, generation string) (objectstore.Metadata, error) {
	full, err := s.key(key)
	if err != nil {
		return objectstore.Metadata{}, err
	}
	headers := http.Header{}
	if generation != "" {
		headers.Set("x-goog-if-generation-match", generation)
	}
	var m objectstore.Metadata
	err = retryOperation(ctx, func() (bool, error) {
		response, _, err := s.request(ctx, http.MethodHead, s.objectURL(full), nil, 0, headers)
		if err != nil {
			return false, err
		}
		if err := responseError(response); err != nil {
			if retryableStatus(response.StatusCode) {
				return false, err
			}
			return false, backoff.Permanent(err)
		}
		m, err = metadata(key, response)
		if err == nil && generation != "" && m.Revision != generation {
			err = objectstore.ErrRevisionChanged
		}
		if err := errors.Join(err, response.Body.Close()); err != nil {
			return false, backoff.Permanent(err)
		}
		return true, nil
	})
	return m, err
}

// Delete removes the current object only if revision matches when supplied.
// An already absent object succeeds. Listing grants no authority to delete.
func (s *Store) Delete(ctx context.Context, key, revision string) error {
	full, err := s.key(key)
	if err != nil {
		return err
	}
	headers := http.Header{}
	if revision != "" {
		headers.Set("x-goog-if-generation-match", revision)
	}
	response, _, err := s.request(ctx, http.MethodDelete, s.objectURL(full), nil, 0, headers)
	if err != nil {
		return err
	}
	err = responseStatusError(response)
	if errors.Is(err, objectstore.ErrNotFound) {
		err = nil
	}
	return errors.Join(err, response.Body.Close())
}
