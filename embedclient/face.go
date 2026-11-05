package embedclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"strings"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/internal/embedassets"
)

const (
	FaceModel        = embedassets.FaceModel
	FaceRevision     = embedassets.FaceRevision
	FaceRecipe       = embedassets.FaceRecipe
	MaxFaceBoxes     = 128
	MaxPreviewPixels = 40_000_000
)

// FaceBox uses normalized coordinates in the already oriented preview.
type FaceBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Validate checks that a box is finite, nonempty, and contained in the preview.
func (b FaceBox) Validate() error {
	for _, v := range []float64{b.X, b.Y, b.Width, b.Height} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("face box must be finite")
		}
	}
	if b.X < -1e-6 || b.Y < -1e-6 || b.Width <= 0 || b.Height <= 0 || b.X+b.Width > 1+1e-6 || b.Y+b.Height > 1+1e-6 {
		return errors.New("face box must fit within the preview")
	}
	return nil
}

// FaceRequest carries one JPEG or PNG preview and its ordered face boxes.
type FaceRequest struct {
	Image []byte    `json:"image"`
	Boxes []FaceBox `json:"boxes"`
}

// FaceResult preserves the requested index. A null embedding means no face matched.
type FaceResult struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// FaceResponse identifies the model and alignment used to produce its vectors.
type FaceResponse struct {
	Model    string       `json:"model"`
	Revision string       `json:"revision"`
	Recipe   string       `json:"recipe"`
	Data     []FaceResult `json:"data"`
}

// EmbedFaces returns one unit vector or nil per box, in request order.
func (c *Client) EmbedFaces(ctx context.Context, preview []byte, boxes []FaceBox) (FaceResponse, error) {
	var result FaceResponse
	if len(boxes) == 0 {
		return result, nil
	}
	if len(boxes) > MaxFaceBoxes {
		return result, errors.New("too many face boxes")
	}
	for _, box := range boxes {
		if err := box.Validate(); err != nil {
			return result, err
		}
	}
	body, err := json.Marshal(FaceRequest{Image: preview, Boxes: boxes})
	if err != nil {
		return result, err
	}
	if len(body) > 64<<20 {
		return result, errors.New("face request exceeds 64 MiB")
	}
	endpoint := strings.TrimSuffix(c.endpoint, "/embeddings") + "/faces"
	payload, err := retryCall(ctx, c.retry, func() ([]byte, error) { return c.postJSON(ctx, endpoint, body) })
	if err != nil {
		return result, err
	}
	// Raw values distinguish an unmatched face from a missing embedding.
	var wire struct {
		Model    string `json:"model"`
		Revision string `json:"revision"`
		Recipe   string `json:"recipe"`
		Data     []struct {
			Index     *int           `json:"index"`
			Embedding jsontext.Value `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return result, errors.New("face response is invalid")
	}
	if wire.Model != FaceModel || wire.Revision != FaceRevision || wire.Recipe != FaceRecipe {
		return result, errors.New("face response model or alignment differs")
	}
	if len(wire.Data) != len(boxes) {
		return result, errors.New("face response result count differs")
	}
	result = FaceResponse{Model: wire.Model, Revision: wire.Revision, Recipe: wire.Recipe, Data: make([]FaceResult, len(boxes))}
	seen := make([]bool, len(boxes))
	for _, item := range wire.Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= len(boxes) || seen[*item.Index] {
			return FaceResponse{}, errors.New("face response index is invalid")
		}
		i := *item.Index
		seen[i] = true
		result.Data[i].Index = i
		if len(item.Embedding) == 0 {
			return FaceResponse{}, errors.New("face response embedding is missing")
		}
		if string(item.Embedding) == "null" {
			continue
		}
		var raw wireEmbedding
		if err := json.Unmarshal(item.Embedding, &raw); err != nil {
			return FaceResponse{}, &VectorError{Index: i, Err: err}
		}
		values, err := decodeVector(raw, 512, embedconfig.NormalizationNone)
		if err != nil {
			return FaceResponse{}, &VectorError{Index: i, Err: err}
		}
		var norm float64
		for _, v := range values {
			norm += float64(v) * float64(v)
		}
		if math.Abs(norm-1) > 1e-4 {
			return FaceResponse{}, &VectorError{Index: i, Err: errors.New("face vector is not normalized")}
		}
		result.Data[i].Embedding = values
	}
	return result, nil
}
