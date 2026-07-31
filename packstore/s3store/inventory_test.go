package s3store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestProbeDisarmsCleanupAfterExplicitDelete(t *testing.T) {
	state, backend := newProbeHTTPBackend(t, false)

	report, err := backend.Probe(context.Background())

	require.NoError(t, err)
	assert.Equal(t, CapabilityReport{
		StrongReadAfterWrite: true,
		RepeatableReads:      true,
		RangeReads:           true,
		Listing:              true,
		MultipartPublication: true,
		Delete:               true,
	}, report)
	assert.Equal(t, 2, state.ownershipReads)
	assert.Equal(t, 1, state.deletes)
}

func TestProbeCleanupUsesFreshDeadline(t *testing.T) {
	state, backend := newProbeHTTPBackend(t, true)

	_, err := backend.Probe(context.Background())

	require.Error(t, err)
	assert.True(t, state.cleanupOwnershipHadDeadline)
	assert.True(t, state.cleanupDeleteHadDeadline)
	assert.Equal(t, 1, state.deletes)
}

type probeHTTPState struct {
	t                           *testing.T
	marker                      []byte
	payload                     []byte
	probeKey                    string
	failFirstProbeRead          bool
	probeReads                  int
	ownershipReads              int
	deletes                     int
	cleanupOwnershipHadDeadline bool
	cleanupDeleteHadDeadline    bool
}

func newProbeHTTPBackend(t *testing.T, failFirstProbeRead bool) (*probeHTTPState, *Backend) {
	t.Helper()
	owner := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  "archive",
		Epoch:  "epoch-1",
	}
	marker, err := packstore.MarshalOwnership(owner)
	require.NoError(t, err)
	state := &probeHTTPState{
		t:                  t,
		marker:             marker,
		payload:            bytes.Repeat([]byte("kit-s3-probe\n"), 512<<10),
		failFirstProbeRead: failFirstProbeRead,
	}
	backend := newHTTPBackend(packstore.DefaultLimits(), state.roundTrip)
	backend.part = int64(len(state.payload))
	backend.setOwnership(owner, `"owner-etag"`)
	return state, backend
}

func (s *probeHTTPState) roundTrip(request *http.Request) (*http.Response, error) {
	s.t.Helper()
	query := request.URL.Query()
	key := strings.TrimPrefix(request.URL.Path, "/test-bucket/")
	switch {
	case request.Method == http.MethodGet && key == ".kit-store.json":
		s.ownershipReads++
		if s.ownershipReads == 2 && s.failFirstProbeRead {
			_, s.cleanupOwnershipHadDeadline = request.Context().Deadline()
		}
		response := bytesResponse(request, s.marker)
		response.Header.Set("ETag", `"owner-etag"`)
		return response, nil
	case request.Method == http.MethodPost && query.Has("uploads"):
		s.probeKey = key
		return xmlResponse(request, http.StatusOK,
			`<InitiateMultipartUploadResult>`+
				`<Bucket>test-bucket</Bucket><Key>`+key+`</Key>`+
				`<UploadId>upload-1</UploadId>`+
				`</InitiateMultipartUploadResult>`), nil
	case request.Method == http.MethodPut && query.Get("uploadId") == "upload-1":
		response := xmlResponse(request, http.StatusOK, "")
		response.Header.Set("ETag", `"part-etag"`)
		return response, nil
	case request.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
		return xmlResponse(request, http.StatusOK,
			`<CompleteMultipartUploadResult>`+
				`<Bucket>test-bucket</Bucket><Key>`+s.probeKey+`</Key>`+
				`<ETag>"complete-etag"</ETag>`+
				`</CompleteMultipartUploadResult>`), nil
	case request.Method == http.MethodGet && query.Get("list-type") == "2":
		return xmlResponse(request, http.StatusOK,
			`<ListBucketResult>`+
				`<Name>test-bucket</Name><Prefix>`+s.probeKey+`</Prefix>`+
				`<KeyCount>1</KeyCount><MaxKeys>2</MaxKeys><IsTruncated>false</IsTruncated>`+
				`<Contents><Key>`+s.probeKey+`</Key><Size>`+
				strconv.Itoa(len(s.payload))+`</Size></Contents>`+
				`</ListBucketResult>`), nil
	case request.Method == http.MethodGet && key == s.probeKey:
		s.probeReads++
		if s.failFirstProbeRead {
			return nil, fmt.Errorf("probe read failed")
		}
		if request.Header.Get("Range") == "bytes=5-20" {
			return bytesResponse(request, s.payload[5:21]), nil
		}
		return bytesResponse(request, s.payload), nil
	case request.Method == http.MethodDelete && key == s.probeKey:
		s.deletes++
		if s.failFirstProbeRead {
			_, s.cleanupDeleteHadDeadline = request.Context().Deadline()
		}
		return xmlResponse(request, http.StatusNoContent, ""), nil
	default:
		return xmlResponse(request, http.StatusInternalServerError,
			`<Error><Code>UnexpectedRequest</Code></Error>`), nil
	}
}

func bytesResponse(request *http.Request, body []byte) *http.Response {
	header := make(http.Header)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
}
