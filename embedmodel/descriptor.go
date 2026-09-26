package embedmodel

import (
	"errors"
	"strings"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/vector"
)

// ErrIncompatible reports that two descriptors do not share a vector space.
var ErrIncompatible = errors.New("embed descriptors use different vector spaces")

// Descriptor binds a model, the role controls that change its inputs, and the
// input recipe used to produce those inputs. Coordinates requires source spans
// on non-blank text.
type Descriptor struct {
	Model       embedconfig.Model
	Roles       embedconfig.Roles
	Deployment  embedconfig.Deployment
	Input       embedconfig.InputLimits
	Coordinates bool
}

// Validate checks the descriptor parts after trimming them, the same way the
// identities read them. An empty deployment is allowed when the caller has
// not pinned an endpoint yet.
func (d Descriptor) Validate() error {
	if _, _, _, err := prepareSpace(d.Model, d.Roles, d.Deployment); err != nil {
		return err
	}
	_, err := d.Input.Prepared()
	return err
}

// VectorIdentity returns the comparable vector-space id. It covers the model,
// metric, normalization, pooling, requested dimensions, role affixes and
// formatters, input type, and the endpoint only when PinEndpoint is set.
func (d Descriptor) VectorIdentity() (string, error) {
	return vectorIdentity(d.Model, d.Roles, d.Deployment)
}

// InputIdentity returns the indexed-input id: the vector-space fields plus
// the recipe, tokenizer, content selection, and token window.
func (d Descriptor) InputIdentity() (string, error) {
	return inputIdentity(d.Model, d.Roles, d.Deployment, d.Input)
}

// Generation returns the kit generation for this descriptor.
// Params carries only the vector-space id under "vector_space".
// The input recipe stays on InputIdentity. Params does not carry batch size
// or timeout.
func (d Descriptor) Generation() (vector.Generation, error) {
	space, err := d.VectorIdentity()
	if err != nil {
		return vector.Generation{}, err
	}
	return vector.Generation{
		Model:      strings.TrimSpace(d.Model.Name),
		Dimensions: d.Model.Dimensions,
		Params: map[string]string{
			"vector_space": space,
		},
	}, nil
}

// ValidateContent checks one input against this descriptor.
func (d Descriptor) ValidateContent(content Content) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := content.Validate(); err != nil {
		return err
	}
	text, err := content.EmbedText()
	if err != nil {
		if errors.Is(err, ErrUnsupportedContent) {
			return nil
		}
		return err
	}
	if d.Coordinates && !BlankText(text) && len(content.Spans) == 0 {
		return errors.New("embed content requires source spans")
	}
	return nil
}

// Compatible reports whether document and query vectors can be compared.
// Input windows may differ.
func Compatible(document, query Descriptor) error {
	if err := document.Validate(); err != nil {
		return err
	}
	if err := query.Validate(); err != nil {
		return err
	}
	left, err := document.VectorIdentity()
	if err != nil {
		return err
	}
	right, err := query.VectorIdentity()
	if err != nil {
		return err
	}
	if left != right {
		return ErrIncompatible
	}
	return nil
}
