package embedclient

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	json "encoding/json/v2"

	"go.kenn.io/kit/embedconfig"
)

// vertexMaxBatchItems is the most texts Vertex takes in one :predict request.
const vertexMaxBatchItems = 250

// vertexBatchModels take vertexMaxBatchItems texts per :predict request.
// Other :predict models send one text per request.
var vertexBatchModels = map[string]bool{
	"gemini-embedding-001":            true,
	"text-embedding-005":              true,
	"text-multilingual-embedding-002": true,
}

// validateGeminiEmbedding2Roles rejects roles that carry no task instruction.
// Gemini Embedding 2 ignores task_type, so without one a query and a document
// with the same text get the same vector.
func validateGeminiEmbedding2Roles(r embedconfig.Roles) error {
	if r.DocumentPrefix == "" && r.QueryPrefix == "" && r.DocumentFormatter == "" && r.QueryFormatter == "" {
		return errors.New(`embed model gemini-embedding-2 needs its task in the role prefixes, such as query prefix "task: search result | query: " and document prefix "title: none | text: "`)
	}
	return nil
}

type vertexInstance struct {
	Content  string `json:"content"`
	TaskType string `json:"task_type"`
}

func (c *Client) vertexRequestBody(inputs []encoded) ([]byte, error) {
	if strings.HasPrefix(c.model.Name, "gemini-embedding-2") {
		part := map[string]any{"text": inputs[0].text}
		if inputs[0].input.MediaType != "" {
			part = map[string]any{"inlineData": map[string]any{"mimeType": inputs[0].input.MediaType, "data": inputs[0].input.Data}}
		}
		body, err := json.Marshal(map[string]any{
			"content":            map[string]any{"parts": []any{part}},
			"embedContentConfig": map[string]any{"outputDimensionality": c.model.Dimensions},
		})
		if err != nil {
			return nil, fmt.Errorf("embed request encoding failed: %w", err)
		}
		return body, nil
	}
	instances := make([]vertexInstance, len(inputs))
	for i, input := range inputs {
		taskType := "RETRIEVAL_DOCUMENT"
		if input.role == embedconfig.RoleQuery {
			taskType = "RETRIEVAL_QUERY"
		}
		instances[i] = vertexInstance{Content: input.text, TaskType: taskType}
	}
	var request struct {
		Instances  []vertexInstance `json:"instances"`
		Parameters struct {
			OutputDimensionality int  `json:"outputDimensionality"`
			AutoTruncate         bool `json:"autoTruncate"`
		} `json:"parameters"`
	}
	request.Instances = instances
	request.Parameters.OutputDimensionality = c.model.Dimensions
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("embed request encoding failed: %w", err)
	}
	return body, nil
}

func (c *Client) classifyVertex(payload []byte, count int) ([][]float32, []error, error) {
	if strings.HasPrefix(c.model.Name, "gemini-embedding-2") {
		var response struct {
			Embedding struct {
				Values wireEmbedding `json:"values"`
			} `json:"embedding"`
			Truncated bool `json:"truncated"`
		}
		if err := json.Unmarshal(payload, &response); err != nil {
			return nil, nil, errors.New("embed response is invalid")
		}
		if response.Truncated {
			return nil, nil, &APIError{StatusCode: http.StatusOK, Reason: ReasonInputTooLong}
		}
		out, problems := c.decodeVectors([]wireEmbedding{response.Embedding.Values})
		return out, problems, nil
	}
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
	raw := make([]wireEmbedding, count)
	for i, item := range response.Predictions {
		raw[i] = item.Embeddings.Values
	}
	out, problems := c.decodeVectors(raw)
	return out, problems, nil
}
