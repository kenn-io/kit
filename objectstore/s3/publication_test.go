package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
)

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
