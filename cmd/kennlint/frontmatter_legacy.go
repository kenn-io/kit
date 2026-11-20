package main

import (
	"time"

	"gopkg.in/yaml.v3"

	"go.kenn.io/kit/okf"
)

func frontmatterDate(n *yaml.Node) bool {
	if n == nil || n.Kind != yaml.ScalarNode || len(n.Value) != len(time.DateOnly) {
		return false
	}
	date, err := time.Parse(time.DateOnly, n.Value)
	return err == nil && date.Year() > 0
}

func legacyProblems(b *okf.Document) []string {
	var problems []string
	for _, key := range []string{"title", "description"} {
		if !okf.NonEmptyString(b.Field(key)) {
			problems = append(problems, key+" must be a non-empty string")
		}
	}
	n := b.Field("last_edited")
	if n == nil || !frontmatterDate(n) || n.Tag != "!!timestamp" || n.Style != 0 {
		problems = append(problems, "last_edited must be a bare YYYY-MM-DD date")
	}
	return problems
}

func fixLegacyDate(b *okf.Document, date string) bool {
	n := b.Field("last_edited")
	switch {
	case n == nil:
		n = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: date}
		appendField(&b.Metadata, "last_edited", n)
	case frontmatterDate(n) && (n.Style == yaml.SingleQuotedStyle || n.Style == yaml.DoubleQuotedStyle):
		n.Tag, n.Style = "!!timestamp", 0
	default:
		return false
	}
	return true
}
