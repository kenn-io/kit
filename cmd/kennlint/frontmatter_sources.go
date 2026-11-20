package main

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// sourcesProblems validates declared provenance without resolving resources.
// A resource may be an opaque scope descriptor rather than a URL or path.
func sourcesProblems(sources *yaml.Node, path string) []string {
	sources = resolveYAML(sources)
	if emptyOptional(sources) {
		return nil
	}
	entries := sources.Content
	if sources.Kind == yaml.MappingNode {
		entries = []*yaml.Node{sources}
	} else if sources.Kind != yaml.SequenceNode {
		return []string{path + " must be a sequence"}
	}
	var problems []string
	ids := make(map[string]struct{})
	for i, source := range entries {
		source = resolveYAML(source)
		entry := fmt.Sprintf("%s[%d]", path, i)
		if source.Kind != yaml.MappingNode {
			problems = append(problems, entry+" must be a mapping")
			continue
		}
		if !validString(mappingField(source, "resource")) {
			problems = append(problems, entry+".resource must be a non-empty string")
		}
		if id := mappingField(source, "id"); id != nil {
			if !validString(id) {
				problems = append(problems, entry+".id must be a non-empty string")
			} else if _, exists := ids[id.Value]; exists {
				problems = append(problems, entry+".id must be unique within "+path)
			} else {
				ids[id.Value] = struct{}{}
			}
		}
	}
	return problems
}

// emptyOptional follows OKF v0.3's absent-value equivalence for optional fields.
func emptyOptional(n *yaml.Node) bool {
	n = resolveYAML(n)
	return n == nil || n.Tag == "!!null" || (n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Value == "") || (n.Kind == yaml.SequenceNode && len(n.Content) == 0)
}
