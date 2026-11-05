package embedclient

import (
	"bytes"
	"encoding/json/v2"
	"image"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
)

func TestEmbedFaces(t *testing.T) {
	var preview bytes.Buffer
	require.NoError(t, png.Encode(&preview, image.NewRGBA(image.Rect(0, 0, 2, 1))))
	unit := "[1" + strings.Repeat(",0", 511) + "]"
	meta := `{"model":"` + FaceModel + `","revision":"` + FaceRevision + `","recipe":"` + FaceRecipe + `","data":`
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"ordered mixed results", meta + `[{"index":1,"embedding":null},{"index":0,"embedding":` + unit + `}]}`, 200, true},
		{"short response", meta + `[]}`, 200, false},
		{"duplicate index", meta + `[{"index":0,"embedding":null},{"index":0,"embedding":null}]}`, 200, false},
		{"bad width", meta + `[{"index":0,"embedding":[1]},{"index":1,"embedding":null}]}`, 200, false},
		{"zero norm", meta + `[{"index":0,"embedding":[0` + strings.Repeat(",0", 511) + `]},{"index":1,"embedding":null}]}`, 200, false},
		{"non-unit vector", meta + `[{"index":0,"embedding":[2` + strings.Repeat(",0", 511) + `]},{"index":1,"embedding":null}]}`, 200, false},
		{"null component", meta + `[{"index":0,"embedding":[null` + strings.Repeat(",0", 511) + `]},{"index":1,"embedding":null}]}`, 200, false},
		{"missing embedding", meta + `[{"index":0},{"index":1,"embedding":null}]}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boxes := []FaceBox{{Width: 0.5, Height: 1}, {X: 0.5, Width: 0.5, Height: 1}}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/v1/faces", r.URL.Path)
				var req FaceRequest
				assert.NoError(t, json.UnmarshalRead(r.Body, &req))
				assert.Equal(t, preview.Bytes(), req.Image)
				assert.Equal(t, boxes, req.Boxes)
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			defer server.Close()
			client, err := New(Options{Model: embedconfig.Model{Name: "nomic-embed-text", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/v1"}})
			require.NoError(t, err)
			result, err := client.EmbedFaces(t.Context(), preview.Bytes(), boxes)
			assert.Equal(t, 1, calls)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Data, 2)
			assert.InDelta(t, 1, result.Data[0].Embedding[0], 1e-6)
			assert.Nil(t, result.Data[1].Embedding)
			assert.Equal(t, 1, result.Data[1].Index)
		})
	}
}

func TestFaceBoxValidation(t *testing.T) {
	for _, box := range []FaceBox{{Width: 0, Height: 1}, {Width: 1, Height: 1, Y: -0.1}, {X: 0.5, Width: 0.6, Height: 1}, {X: math.NaN(), Width: 1, Height: 1}, {Width: 1, Height: math.Inf(1)}, {X: -2e-6, Width: 1, Height: 1}, {X: 0.5, Width: 0.500002, Height: 1}} {
		require.Error(t, box.Validate())
	}
	for _, box := range []FaceBox{
		{Width: 1, Height: 1},
		{X: 0.5, Width: 0.5000005, Height: 1},
		{X: -0.0000005, Y: -0.0000005, Width: 1, Height: 1.000001},
	} {
		assert.NoError(t, box.Validate())
	}
}

func TestEmbedFacesRetries(t *testing.T) {
	var preview bytes.Buffer
	require.NoError(t, png.Encode(&preview, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/faces", r.URL.Path)
		var request FaceRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, preview.Bytes(), request.Image)
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, FaceResponse{Model: FaceModel, Revision: FaceRevision, Recipe: FaceRecipe, Data: []FaceResult{{Index: 0}}}, json.FormatNilSliceAsNull(true)))
	}))
	defer server.Close()
	client, err := New(Options{Model: embedconfig.Model{Name: "nomic-embed-text", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: server.URL + "/v1"}, Retry: Retry{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxRetryAfter: time.Millisecond}})
	require.NoError(t, err)
	result, err := client.EmbedFaces(t.Context(), preview.Bytes(), []FaceBox{{Width: 1, Height: 1}})
	require.NoError(t, err)
	require.Len(t, result.Data, 1)
	assert.Nil(t, result.Data[0].Embedding)
	assert.Equal(t, int32(2), calls.Load())
}
