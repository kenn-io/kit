package agentmcp

import (
	"bytes"
	"maps"
	"reflect"
	"slices"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
)

func editTOML(original []byte, name string, entry map[string]any) ([]byte, error) {
	doc, err := tomledit.Parse(bytes.NewReader(original))
	if err != nil {
		return nil, err
	}
	encoded, err := encodeDocument(AgentCodex, entry)
	if err != nil {
		return nil, err
	}
	replacement, err := tomledit.Parse(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	key := parser.Key{"mcp_servers", name}
	var inlineEntry parser.Inline
	if doc.First("mcp_servers") != nil && doc.First("mcp_servers").IsMapping() || doc.First(key...) != nil && doc.First(key...).IsMapping() {
		value, err := tomlInlineValue(reflect.ValueOf(entry))
		if err != nil {
			return nil, err
		}
		inlineEntry = value.X.(parser.Inline)
	}
	switch {
	case doc.First("mcp_servers") != nil && doc.First("mcp_servers").IsMapping():
		// TOML inline tables cannot be extended by a later section declaration.
		parent := doc.First("mcp_servers").KeyValue
		inline := parent.Value.X.(parser.Inline)
		var kept parser.Inline
		var comments parser.Comments
		for _, field := range inline {
			if (parser.Key{name}).IsPrefixOf(field.Name) {
				comments = append(comments, tomlKeyComments(field)...)
			} else {
				kept = append(kept, field)
			}
		}
		parent.Block = append(parent.Block, comments...)
		kept = append(kept, &parser.KeyValue{Name: parser.Key{name}, Value: parser.Value{X: inlineEntry}})
		parent.Value.X = kept
	case doc.First(key...) != nil && doc.First(key...).IsMapping():
		// An existing inline table can be replaced without changing its parent.
		target := doc.First(key...).KeyValue
		target.Block = append(target.Block, tomlValueComments(target.Value.X)...)
		target.Value.X = inlineEntry
	default:
		var comments parser.Comments
		var kept []*tomledit.Section
		position := -1
		for _, section := range doc.Sections {
			if key.IsPrefixOf(section.TableName()) {
				if position < 0 {
					position = len(kept)
				}
				comments = append(comments, section.Block...)
				if section.Trailer != "" {
					comments = append(comments, section.Trailer)
				}
				comments = append(comments, tomlItemComments(section.Items)...)
			} else {
				kept = append(kept, section)
			}
		}
		doc.Sections = kept
		// Dotted assignments may appear in the global or parent section.
		for _, section := range append([]*tomledit.Section{doc.Global}, doc.Sections...) {
			var items []parser.Item
			for _, item := range section.Items {
				field, ok := item.(*parser.KeyValue)
				if ok && key.IsPrefixOf(append(slices.Clone(section.TableName()), field.Name...)) {
					comments = append(comments, tomlKeyComments(field)...)
				} else {
					items = append(items, item)
				}
			}
			section.Items = items
		}
		root := &tomledit.Section{Heading: &parser.Heading{Name: key, Block: comments}, Items: replacement.Global.Items}
		sections := []*tomledit.Section{root}
		for _, section := range replacement.Sections {
			section.Name = append(slices.Clone(key), section.Name...)
			sections = append(sections, section)
		}
		if position < 0 {
			position = len(doc.Sections)
		}
		doc.Sections = slices.Insert(doc.Sections, position, sections...)
	}
	var out bytes.Buffer
	if err := tomledit.Format(&out, doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func tomlInlineValue(value reflect.Value) (parser.Value, error) {
	if value.Kind() == reflect.Interface {
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Map:
		fields := make(map[string]reflect.Value, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			fields[iter.Key().String()] = iter.Value()
		}
		var inline parser.Inline
		for _, name := range slices.Sorted(maps.Keys(fields)) {
			item, err := tomlInlineValue(fields[name])
			if err != nil {
				return parser.Value{}, err
			}
			inline = append(inline, &parser.KeyValue{Name: parser.Key{name}, Value: item})
		}
		return parser.Value{X: inline}, nil
	case reflect.Slice, reflect.Array:
		var array parser.Array
		for i := range value.Len() {
			item, err := tomlInlineValue(value.Index(i))
			if err != nil {
				return parser.Value{}, err
			}
			array = append(array, item)
		}
		return parser.Value{X: array}, nil
	default:
		encoded, err := encodeDocument(AgentCodex, map[string]any{"value": value.Interface()})
		if err != nil {
			return parser.Value{}, err
		}
		doc, err := tomledit.Parse(bytes.NewReader(encoded))
		if err != nil {
			return parser.Value{}, err
		}
		return doc.Global.Items[0].(*parser.KeyValue).Value, nil
	}
}

func tomlItemComments(items []parser.Item) parser.Comments {
	var comments parser.Comments
	for _, item := range items {
		switch item := item.(type) {
		case parser.Comments:
			comments = append(comments, item...)
		case *parser.KeyValue:
			comments = append(comments, tomlKeyComments(item)...)
		}
	}
	return comments
}

func tomlKeyComments(field *parser.KeyValue) parser.Comments {
	comments := slices.Clone(field.Block)
	comments = append(comments, tomlValueComments(field.Value.X)...)
	if field.Value.Trailer != "" {
		comments = append(comments, field.Value.Trailer)
	}
	return comments
}

func tomlValueComments(value parser.Datum) parser.Comments {
	var comments parser.Comments
	switch value := value.(type) {
	case parser.Inline:
		for _, field := range value {
			comments = append(comments, tomlKeyComments(field)...)
		}
	case parser.Array:
		for _, item := range value {
			switch item := item.(type) {
			case parser.Comments:
				comments = append(comments, item...)
			case parser.Value:
				comments = append(comments, tomlValueComments(item.X)...)
				if item.Trailer != "" {
					comments = append(comments, item.Trailer)
				}
			}
		}
	}
	return comments
}
