package embedclient

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"

	jsonv2 "encoding/json/v2"

	"go.kenn.io/kit/embedconfig"
)

type wireRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	InputType      string   `json:"input_type,omitempty"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type wireResponse struct {
	Data []wireItem `json:"data"`
}

type wireItem struct {
	Embedding jsontext.Value `json:"embedding"`
	Index     *int           `json:"index"`
}

func (c *Client) decode(payload []byte, count int) ([][]float32, error) {
	var decoded wireResponse
	if err := jsonv2.Unmarshal(payload, &decoded); err != nil {
		return nil, errors.New("embed response is invalid")
	}
	raw, err := orderItems(decoded.Data, count)
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(raw))
	for i, item := range raw {
		vector, err := decodeVector(item, c.model.Dimensions, c.model.Normalization)
		if err != nil {
			return nil, fmt.Errorf("embed vector %d: %w", i, err)
		}
		out[i] = vector
	}
	return out, nil
}

func orderItems(items []wireItem, count int) ([]jsontext.Value, error) {
	if len(items) != count {
		return nil, fmt.Errorf("embed response contained %d vectors for %d inputs", len(items), count)
	}
	missing := 0
	for _, item := range items {
		if item.Index == nil {
			missing++
		}
	}
	out := make([]jsontext.Value, count)
	if missing == count {
		for i, item := range items {
			out[i] = item.Embedding
		}
		return out, nil
	}
	if missing != 0 {
		return nil, errors.New("embed response mixes indexed and unindexed vectors")
	}
	seen := make([]bool, count)
	for pos, item := range items {
		index := *item.Index
		if index < 0 || index >= count {
			return nil, fmt.Errorf("embed response item %d has index %d", pos, index)
		}
		if seen[index] {
			return nil, fmt.Errorf("embed response contains duplicate index %d", index)
		}
		seen[index] = true
		out[index] = item.Embedding
	}
	return out, nil
}

func decodeVector(raw jsontext.Value, dims int, normalization embedconfig.Normalization) ([]float32, error) {
	var elements []*float64
	if err := jsonv2.Unmarshal(raw, &elements); err != nil || len(raw) == 0 {
		return nil, errors.New("vector is invalid")
	}
	if len(elements) != dims {
		return nil, fmt.Errorf("has %d dimensions, expected %d", len(elements), dims)
	}
	out := make([]float32, len(elements))
	var sumSquares float64
	for i, element := range elements {
		if element == nil {
			return nil, fmt.Errorf("component %d is null", i)
		}
		value := float32(*element)
		if math.IsNaN(*element) || math.IsInf(*element, 0) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("component %d is not finite", i)
		}
		out[i] = value
		sumSquares += float64(value) * float64(value)
	}
	if sumSquares == 0 {
		return nil, errors.New("has zero norm")
	}
	if normalization != embedconfig.NormalizationL2 {
		return out, nil
	}
	norm := math.Sqrt(sumSquares)
	for i, value := range out {
		out[i] = float32(float64(value) / norm)
	}
	return out, nil
}
