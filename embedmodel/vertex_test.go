package embedmodel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kit/embedconfig"
)

func TestDescriptorRejectsInvalidAPIWithoutEndpoint(t *testing.T) {
	d := sampleDescriptor()
	d.Deployment = embedconfig.Deployment{API: "invalid"}
	_, err := d.VectorIdentity()
	assert.Error(t, err)
}
