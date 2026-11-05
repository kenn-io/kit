package embedclient

import (
	"bytes"
	"encoding/json/v2"
	"image"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
)

func TestEmbedFaces(t *testing.T) {
	meta := `{"model":"` + FaceModel + `","revision":"` + FaceRevision + `","recipe":"` + FaceRecipe + `","data":`
	for _, tc := range []struct {
		name, body, problem string
	}{
		{"short response", meta + `[]}`, "face response result count differs"},
		{"duplicate index", meta + `[{"index":0,"embedding":null},{"index":0,"embedding":null}]}`, "face response index is invalid"},
		{"bad width", meta + `[{"index":0,"embedding":[1]},{"index":1,"embedding":null}]}`, "has 1 dimensions, expected 512"},
		{"non-unit vector", meta + `[{"index":0,"embedding":[2` + strings.Repeat(",0", 511) + `]},{"index":1,"embedding":null}]}`, "face vector is not normalized"},
		{"missing embedding", meta + `[{"index":0},{"index":1,"embedding":null}]}`, "face response embedding is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boxes := []FaceBox{{Width: 0.5, Height: 1}, {X: 0.5, Width: 0.5, Height: 1}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			defer server.Close()
			client, err := faceTestClient(server.URL, Retry{})
			require.NoError(t, err)
			_, err = client.EmbedFaces(t.Context(), []byte("preview"), boxes)
			require.ErrorContains(t, err, tc.problem)
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
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		assert.NoError(t, json.MarshalWrite(w, FaceResponse{Model: FaceModel, Revision: FaceRevision, Recipe: FaceRecipe, Data: []FaceResult{{Index: 0}}}, json.FormatNilSliceAsNull(true)))
	}))
	defer server.Close()
	client, err := faceTestClient(server.URL, Retry{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxRetryAfter: time.Millisecond})
	require.NoError(t, err)
	result, err := client.EmbedFaces(t.Context(), []byte("preview"), []FaceBox{{Width: 1, Height: 1}})
	require.NoError(t, err)
	require.Len(t, result.Data, 1)
	assert.Nil(t, result.Data[0].Embedding)
	assert.Equal(t, int32(2), calls.Load())
}

func TestEmbedFacesBatches(t *testing.T) {
	var preview bytes.Buffer
	require.NoError(t, png.Encode(&preview, image.NewRGBA(image.Rect(0, 0, 2, 1))))
	boxes := make([]FaceBox, 128)
	for i := range boxes {
		boxes[i] = FaceBox{X: float64(i) / 256, Width: 1.0 / 256, Height: 1}
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/faces", r.URL.Path)
		var req FaceRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !assert.LessOrEqual(t, len(req.Boxes), faceBatchItems, "face requests must stay within their inference budget") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, preview.Bytes(), req.Image)
		start := int(calls.Add(1)-1) * faceBatchItems
		if !assert.LessOrEqual(t, start+len(req.Boxes), len(boxes)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, boxes[start:start+len(req.Boxes)], req.Boxes)
		result := FaceResponse{Model: FaceModel, Revision: FaceRevision, Recipe: FaceRecipe}
		for i, box := range slices.Backward(req.Boxes) {
			global := int(math.Round(box.X * 256))
			item := FaceResult{Index: i}
			if global%3 != 0 {
				item.Embedding = make([]float32, 512)
				item.Embedding[global] = 1
			}
			result.Data = append(result.Data, item)
		}
		assert.NoError(t, json.MarshalWrite(w, result, json.FormatNilSliceAsNull(true)))
	}))
	defer server.Close()
	client, err := faceTestClient(server.URL, Retry{})
	require.NoError(t, err)
	result, err := client.EmbedFaces(t.Context(), preview.Bytes(), boxes)
	require.NoError(t, err)
	require.Len(t, result.Data, 128)
	assert.Equal(t, int32((len(boxes)+faceBatchItems-1)/faceBatchItems), calls.Load())
	for i, item := range result.Data {
		assert.Equal(t, i, item.Index)
		if i%3 == 0 {
			assert.Nil(t, item.Embedding)
			continue
		}
		require.Len(t, item.Embedding, 512)
		assert.InDelta(t, 1, item.Embedding[i], 1e-7)
	}
}

func TestEmbedFacesBatchVectorError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req FaceRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		result := FaceResponse{Model: FaceModel, Revision: FaceRevision, Recipe: FaceRecipe, Data: make([]FaceResult, len(req.Boxes))}
		for i := range result.Data {
			result.Data[i].Index = i
		}
		if calls.Add(1) == 2 {
			result.Data[0].Embedding = []float32{1}
		}
		assert.NoError(t, json.MarshalWrite(w, result, json.FormatNilSliceAsNull(true)))
	}))
	defer server.Close()
	client, err := faceTestClient(server.URL, Retry{})
	require.NoError(t, err)
	boxes := make([]FaceBox, 17)
	for i := range boxes {
		boxes[i] = FaceBox{Width: 1, Height: 1}
	}
	result, err := client.EmbedFaces(t.Context(), []byte("preview is validated by server"), boxes)
	var vector *VectorError
	require.ErrorAs(t, err, &vector)
	assert.Equal(t, 8, vector.Index)
	assert.Empty(t, result.Data, "a failed batch must not return partial vectors")
	assert.Equal(t, int32(2), calls.Load(), "stop before the third batch")
}

func faceTestClient(endpoint string, retry Retry) (*Client, error) {
	return New(Options{Model: embedconfig.Model{Name: FaceModel, Dimensions: 512, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: endpoint + "/v1"}, Retry: retry})
}
