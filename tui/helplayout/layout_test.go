package helplayout

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReflowRows(t *testing.T) {
	item := func(key, description string) HelpItem {
		return HelpItem{Key: key, Description: description}
	}

	tests := []struct {
		name   string
		rows   [][]HelpItem
		width  int
		gap    int
		wanted [][]HelpItem
	}{
		{
			name:   "empty input",
			rows:   nil,
			width:  80,
			gap:    2,
			wanted: nil,
		},
		{
			name: "wide width preserves source rows",
			rows: [][]HelpItem{
				{item("a", "one"), item("b", "two"), item("c", "three")},
				{item("d", "four"), item("e", "five")},
			},
			width: 23,
			gap:   2,
			wanted: [][]HelpItem{
				{item("a", "one"), item("b", "two"), item("c", "three")},
				{item("d", "four"), item("e", "five")},
			},
		},
		{
			name: "narrow width chunks each source row in stable order",
			rows: [][]HelpItem{
				{item("a", "one"), item("b", "two"), item("c", "three")},
				{item("d", "four"), item("e", "five")},
			},
			width: 15,
			gap:   2,
			wanted: [][]HelpItem{
				{item("a", "one"), item("b", "two")},
				{item("c", "three")},
				{item("d", "four"), item("e", "five")},
			},
		},
		{
			name: "long key and description force fewer columns",
			rows: [][]HelpItem{{
				item("very-long-key", "long description"),
				item("x", "one"),
			}},
			width: 35,
			gap:   2,
			wanted: [][]HelpItem{
				{item("very-long-key", "long description")},
				{item("x", "one")},
			},
		},
		{
			name:   "single item remains one row",
			rows:   [][]HelpItem{{item("enter", "open")}},
			width:  10,
			gap:    2,
			wanted: [][]HelpItem{{item("enter", "open")}},
		},
		{
			name:   "exact width fits",
			rows:   [][]HelpItem{{item("a", "one"), item("b", "two")}},
			width:  12,
			gap:    2,
			wanted: [][]HelpItem{{item("a", "one"), item("b", "two")}},
		},
		{
			name:  "one cell below exact width reflows",
			rows:  [][]HelpItem{{item("a", "one"), item("b", "two")}},
			width: 11,
			gap:   2,
			wanted: [][]HelpItem{
				{item("a", "one")},
				{item("b", "two")},
			},
		},
		{
			name: "overwide item remains intact on its own row",
			rows: [][]HelpItem{{
				item("very-long-key", "long description"),
				item("x", "one"),
			}},
			width: 8,
			gap:   2,
			wanted: [][]HelpItem{
				{item("very-long-key", "long description")},
				{item("x", "one")},
			},
		},
		{
			name:   "unicode uses terminal cell widths",
			rows:   [][]HelpItem{{item("界", "go"), item("x", "")}},
			width:  8,
			gap:    2,
			wanted: [][]HelpItem{{item("界", "go"), item("x", "")}},
		},
		{
			name: "nonpositive width preserves source grouping",
			rows: [][]HelpItem{
				{item("a", "one"), item("b", "two")},
				{item("c", "three")},
			},
			width: 0,
			gap:   2,
			wanted: [][]HelpItem{
				{item("a", "one"), item("b", "two")},
				{item("c", "three")},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.wanted, ReflowRows(test.rows, test.width, test.gap))
		})
	}
}
