package helplayout

import "github.com/mattn/go-runewidth"

// HelpItem is one unstyled key-and-description entry in a help display.
// Description may be empty.
type HelpItem struct {
	Key         string
	Description string
}

// ReflowRows returns rows using the greatest aligned column count that fits
// within availableWidth.
//
// An item occupies the terminal-cell width of Key. A non-empty Description
// adds one separating cell followed by its terminal-cell width. columnGap is
// the complete visible width inserted before every column after the first; a
// negative value is treated as zero. Column widths are the widest item in that
// column across all returned rows.
//
// Items retain their input order, and chunks from one source row are never
// combined with another source row. If availableWidth is nonpositive, the
// source grouping is preserved. An item wider than availableWidth is not
// truncated or split; it is returned intact on its own row.
func ReflowRows(rows [][]HelpItem, availableWidth, columnGap int) [][]HelpItem {
	if availableWidth <= 0 {
		return cloneRows(rows)
	}
	columnGap = max(columnGap, 0)

	maxItemsPerRow := 0
	for _, row := range rows {
		maxItemsPerRow = max(maxItemsPerRow, len(row))
	}
	if maxItemsPerRow == 0 {
		return nil
	}

	for columns := maxItemsPerRow; columns >= 1; columns-- {
		candidate := chunkRows(rows, columns)
		columnWidths := make([]int, columns)
		for _, row := range candidate {
			for column, item := range row {
				columnWidths[column] = max(columnWidths[column], itemWidth(item))
			}
		}

		totalWidth := columnGap * (columns - 1)
		for _, width := range columnWidths {
			totalWidth += width
		}
		if totalWidth <= availableWidth {
			return candidate
		}
	}

	return chunkRows(rows, 1)
}

func itemWidth(item HelpItem) int {
	width := runewidth.StringWidth(item.Key)
	if item.Description != "" {
		width += 1 + runewidth.StringWidth(item.Description)
	}
	return width
}

func chunkRows(rows [][]HelpItem, columns int) [][]HelpItem {
	var result [][]HelpItem
	for _, row := range rows {
		for start := 0; start < len(row); start += columns {
			end := min(start+columns, len(row))
			result = append(result, append([]HelpItem(nil), row[start:end]...))
		}
	}
	return result
}

func cloneRows(rows [][]HelpItem) [][]HelpItem {
	if rows == nil {
		return nil
	}
	result := make([][]HelpItem, len(rows))
	for i, row := range rows {
		result[i] = append([]HelpItem(nil), row...)
	}
	return result
}
