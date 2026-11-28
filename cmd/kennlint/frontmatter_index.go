package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/text"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/okf"
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
		excluded, err := frontmatterResolvesBeyondBoundary(root, file, skillDirs, excludes)
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
		b, err := okf.Parse(src)
		if err != nil {
			return nil, &indexPageError{file, err}
		}
		if status := b.Field("status"); status != nil && status.Tag != "!!null" && (status.Tag != "!!str" || status.Value != "") && (!nonEmptyString(status) || (status.Value != "stable" && status.Value != "draft")) {
			continue
		}
		title, description := b.Field("title"), b.Field("description")
		if title == nil || description == nil || !nonEmptyString(title) || !nonEmptyString(description) {
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
	if excluded, err := frontmatterResolvesBeyondBoundary(root, file, skillDirs, excludes); err != nil || excluded {
		return nil, false, err
	}
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return nil, false, err
	}
	body := src
	block, err := okf.Parse(src)
	switch {
	case err == nil:
		body = block.Body
	case !errors.Is(err, okf.ErrNoFrontmatter):
		return src, false, err
	}
	bounds, err := findIndexBlock(body)
	if err != nil || bounds == nil {
		return src, false, err
	}
	offset := len(src) - len(body)
	bounds.start += offset
	bounds.finish += offset
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

// A selected path can change any managed index above it, including when the
// selected file was removed. Selecting a directory also selects indexes below it.
func frontmatterIndexSelected(file string, selected []string) bool {
	if len(selected) == 0 {
		return true
	}
	dir := path.Dir(file)
	return slices.ContainsFunc(selected, func(p string) bool {
		return p == "." || dir == "." || p == dir || strings.HasPrefix(p, dir+"/") || strings.HasPrefix(file, p+"/")
	})
}

func frontmatterUpdateIndexes(root string, files, selected []string, skillDirs map[string]struct{}, excludes []string, fix bool, reported map[string]bool, report func(string, []string)) (int, error) {
	if _, rootSkill := skillDirs["."]; rootSkill {
		return 0, nil
	}
	fixed := 0
	for _, file := range files {
		if reported[file] || path.Base(file) != "index.md" || frontmatterHiddenPath(file) || frontmatterInSkillDirectory(file, skillDirs) || !frontmatterSelected(file, nil, excludes) || !frontmatterIndexSelected(file, selected) {
			continue
		}
		updated, stale, err := refreshTrackedIndex(root, file, files, skillDirs, excludes)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			pageErr, ok := errors.AsType[*indexPageError](err)
			if !ok || !reported[pageErr.file] {
				report(file, []string{err.Error()})
			}
			continue
		}
		if !stale {
			continue
		}
		if !fix {
			report(file, []string{"managed index is stale"})
			continue
		}
		if err := atomicfile.WriteFile(filepath.Join(root, filepath.FromSlash(file)), updated, atomicfile.WithPreserveMode(), atomicfile.WithFollowLink()); err != nil {
			return fixed, err
		}
		fixed++
	}
	return fixed, nil
}
