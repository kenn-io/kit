package embedmodel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
)

func TestVertexTaskTypeIsPartOfVectorSpace(t *testing.T) {
	model := embedconfig.Model{Name: "model", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2}
	fields, err := vectorFields(model, embedconfig.Roles{}, embedconfig.Deployment{API: embedconfig.APIVertex})
	require.NoError(t, err)
	assert.Equal(t, "RETRIEVAL_DOCUMENT", fields["vertex_task_type"])
	model.Name = "gemini-embedding-2"
	fields, err = vectorFields(model, embedconfig.Roles{}, embedconfig.Deployment{API: embedconfig.APIVertex})
	require.NoError(t, err)
	assert.NotContains(t, fields, "vertex_task_type")
}
