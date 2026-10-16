package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
)

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
			f, s := newFixture(t)
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
				f, s := newFixture(t)
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
				assert.Equal(t, prefix, string(f.objects["namespace/key"]))
			})
		}
	}
}

func TestMultipartSourceLengthAfterEmptyRead(t *testing.T) {
	for _, suffix := range []string{"", "x"} {
		t.Run(fmt.Sprintf("suffix=%q", suffix), func(t *testing.T) {
			f, s := newFixture(t)
			data := bytes.Repeat([]byte("m"), (5<<20)+17)
			source := &emptyReadSource{Reader: bytes.NewReader(append(bytes.Clone(data), suffix...)), pauseAt: int64(len(data))}
			// Exercise the finalization guard independently of pre-upload validation.
			var state dispatchState
			err := s.multipart(t.Context(), "namespace/key", source, int64(len(data)), sha256.Sum256(data), crcValue(data), &state)
			if suffix != "" {
				require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
				assert.False(t, state.sent.Load())
				assert.Equal(t, 1, f.aborts)
				assert.Empty(t, f.objects)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, data, f.objects["namespace/key"])
			assert.Zero(t, f.aborts)
		})
	}
}

func TestPublishRejectsPartCapacityBeforeRequest(t *testing.T) {
	f, s := newFixture(t)
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(nil), 10000*s.part+1, sha256.Sum256(nil))
	require.Error(t, err)
	require.NotErrorIs(t, err, objectstore.ErrChecksumMismatch)
	assert.Zero(t, receipt)
	assert.Zero(t, f.requests)
}

func TestMultipartFullChecksum(t *testing.T) {
	f, s := newFixture(t)
	data := bytes.Repeat([]byte("m"), (5<<20)+17)
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "large", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "provider-crc32c", receipt.Verification)
	assert.Equal(t, sum, sha256.Sum256(f.objects["namespace/large"]))
	assert.Len(t, f.parts, 2)
	receipt, err = s.Publish(t.Context(), "large", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
}

func TestLostCompletionResponse(t *testing.T) {
	f, s := newFixture(t)
	f.lossComplete = true
	data := bytes.Repeat([]byte("m"), (5<<20)+3)
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "large", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, sum, receipt.SHA256)
	assert.Equal(t, "readback-sha256", receipt.Verification)
	assert.Positive(t, f.heads)
}

func TestMultipartAbortAndRetry(t *testing.T) {
	f, s := newFixture(t)
	f.failPart = true
	data := bytes.Repeat([]byte("m"), (5<<20)+3)
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "large", bytes.NewReader(data), int64(len(data)), sum)
	require.Error(t, err)
	assert.Zero(t, receipt)
	assert.Equal(t, 1, f.aborts)
	assert.Empty(t, f.objects)
	f.failPart = false
	receipt, err = s.Publish(t.Context(), "large", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, sum, receipt.SHA256)
}

func TestCompositeChecksumReadback(t *testing.T) {
	f, s := newFixture(t)
	data := []byte("same bytes")
	sum := sha256.Sum256(data)
	f.objects["namespace/key"] = data
	f.sha = shaValue([]byte("not full object")) + "-2"
	f.scope = "COMPOSITE"
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
	require.Len(t, receipt.Object.Checksums, 1)
	assert.Equal(t, "composite", receipt.Object.Checksums[0].Scope)
	s.readback = false
	receipt, err = s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
}

func TestRoutineReadbackOptIn(t *testing.T) {
	f, s := newFixture(t)
	f.omitChecksums = true
	s.readback = false
	data := []byte("bytes")
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	// A retry reconciles the recorded intent with SHA-256 even though routine
	// checksum fallback was disabled. CRC or metadata never establishes identity.
	receipt, err = s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
	s.readback = true
	receipt, err = s.Publish(t.Context(), "other", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
}

func TestVerificationFailures(t *testing.T) {
	for _, scenario := range []string{"length", "sha256", "inspection", "readback forbidden", "readback truncated", "revision unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			f, s := newFixture(t)
			data := []byte("bytes")
			sum := sha256.Sum256(data)
			want := objectstore.ErrOutcomeUnknown
			switch scenario {
			case "length":
				f.wrongLength = true
				want = objectstore.ErrChecksumMismatch
			case "sha256":
				f.wrongSHA = true
				want = objectstore.ErrChecksumMismatch
			case "inspection":
				f.denyHead = true
			case "readback forbidden":
				f.omitChecksums = true
				f.denyGet = true
			case "readback truncated":
				f.omitChecksums = true
				f.truncate = true
			case "revision unavailable":
				f.omitChecksums = true
				f.omitRevision = true
			}
			receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
			require.ErrorIs(t, err, want)
			assert.Zero(t, receipt)
			assert.Equal(t, data, f.objects["namespace/key"])
		})
	}
}

func TestMissingAfterAmbiguousUpload(t *testing.T) {
	f, s := newFixture(t)
	f.lossPut = true
	f.dropOnConflict = true
	data := []byte("bytes")
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	require.ErrorIs(t, err, objectstore.ErrNotFound)
}

func TestCanceledAfterDispatch(t *testing.T) {
	f, s := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.afterPut = cancel
	f.lossPut = true
	data := []byte("bytes")
	receipt, err := s.Publish(ctx, "key", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, receipt)
	assert.Equal(t, data, f.objects["namespace/key"])
	assert.Zero(t, f.heads)
}

func TestMultipartCompositeReadback(t *testing.T) {
	f, s := newFixture(t)
	f.scope = "COMPOSITE"
	data := bytes.Repeat([]byte("m"), (5<<20)+1)
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "large", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
}

func TestLostPutThenDeniedRetry(t *testing.T) {
	f, s := newFixture(t)
	f.lossPut = true
	f.retryDenied = true
	data := []byte("stored before the retry was denied")
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, sum, receipt.SHA256)
	assert.Positive(t, f.heads)
}

func TestMalformedFullChecksumReadback(t *testing.T) {
	f, s := newFixture(t)
	data := []byte("same bytes")
	sum := sha256.Sum256(data)
	f.objects["namespace/key"] = data
	f.sha = "not-a-full-sha256"
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "readback-sha256", receipt.Verification)
}

func TestLostCompletionBody(t *testing.T) {
	f, s := newFixture(t)
	f.lossCompleteHeaders = true
	data := bytes.Repeat([]byte("m"), (5<<20)+3)
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "large", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, sum, receipt.SHA256)
	assert.Equal(t, "readback-sha256", receipt.Verification)
	assert.Positive(t, f.heads)
}

func TestCanceledDuringTLSBeforeDispatch(t *testing.T) {
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	type helloResult struct {
		header [6]byte
		err    error
	}
	observed := make(chan helloResult, 1)
	go func() {
		var result helloResult
		conn, err := listener.Accept()
		if err == nil {
			defer func() { _ = conn.Close() }()
			err = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err == nil {
				_, err = io.ReadFull(conn, result.header[:])
			}
		}
		result.err = err
		// The server sends zero bytes: TLS cannot finish, so HTTP cannot start.
		cancel()
		observed <- result
	}()
	store, err := New(ctx, Config{Bucket: "bucket", Region: "us-east-1", Endpoint: "https://" + listener.Addr().String(), ForcePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("test", "test-secret", "")})
	require.NoError(t, err)
	data := []byte("canceled before HTTP dispatch")
	receipt, err := store.Publish(ctx, "key", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	result := <-observed
	require.NoError(t, result.err)
	assert.Equal(t, byte(22), result.header[0], "TLS handshake record")
	assert.Equal(t, byte(1), result.header[5], "ClientHello handshake")
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
}
