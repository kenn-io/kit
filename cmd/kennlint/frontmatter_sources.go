package main

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// sourcesProblems validates declared provenance without resolving resources.
// A resource may be an opaque scope descriptor rather than a URL or path.
func sourcesProblems(sources *yaml.Node, path string) []string {
	if sources == nil {
		return nil
	}
	if sources.Kind != yaml.SequenceNode {
		return []string{path + " must be a sequence"}
	}
	var problems []string
	ids := make(map[string]struct{})
	for i, source := range sources.Content {
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
