package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/text"
)

const (
	indexBegin = "<!-- BEGIN kennlint index -->"
	indexEnd   = "<!-- END kennlint index -->"
)

type indexPage struct{ path, title, description string }

type indexPageError struct {
	file string
	err  error
}

func (e *indexPageError) Error() string { return e.file + ": " + e.err.Error() }
func (e *indexPageError) Unwrap() error { return e.err }

type indexBlock struct {
	start, finish int
	newline       string
}

// findIndexBlock reads markers only from HTML blocks, so marker lines inside
// code, such as a documented example, are never a managed block.
func findIndexBlock(src []byte) (*indexBlock, error) {
	var begins, ends []text.Segment
	for n := parser.New().Parse(src).FirstChild(); n != nil; n = n.NextSibling() {
		if html, ok := n.(*ast.HTMLBlock); ok {
			for _, line := range html.Value.Segments() {
				switch strings.TrimRight(string(src[line.Start:line.Stop]), "\r\n") {
				case indexBegin:
					begins = append(begins, line)
				case indexEnd:
					ends = append(ends, line)
				}
			}
		}
	}
	if len(begins) == 0 && len(ends) == 0 {
		return nil, nil
	}
	if len(begins) != 1 || len(ends) != 1 || ends[0].Start < begins[0].Stop {
		return nil, errors.New("invalid managed index markers")
	}
	newline := "\n"
	if bytes.HasSuffix(src[:begins[0].Stop], []byte("\r\n")) {
		newline = "\r\n"
	}
	return &indexBlock{begins[0].Stop, ends[0].Start, newline}, nil
}

func renderIndex(src []byte, bounds *indexBlock, pages []indexPage) ([]byte, bool) {
	var block strings.Builder
	for _, page := range pages {
		title := escapeIndexText(page.title)
		parts := strings.Split(page.path, "/")
		for i, part := range parts {
			parts[i] = url.PathEscape(part)
		}
		destination := strings.Join(parts, "/")
		if strings.Contains(parts[0], ":") {
			destination = "./" + destination
		}
		fmt.Fprintf(&block, "- [%s](%s) - %s%s", title, destination, escapeIndexText(page.description), bounds.newline)
	}
	result := append(append(bytes.Clone(src[:bounds.start]), block.String()...), src[bounds.finish:]...)
	return result, !bytes.Equal(src, result)
}

func escapeIndexText(text string) string {
	var escaped strings.Builder
	for _, r := range strings.Join(strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '\f' }), " ") {
		if r < 128 && strings.ContainsRune("\\`*_[]<>&!#|~:", r) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}
	return escaped.String()
}

func indexPages(root, index string, files []string, skillDirs map[string]struct{}, excludes []string) ([]indexPage, error) {
	var pages []indexPage
	dir := path.Dir(index)
	for _, file := range files {
		rel, ok := strings.CutPrefix(file, dir+"/")
		if dir == "." {
			rel, ok = file, true
		}
		base := path.Base(file)
		if !ok || frontmatterHiddenPath(rel) || base == "index.md" || base == "log.md" || frontmatterInSkillDirectory(file, skillDirs) || !frontmatterSelected(file, nil, excludes) {
			continue
		}
		excluded, err := frontmatterExcludedTarget(root, file, skillDirs, excludes)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		b, err := parseFrontmatter(src)
		if err != nil {
			return nil, &indexPageError{file, err}
		}
		if status := b.fields["status"]; !emptyOptional(status) && (!validString(status) || (status.Value != "stable" && status.Value != "draft")) {
			continue
		}
		title, description := b.fields["title"], b.fields["description"]
		if title == nil || description == nil || !validString(title) || !validString(description) {
			return nil, &indexPageError{file, errors.New("index page requires a valid title and description")}
		}
		pages = append(pages, indexPage{rel, title.Value, description.Value})
	}
	return pages, nil
}

func refreshTrackedIndex(root, file string, files []string, skillDirs map[string]struct{}, excludes []string) ([]byte, bool, error) {
	// An index alias, through a linked final element or ancestor directory, can
	// point into imported or excluded content. Only directly owned index files
	// may be regenerated.
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(file))); err != nil {
		return nil, false, err
	}
	if excluded, err := frontmatterExcludedTarget(root, file, skillDirs, excludes); err != nil || excluded {
		return nil, false, err
	}
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return nil, false, err
	}
	bounds, err := findIndexBlock(src)
	if err != nil || bounds == nil {
		return src, false, err
	}
	pages, err := indexPages(root, file, files, skillDirs, excludes)
	if err != nil {
		return nil, false, err
	}
	updated, changed := renderIndex(src, bounds, pages)
	return updated, changed, nil
}

func frontmatterHiddenPath(file string) bool {
	for part := range strings.SplitSeq(file, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}
