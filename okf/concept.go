package okf

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// sourcesProblems validates declared provenance without resolving resources.
// A resource may be an opaque scope descriptor rather than a URL or path.
func sourcesProblems(sources *yaml.Node) []string {
	if sources == nil || emptyOptional(sources) {
		return nil
	}
	entries := sources.Content
	if sources.Kind == yaml.MappingNode {
		entries = []*yaml.Node{sources}
	} else if sources.Kind != yaml.SequenceNode {
		return []string{"sources must be a sequence or a mapping"}
	}
	var problems []string
	ids := make(map[string]struct{})
	for i, source := range entries {
		entry := fmt.Sprintf("sources[%d]", i)
		if source.Kind != yaml.MappingNode {
			problems = append(problems, entry+" must be a mapping")
			continue
		}
		if !NonEmptyString(mappingField(source, "resource")) {
			problems = append(problems, entry+".resource must be a non-empty string")
		}
		if id := mappingField(source, "id"); id != nil && !emptyOptional(id) {
			if !NonEmptyString(id) {
				problems = append(problems, entry+".id must be a non-empty string")
			} else if _, exists := ids[id.Value]; exists {
				problems = append(problems, entry+".id must be unique within sources")
			} else {
				ids[id.Value] = struct{}{}
			}
		}
	}
	return problems
}

// emptyOptional follows OKF v0.3's absent-value equivalence for optional fields.
func emptyOptional(n *yaml.Node) bool {
	return n == nil || n.Tag == "!!null" || (n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Value == "") || (n.Kind == yaml.SequenceNode && len(n.Content) == 0)
}

// ConceptProblems checks type, optional status, and source declarations.
// Other fields (including unknown extensions) are left to the caller's profile.
func (d *Document) ConceptProblems() []string {
	problems := sourcesProblems(d.Field("sources"))
	if !NonEmptyString(d.Field("type")) {
		problems = append(problems, "type must be a non-empty string")
	}
	if status := d.Field("status"); !emptyOptional(status) && !NonEmptyString(status) {
		problems = append(problems, "status must be a non-empty string")
	}
	return problems
}

// NonEmptyString reports whether a field is a YAML string containing non-space text.
func NonEmptyString(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" && strings.TrimSpace(n.Value) != ""
}
