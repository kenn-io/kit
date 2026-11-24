package embedclient

import (
	"errors"
	"fmt"

	json "encoding/json/v2"
)

type vertexInstance struct {
	Content  string `json:"content"`
	TaskType string `json:"task_type"`
}

func (c *Client) vertexRequestBody(texts []string) ([]byte, error) {
	instances := make([]vertexInstance, len(texts))
	for i, text := range texts {
		instances[i] = vertexInstance{Content: text, TaskType: "RETRIEVAL_DOCUMENT"}
	}
	var request struct {
		Instances  []vertexInstance `json:"instances"`
		Parameters struct {
			OutputDimensionality int `json:"outputDimensionality,omitzero"`
		} `json:"parameters"`
	}
	request.Instances = instances
	if c.model.RequestDimensions {
		request.Parameters.OutputDimensionality = c.model.Dimensions
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("embed request encoding failed: %w", err)
	}
	return body, nil
}

func (c *Client) classifyVertex(payload []byte, count int) ([][]float32, []error, error) {
	var response struct {
		Predictions []struct {
			Embeddings struct {
				Values wireEmbedding `json:"values"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, nil, errors.New("embed response is invalid")
	}
	if len(response.Predictions) != count {
		return nil, nil, fmt.Errorf("embed response contained %d vectors for %d inputs", len(response.Predictions), count)
	}
	out := make([][]float32, count)
	var problems []error
	for i, item := range response.Predictions {
		v, err := decodeVector(item.Embeddings.Values, c.model.Dimensions, c.model.Normalization)
		if err != nil {
			problems = append(problems, &VectorError{Index: i, Err: err})
			continue
		}
		out[i] = v
	}
	return out, problems, nil
}
