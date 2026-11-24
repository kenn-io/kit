package embedmodel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
)

func TestVertexIdentity(t *testing.T) {
	d := sampleDescriptor()
	original, err := d.VectorIdentity()
	require.NoError(t, err)
	d.Deployment.API = embedconfig.APIVertex
	vertex, err := d.VectorIdentity()
	require.NoError(t, err)
	assert.NotEqual(t, original, vertex)
	d.Model.RequestDimensions = true
	requested, err := d.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, vertex, requested)
	d.Roles.InputType = embedconfig.InputTypeRetrieval
	retrieval, err := d.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, vertex, retrieval)
}

func TestDescriptorRejectsInvalidAPIWithoutEndpoint(t *testing.T) {
	d := sampleDescriptor()
	d.Deployment = embedconfig.Deployment{API: "invalid"}
	_, err := d.VectorIdentity()
	assert.Error(t, err)
}
