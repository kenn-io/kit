package gcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
)

type fixtureObject struct {
	Data       []byte
	Generation string
}
type fixtureSession struct {
	Key  string
	Data []byte
}
type gcsFixture struct {
	mu                                         sync.Mutex
	objects                                    map[string]fixtureObject
	sessions                                   map[string]*fixtureSession
	next                                       int
	mode                                       string
	statuses, reads, starts, aborts, conflicts int
	requests                                   int
	final                                      chan struct{}
	release                                    chan struct{}
	server                                     *httptest.Server
	chunkFailures                              map[int64]int
	chunkStatus, headFailures, headRequests    int
	noProgress                                 int
}

func newFixture(t *testing.T, mode string) (*gcsFixture, *Store) {
	t.Helper()
	f := &gcsFixture{objects: map[string]fixtureObject{}, sessions: map[string]*fixtureSession{}, mode: mode, final: make(chan struct{}), release: make(chan struct{})}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	store, err := New(Config{Bucket: "bucket-a", Prefix: "namespace", KeyID: "test-key-id", Secret: "test-secret", Endpoint: f.server.URL, Client: f.server.Client()})
	require.NoError(t, err)
	return f, store
}

func crcValue(data []byte) string {
	return base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))))
}

func TestPublishRejectsInvalidSourceBeforeRequest(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  io.ReadSeeker
		size int64
	}{
		{name: "nil source", size: 0},
		{name: "negative size", src: bytes.NewReader(nil), size: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, s := newFixture(t, "")
			receipt, err := s.Publish(t.Context(), "key", tt.src, tt.size, sha256.Sum256(nil))
			require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
			assert.Zero(t, receipt)
			assert.Zero(t, f.requests)
		})
	}
}

type emptyReadSource struct {
	*bytes.Reader
	pauseAt int64
	paused  bool
}

func (s *emptyReadSource) Read(p []byte) (int, error) {
	if !s.paused && s.Size()-int64(s.Len()) == s.pauseAt {
		s.paused = true
		return 0, nil
	}
	return s.Reader.Read(p)
}

func TestPublishSourceLengthAfterEmptyRead(t *testing.T) {
	for _, prefix := range []string{"", "prefix"} {
		for _, suffix := range []string{"", "x"} {
			t.Run(fmt.Sprintf("prefix=%q/suffix=%q", prefix, suffix), func(t *testing.T) {
				f, s := newFixture(t, "")
				source := &emptyReadSource{Reader: bytes.NewReader([]byte(prefix + suffix)), pauseAt: int64(len(prefix))}
				receipt, err := s.Publish(t.Context(), "key", source, int64(len(prefix)), sha256.Sum256([]byte(prefix)))
				if suffix != "" {
					require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
					assert.Zero(t, receipt)
					assert.Zero(t, f.requests)
					assert.Empty(t, f.objects)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, "key", receipt.Object.Key)
				assert.Equal(t, prefix, string(f.objects["namespace/key"].Data))
			})
		}
	}
}

func (f *gcsFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-key-id/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if f.mode == "missing-bucket" || f.mode == "missing-key" {
		code := "NoSuchBucket"
		if f.mode == "missing-key" {
			code = "NoSuchKey"
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", code)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket-a/")
	if id := r.URL.Query().Get("upload_id"); id != "" {
		session, ok := f.sessions[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.sessions, id)
			f.aborts++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		cr := r.Header.Get("Content-Range")
		if strings.HasPrefix(cr, "bytes */") {
			f.statuses++
			if f.mode == "uninspectable" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if f.mode == "lost-conflict" {
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
			if obj, ok := f.objects[session.Key]; ok {
				f.headers(w, obj)
				w.WriteHeader(http.StatusOK)
				return
			}
			if len(session.Data) > 0 {
				w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(session.Data)-1))
			}
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		if f.mode == "reject-chunk" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.mode == "unexpected-chunk" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if f.noProgress > 0 {
			f.noProgress--
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		var chunkStart int64
		_, _ = fmt.Sscanf(cr, "bytes %d-", &chunkStart)
		if f.chunkFailures[chunkStart] > 0 {
			f.chunkFailures[chunkStart]--
			w.WriteHeader(f.chunkStatus)
			return
		}
		data, _ := io.ReadAll(r.Body)
		if _, exists := f.objects[session.Key]; exists {
			f.conflicts++
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		var start, end, total int64
		if len(data) == 0 && cr == "" {
			total = 0
		} else if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err != nil || end-start+1 != int64(len(data)) || start != int64(len(session.Data)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.mode == "progress" && f.statuses == 0 {
			session.Data = append(session.Data, data[:256<<10]...)
			f.statuses++
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(session.Data)-1))
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		session.Data = append(session.Data, data...)
		if int64(len(session.Data)) < total {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(session.Data)-1))
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		if f.mode == "late" {
			close(f.final)
			f.mu.Unlock()
			<-f.release
			f.mu.Lock()
		}
		f.next++
		obj := fixtureObject{Data: bytes.Clone(session.Data), Generation: strconv.Itoa(f.next)}
		if f.mode == "corrupt" {
			obj.Data = bytes.Repeat([]byte("z"), len(obj.Data))
		}
		f.objects[session.Key] = obj
		if f.mode == "lost-missing" {
			delete(f.objects, session.Key)
		}
		if f.mode == "lost-missing" || f.mode == "lost" || f.mode == "lost-conflict" || f.mode == "uninspectable" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		f.headers(w, obj)
		if f.mode == "generation-changed" {
			obj.Generation = "2"
			f.objects[session.Key] = obj
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method == http.MethodPost {
		if f.mode == "initiation-conflict" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if r.Header.Get("x-goog-if-generation-match") != "0" || r.Header.Get("x-goog-resumable") != "start" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.starts++
		id := strconv.Itoa(f.starts)
		f.sessions[id] = &fixtureSession{Key: key}
		w.Header().Set("Location", f.server.URL+r.URL.EscapedPath()+"?upload_id="+id)
		w.WriteHeader(http.StatusCreated)
		return
	}
	if r.URL.Path == "/bucket-a" || r.URL.Path == "/bucket-a/" {
		if r.URL.Query().Get("list-type") != "2" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		marker := ""
		if token := r.URL.Query().Get("continuation-token"); token != "" {
			encoded, ok := strings.CutPrefix(token, "cursor%+/")
			decoded, err := base64.RawStdEncoding.DecodeString(encoded)
			if !ok || err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			marker = string(decoded)
		}
		keys := []string{}
		for k := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) && k > marker {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		limit, _ := strconv.Atoi(r.URL.Query().Get("max-keys"))
		next := ""
		if len(keys) > limit {
			keys = keys[:limit]
			next = keys[len(keys)-1]
		}
		type item struct {
			Key          string
			Size         int
			Generation   string
			LastModified string
		}
		response := struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			Contents              []item
			IsTruncated           bool
			NextContinuationToken string `xml:",omitempty"`
			EncodingType          string `xml:",omitempty"`
		}{IsTruncated: next != ""}
		encodedNames := r.URL.Query().Get("encoding-type") == "url" && f.mode != "unencoded-listing"
		if encodedNames {
			response.EncodingType = "url"
		}
		if next != "" {
			response.NextContinuationToken = "cursor%+/" + base64.RawStdEncoding.EncodeToString([]byte(next))
		}
		for _, k := range keys {
			o := f.objects[k]
			if encodedNames {
				k = url.PathEscape(k)
			}
			response.Contents = append(response.Contents, item{Key: k, Size: len(o.Data), Generation: o.Generation, LastModified: "2026-10-15T12:00:00Z"})
		}
		_ = xml.NewEncoder(w).Encode(response)
		return
	}
	obj, ok := f.objects[key]
	if r.Method == http.MethodHead {
		f.headRequests++
		if f.headFailures > 0 {
			f.headFailures--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	if r.Method == http.MethodHead && f.mode == "uninspectable" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if generation := r.Header.Get("x-goog-if-generation-match"); generation != "" && generation != obj.Generation {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	f.headers(w, obj)
	switch r.Method {
	case http.MethodHead:
	case http.MethodDelete:
		delete(f.objects, key)
		w.Header().Del("Content-Length")
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		f.reads++
		data := obj.Data
		if r.Header.Get("Range") != "" {
			var start, end int
			_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
			if start < 0 || end >= len(data) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			data = data[start : end+1]
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		if f.mode == "truncated" {
			data = data[:len(data)-1]
		}
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *gcsFixture) headers(w http.ResponseWriter, o fixtureObject) {
	w.Header().Set("x-goog-generation", o.Generation)
	w.Header().Set("x-goog-hash", "crc32c="+crcValue(o.Data))
	w.Header().Set("Content-Length", strconv.Itoa(len(o.Data)))
	w.Header().Set("Last-Modified", time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat))
}

func TestResumableConditionalPublish(t *testing.T) {
	f, s := newFixture(t, "")
	data := []byte("verified bytes")
	digest := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), digest)
	require.NoError(t, err)
	assert.Equal(t, "provider-crc32c", receipt.Verification)
	assert.Equal(t, data, f.objects["namespace/object"].Data)
	again, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), digest)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", again.Verification)
	assert.Equal(t, receipt.Object.Revision, again.Object.Revision)
	assert.Positive(t, f.reads)
	different := []byte("different data")
	zero, overwriteErr := s.Publish(t.Context(), "object", bytes.NewReader(different), int64(len(different)), sha256.Sum256(different))
	require.ErrorIs(t, overwriteErr, objectstore.ErrExists)
	assert.Zero(t, zero)
	assert.Equal(t, data, f.objects["namespace/object"].Data)
	require.NoError(t, s.Delete(t.Context(), "object", receipt.Object.Revision))
	require.NoError(t, s.Delete(t.Context(), "object", receipt.Object.Revision))
}

func TestMissingBucketIsNotMissingObject(t *testing.T) {
	for _, operation := range []string{"delete", "read", "list", "publish"} {
		t.Run(operation, func(t *testing.T) {
			_, s := newFixture(t, "missing-bucket")
			var err error
			switch operation {
			case "delete":
				err = s.Delete(t.Context(), "object", "")
			case "read":
				var body io.ReadCloser
				body, err = s.OpenRange(t.Context(), "object", "", 0, 1)
				assert.Nil(t, body)
			case "list":
				_, err = s.ListPage(t.Context(), "", "", 1)
			case "publish":
				var receipt objectstore.Receipt
				receipt, err = s.Publish(t.Context(), "object", bytes.NewReader([]byte("x")), 1, sha256.Sum256([]byte("x")))
				assert.Zero(t, receipt)
			}
			require.Error(t, err)
			require.NotErrorIs(t, err, objectstore.ErrNotFound)
			require.ErrorContains(t, err, "bucket not found")
		})
	}
}

func TestMissingObjectRemainsIdempotent(t *testing.T) {
	_, s := newFixture(t, "missing-key")
	require.NoError(t, s.Delete(t.Context(), "object", ""))
	body, err := s.OpenRange(t.Context(), "object", "", 0, 1)
	require.ErrorIs(t, err, objectstore.ErrNotFound)
	assert.Nil(t, body)
	_, err = s.Inspect(t.Context(), "object")
	require.ErrorIs(t, err, objectstore.ErrNotFound)
}

func TestGenerationChanged(t *testing.T) {
	f, s := newFixture(t, "")
	f.objects["namespace/object"] = fixtureObject{Data: []byte("data"), Generation: "2"}
	_, changedErr := s.OpenRange(t.Context(), "object", "1", 0, 1)
	require.ErrorIs(t, changedErr, objectstore.ErrRevisionChanged)
	require.ErrorIs(t, s.Delete(t.Context(), "object", "1"), objectstore.ErrRevisionChanged)
	assert.Equal(t, "2", f.objects["namespace/object"].Generation)
}

func TestResumableRetriesEachChunk(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f, s := newFixture(t, "")
			f.chunkStatus = status
			f.chunkFailures = map[int64]int{0: 1, 8 << 20: 1, 16 << 20: 1}
			data := bytes.Repeat([]byte("x"), (16<<20)+1)
			receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
			require.NoError(t, err)
			assert.Equal(t, sha256.Sum256(data), receipt.SHA256)
			assert.Equal(t, data, f.objects["namespace/object"].Data)
			assert.Equal(t, 3, f.statuses)
			assert.Zero(t, f.aborts)
		})
	}
}

func TestResumableRetriesNoProgress(t *testing.T) {
	for _, replies := range []int{1, 10} {
		t.Run(strconv.Itoa(replies), func(t *testing.T) {
			f, s := newFixture(t, "")
			f.noProgress = replies
			data := []byte("bytes")
			receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), 5, sha256.Sum256(data))
			if replies > 1 {
				require.Error(t, err)
				assert.Zero(t, receipt)
				assert.Empty(t, f.objects)
				assert.Equal(t, 1, f.aborts)
				assert.Equal(t, 7, f.noProgress)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, data, f.objects["namespace/object"].Data)
		})
	}
}

func TestVerificationRetriesTransientHead(t *testing.T) {
	f, s := newFixture(t, "")
	f.headFailures = 1
	data := []byte("stored")
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), 6, sha256.Sum256(data))
	require.NoError(t, err)
	assert.Equal(t, "provider-crc32c", receipt.Verification)
	assert.Equal(t, 2, f.headRequests)
	assert.Equal(t, 1, f.starts)
}

type observedCloseBody struct {
	io.ReadCloser
	onClose func()
}

func (b *observedCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.onClose()
	return err
}

func TestInspectionCancellationStopsRetries(t *testing.T) {
	f, s := newFixture(t, "")
	f.headFailures = 10
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	transport := f.server.Client().Transport
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil {
			response.Body = &observedCloseBody{ReadCloser: response.Body, onClose: cancel}
		}
		return response, err
	})
	_, err := s.Inspect(ctx, "object")
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, f.headRequests)
}

func TestUnexpectedChunkStatusClosesBody(t *testing.T) {
	f, s := newFixture(t, "unexpected-chunk")
	closed := false
	transport := f.server.Client().Transport
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil && r.Method == http.MethodPut {
			response.Body = &observedCloseBody{ReadCloser: response.Body, onClose: func() { closed = true }}
		}
		return response, err
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	data := bytes.Repeat([]byte("x"), (8<<20)+1)
	receipt, err := s.Publish(ctx, "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.Zero(t, receipt)
	assert.True(t, closed)
	assert.Empty(t, f.objects)
	assert.Equal(t, 1, f.aborts)
}

func TestCreateRaceHasOnlyConflictIdentity(t *testing.T) {
	f, s := newFixture(t, "")
	f.objects["namespace/object"] = fixtureObject{Data: []byte("different"), Generation: "7"}
	data := []byte("source")
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), 6, sha256.Sum256(data))
	require.ErrorIs(t, err, objectstore.ErrExists)
	require.NotErrorIs(t, err, objectstore.ErrRevisionChanged)
	require.NotErrorIs(t, err, objectstore.ErrNotFound)
	assert.Zero(t, receipt)
	assert.Equal(t, "different", string(f.objects["namespace/object"].Data))
}

func TestStoredChecksumMismatch(t *testing.T) {
	f, s := newFixture(t, "corrupt")
	data := []byte("good bytes")
	receipt, mismatchErr := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.ErrorIs(t, mismatchErr, objectstore.ErrChecksumMismatch)
	assert.Zero(t, receipt)
	assert.Equal(t, bytes.Repeat([]byte("z"), len(data)), f.objects["namespace/object"].Data)
	assert.Zero(t, f.aborts)
}

func TestResumeAfterLostResponse(t *testing.T) {
	for _, mode := range []string{"lost", "progress", "uninspectable"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newFixture(t, mode)
			data := bytes.Repeat([]byte("x"), (8<<20)+7)
			receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
			if mode == "uninspectable" {
				require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
				assert.Zero(t, receipt)
			} else {
				require.NoError(t, err)
				assert.Equal(t, sha256.Sum256(data), receipt.SHA256)
				if mode == "lost" {
					assert.Equal(t, "readback-sha256", receipt.Verification)
				}
			}
			assert.Equal(t, data, f.objects["namespace/object"].Data)
			assert.Positive(t, f.statuses)
		})
	}
}

func TestLateCompletionAfterCancellation(t *testing.T) {
	f, s := newFixture(t, "late")
	ctx, cancel := context.WithCancel(t.Context())
	data := []byte("late bytes")
	result := make(chan error, 1)
	go func() {
		receipt, err := s.Publish(ctx, "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
		if receipt.Verification != "" {
			result <- errors.New("unexpected receipt")
			return
		}
		result <- err
	}()
	<-f.final
	cancel()
	err := <-result
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	close(f.release)
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return bytes.Equal(data, f.objects["namespace/object"].Data)
	}, time.Second, time.Millisecond)
}

func TestLostCompletionThenConflict(t *testing.T) {
	f, s := newFixture(t, "lost-conflict")
	data := []byte("stored before lost reply")
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
	assert.Equal(t, data, f.objects["namespace/object"].Data)
	assert.Equal(t, "1", receipt.Object.Revision)
	assert.Equal(t, 1, f.conflicts)
}

func TestInitiationConflictReadback(t *testing.T) {
	f, s := newFixture(t, "initiation-conflict")
	data := []byte("existing")
	f.objects["namespace/object"] = fixtureObject{Data: data, Generation: "7"}
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
	assert.Equal(t, "7", receipt.Object.Revision)
	assert.Empty(t, f.sessions)
}

func TestCompletedGenerationChangesBeforeInspection(t *testing.T) {
	f, s := newFixture(t, "generation-changed")
	data := []byte("verified bytes")
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	require.ErrorIs(t, err, objectstore.ErrRevisionChanged)
	assert.Zero(t, receipt)
	assert.Equal(t, "2", f.objects["namespace/object"].Generation)
}

func TestMissingAfterAmbiguousCompletion(t *testing.T) {
	f, s := newFixture(t, "lost-missing")
	data := []byte("uncertain")
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	require.ErrorIs(t, err, objectstore.ErrNotFound)
	assert.Zero(t, receipt)
	assert.NotContains(t, f.objects, "namespace/object")
}

func TestAbortOnlyOwnedSession(t *testing.T) {
	f, s := newFixture(t, "reject-chunk")
	f.sessions["foreign"] = &fixtureSession{Key: "namespace/foreign"}
	data := bytes.Repeat([]byte("x"), (8<<20)+1)
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.Error(t, err)
	require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	assert.Len(t, f.sessions, 1)
	assert.Contains(t, f.sessions, "foreign")
	assert.Equal(t, 1, f.aborts)
	assert.Empty(t, f.objects)
}
