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
// on non-blank text. Lexical is recorded beside the descriptor so callers can
// see that it is a different identity.
type Descriptor struct {
	Model       embedconfig.Model
	Roles       embedconfig.Roles
	Deployment  embedconfig.Deployment
	Input       embedconfig.InputLimits
	Lexical     LexicalAnalyzer
	Coordinates bool
}

// Validate checks the descriptor parts. An empty deployment is allowed when
// the caller has not pinned an endpoint yet.
func (d Descriptor) Validate() error {
	if err := d.Model.Validate(); err != nil {
		return err
	}
	if err := d.Roles.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(d.Deployment.BaseURL) != "" || d.Deployment.PinEndpoint {
		if err := d.Deployment.Validate(); err != nil {
			return err
		}
	}
	if err := d.Input.Validate(); err != nil {
		return err
	}
	return d.Lexical.Validate()
}

// VectorIdentity returns the comparable vector-space id.
func (d Descriptor) VectorIdentity() (string, error) {
	return embedconfig.VectorIdentity(d.Model, d.Roles, d.Deployment)
}

// InputIdentity returns the indexed-input id.
func (d Descriptor) InputIdentity() (string, error) {
	return embedconfig.InputIdentity(d.Model, d.Roles, d.Deployment, d.Input)
}

// Generation returns the kit generation for this descriptor.
// The parameters carry the vector-space id and the input-recipe id. They do
// not carry the lexical analyzer, batch size, timeout, or retrieval budget.
func (d Descriptor) Generation() (vector.Generation, error) {
	space, err := d.VectorIdentity()
	if err != nil {
		return vector.Generation{}, err
	}
	recipe, err := d.InputIdentity()
	if err != nil {
		return vector.Generation{}, err
	}
	return vector.Generation{
		Model:      strings.TrimSpace(d.Model.Name),
		Dimensions: d.Model.Dimensions,
		Params: map[string]string{
			"vector_space": space,
			"input_recipe": recipe,
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
// Input windows may differ. Lexical analyzers are ignored.
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
