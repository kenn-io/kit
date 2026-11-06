package embedserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
)

func TestFaceAlignment(t *testing.T) {
	// source = (200-2*y, 3+2*x): a 90-degree rotation, scale, and translation.
	var source [5]point
	for i, p := range canonicalPoints {
		source[i] = point{200 - 2*p.y, 3 + 2*p.x}
	}
	transform, err := similarityTransform(source)
	require.NoError(t, err)
	for i, want := range []float64{0, 0.5, -1.5, -0.5, 0, 100} {
		assert.InDelta(t, want, transform[i], 1e-9)
	}
	_, err = similarityTransform([5]point{})
	require.Error(t, err)

	// An asymmetric image and exact quarter-turn catch mirrored/shifted sampling.
	src := image.NewRGBA(image.Rect(0, 0, 240, 240))
	for y := range 240 {
		for x := range 240 {
			src.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 17, 255})
		}
	}
	aligned, err := alignedFace(src, source)
	require.NoError(t, err)
	assert.InDelta(t, 159, aligned.RGBAAt(10, 20).R, 1)
	assert.InDelta(t, 23.5, aligned.RGBAAt(10, 20).G, 1)
	assert.InDelta(t, 79, aligned.RGBAAt(40, 60).R, 1)
	assert.InDelta(t, 83.5, aligned.RGBAAt(40, 60).G, 1)
}

func TestFaceBGRPlanes(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.SetRGBA(0, 0, color.RGBA{255, 128, 0, 255})
	src.SetRGBA(1, 0, color.RGBA{0, 255, 128, 255})
	assert.Equal(t, []float32{0, 128, 128, 255, 255, 0}, bgrPlanes(src, false))
	assert.Equal(t, []float32{-0.99609375, 0.00390625, 0.00390625, 0.99609375, 0.99609375, -0.99609375}, bgrPlanes(src, true))
}

func TestYuNetSelection(t *testing.T) {
	outputs := make([][]float32, 12)
	for level, stride := range []int{8, 16, 32} {
		for kind, channels := range []int{1, 1, 4, 10} {
			outputs[kind*3+level] = make([]float32, (640/stride)*(640/stride)*channels)
		}
	}
	// The highest score covers less of the requested crop. The second detection
	// exactly covers it and maps landmarks from the scaled crop to preview pixels.
	for index, score := range []float32{1, 0.95} {
		outputs[0][index], outputs[3][index] = score, score
		width := float32(8)
		if index == 1 {
			width = 80
		}
		outputs[6][index*4], outputs[6][index*4+1] = 5-float32(index), 5
		outputs[6][index*4+2], outputs[6][index*4+3] = float32(math.Log(float64(width)/8)), float32(math.Log(float64(width)/8))
		for i, v := range []float32{1, 2, 3, 2, 2, 3, 1, 4, 3, 4} {
			outputs[9][index*10+i] = v
		}
	}
	points, found := selectLandmarks(outputs, 80, 80, image.Rect(100, 200, 260, 360))
	require.True(t, found)
	assert.Equal(t, [5]point{{132, 232}, {164, 232}, {148, 248}, {132, 264}, {164, 264}}, points)
	outputs[0][0], outputs[0][1] = 0.2, 0.2
	_, found = selectLandmarks(outputs, 80, 80, image.Rect(100, 200, 260, 360))
	assert.False(t, found)
}

type faceEngine struct {
	*fakeEngine
	runFace func(context.Context, image.Image, embedclient.FaceBox) ([]float32, error)
}

func (e *faceEngine) embedFace(ctx context.Context, preview image.Image, box embedclient.FaceBox) ([]float32, error) {
	return e.runFace(ctx, preview, box)
}

func TestFaceHTTP(t *testing.T) {
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 8, 4))))
	oversized := bytes.Clone(encoded.Bytes())
	binary.BigEndian.PutUint32(oversized[16:20], 6401)
	binary.BigEndian.PutUint32(oversized[20:24], 6400)
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	tooMany := make([]embedclient.FaceBox, 129)
	for i := range tooMany {
		tooMany[i] = embedclient.FaceBox{Width: 1, Height: 1}
	}
	for _, tc := range []struct {
		name, message string
		boxes         []embedclient.FaceBox
		image         []byte
		fail          bool
		status, calls int
	}{
		{"ordered results", "", []embedclient.FaceBox{{X: 0, Width: 0.5, Height: 1}, {X: 0.5, Width: 0.5, Height: 1}}, encoded.Bytes(), false, 200, 2},
		{"bad box", "", []embedclient.FaceBox{{Width: 2, Height: 1}}, encoded.Bytes(), false, 400, 0},
		{"empty box list", "", nil, encoded.Bytes(), false, 400, 0},
		{"pixel limit", "too many pixels", []embedclient.FaceBox{{Width: 1, Height: 1}}, oversized, false, 400, 0},
		{"box count limit", "box count", tooMany, encoded.Bytes(), false, 400, 0},
		{"invalid image", "", []embedclient.FaceBox{{Width: 1, Height: 1}}, []byte("broken"), false, 400, 0},
		{"truncated image", "invalid preview", []embedclient.FaceBox{{Width: 1, Height: 1}}, encoded.Bytes()[:33], false, 400, 0},
		{"inference failure", "", []embedclient.FaceBox{{Width: 1, Height: 1}}, encoded.Bytes(), true, 500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			e := &faceEngine{fakeEngine: new(fakeEngine), runFace: func(_ context.Context, preview image.Image, box embedclient.FaceBox) ([]float32, error) {
				calls++
				assert.Equal(t, image.Rect(0, 0, 8, 4), preview.Bounds())
				assert.Equal(t, tc.boxes[calls-1], box)
				if tc.fail {
					return nil, errors.New("native failure")
				}
				if box.X > 0 {
					return nil, nil
				}
				vector := make([]float32, 512)
				vector[0] = 1
				return vector, nil
			}}
			h := &handler{engine: e, gate: make(chan struct{}, 1), previews: make(chan struct{}, 1), log: io.Discard}
			body, err := json.Marshal(embedclient.FaceRequest{Image: tc.image, Boxes: tc.boxes})
			require.NoError(t, err)
			w := httptest.NewRecorder()
			h.faces(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/faces", bytes.NewReader(body)))
			assert.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.calls, calls)
			if tc.message != "" {
				assert.Contains(t, w.Body.String(), tc.message)
			}
			if tc.status == 200 {
				var result embedclient.FaceResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
				require.Len(t, result.Data, 2)
				assert.Equal(t, embedclient.FaceRevision, result.Revision)
				assert.Equal(t, embedclient.FaceRecipe, result.Recipe)
				assert.Equal(t, 0, result.Data[0].Index)
				assert.InDelta(t, 1, result.Data[0].Embedding[0], 1e-6)
				assert.Equal(t, 1, result.Data[1].Index)
				assert.Nil(t, result.Data[1].Embedding)
				assert.Contains(t, w.Body.String(), `"embedding":null`)
			}
		})
	}
}

func TestFaceCropWithoutPixels(t *testing.T) {
	vector, err := new(faceModel).embed(t.Context(), image.NewRGBA(image.Rect(0, 0, 8, 4)), embedclient.FaceBox{X: 0.5, Y: 0.5, Width: 1e-20, Height: 1e-20})
	require.NoError(t, err)
	assert.Nil(t, vector)
}

func TestFaceDecodeWaitsForPreviewAdmission(t *testing.T) {
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 1))))
	// A PNG header passes DecodeConfig, but the missing pixel data fails Decode.
	truncated := encoded.Bytes()[:33]
	_, _, err := image.DecodeConfig(bytes.NewReader(truncated))
	require.NoError(t, err)
	_, _, err = image.Decode(bytes.NewReader(truncated))
	require.Error(t, err)
	body, err := json.Marshal(embedclient.FaceRequest{Image: truncated, Boxes: []embedclient.FaceBox{{Width: 1, Height: 1}}})
	require.NoError(t, err)
	h := &handler{gate: make(chan struct{}, 1), previews: make(chan struct{}, 1)}
	h.previews <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := httptest.NewRecorder()
	h.faces(w, httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/faces", bytes.NewReader(body)))
	// Cancellation while another preview holds admission returns before Decode
	// can reject this truncated image or allocate the decoded preview.
	require.Empty(t, w.Body.String())
	<-h.previews
}

func TestFaceResponseReleasesInferenceGate(t *testing.T) {
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 1))))
	for _, tc := range []struct {
		name      string
		preview   []byte
		inference error
	}{
		{name: "success", preview: encoded.Bytes()},
		{name: "decode error", preview: encoded.Bytes()[:33]},
		{name: "inference error", preview: encoded.Bytes(), inference: errors.New("native failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &faceEngine{fakeEngine: new(fakeEngine), runFace: func(context.Context, image.Image, embedclient.FaceBox) ([]float32, error) { return nil, tc.inference }}
			h := &handler{engine: e, gate: make(chan struct{}, 1), previews: make(chan struct{}, 1), log: io.Discard}
			body, err := json.Marshal(embedclient.FaceRequest{Image: tc.preview, Boxes: []embedclient.FaceBox{{Width: 1, Height: 1}}})
			require.NoError(t, err)
			writing, release := make(chan struct{}), make(chan struct{})
			writer := &blockedResponseWriter{ResponseRecorder: httptest.NewRecorder(), entered: writing, release: release}
			done := make(chan struct{})
			defer func() {
				close(release)
				<-done
			}()
			go func() {
				defer close(done)
				h.faces(writer, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/faces", bytes.NewReader(body)))
			}()
			select {
			case <-writing:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "face response did not reach blocked Write")
			}
			assert.Empty(t, h.previews)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			text := httptest.NewRecorder()
			h.ServeHTTP(text, httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`)))
			assert.Equal(t, http.StatusOK, text.Code)
			assert.NotEmpty(t, text.Body.String(), "blocked face response retained inference gate")
			e.mu.Lock()
			defer e.mu.Unlock()
			assert.Equal(t, []string{"x"}, e.inputs)
		})
	}
}

func TestFaceRunsTextBetweenItems(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var order []string
		release := make(chan struct{})
		e := &faceEngine{fakeEngine: &fakeEngine{run: func(_ context.Context, text string) ([]float32, int, error) {
			order = append(order, text)
			vector := make([]float32, 768)
			vector[0] = 1
			return vector, 1, nil
		}}, runFace: func(_ context.Context, _ image.Image, box embedclient.FaceBox) ([]float32, error) {
			if box.X == 0 {
				order = append(order, "face 0")
				<-release
			} else {
				order = append(order, "face 1")
			}
			return nil, nil
		}}
		h := &handler{engine: e, gate: make(chan struct{}, 1), previews: make(chan struct{}, 1), log: io.Discard}
		var preview bytes.Buffer
		require.NoError(t, png.Encode(&preview, image.NewRGBA(image.Rect(0, 0, 2, 1))))
		body, err := json.Marshal(embedclient.FaceRequest{Image: preview.Bytes(), Boxes: []embedclient.FaceBox{{Width: .5, Height: 1}, {X: .5, Width: .5, Height: 1}}})
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Go(func() {
			w := httptest.NewRecorder()
			h.faces(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/faces", bytes.NewReader(body)))
			assert.Equal(t, http.StatusOK, w.Code)
		})
		synctest.Wait()
		wg.Go(func() {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"query"}`)))
			assert.Equal(t, http.StatusOK, w.Code)
		})
		synctest.Wait()
		close(release)
		wg.Wait()
		assert.Equal(t, []string{"face 0", "query", "face 1"}, order)
	})
}
