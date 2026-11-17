package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
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

func findIndexBlock(src []byte) (*indexBlock, error) {
	lines := bytes.SplitAfter(src, []byte("\n"))
	begin, end, offset, start, finish := 0, 0, 0, 0, 0
	newline := "\n"
	for _, line := range lines {
		value := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		switch value {
		case indexBegin:
			begin++
			if begin == 1 {
				start = offset + len(line)
				if bytes.HasSuffix(line, []byte("\r\n")) {
					newline = "\r\n"
				}
			}
		case indexEnd:
			end++
			if end == 1 {
				finish = offset
			}
		}
		offset += len(line)
	}
	if begin == 0 && end == 0 {
		return nil, nil
	}
	if begin != 1 || end != 1 || finish < start {
		return nil, errors.New("invalid managed index markers")
	}
	return &indexBlock{start, finish, newline}, nil
}

func renderIndex(src []byte, bounds *indexBlock, pages []indexPage) ([]byte, bool) {
	slices.SortFunc(pages, func(a, b indexPage) int { return strings.Compare(a.path, b.path) })
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
		if dir != "." && !strings.HasPrefix(file, dir+"/") {
			continue
		}
		base := path.Base(file)
		if base == "index.md" || base == "log.md" || base == "SKILL.md" || frontmatterInSkillDirectory(file, skillDirs) || !frontmatterSelected(file, nil, excludes) {
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
		if status := b.fields["status"]; validString(status) && status.Value == "deprecated" {
			continue
		}
		if !validString(b.fields["title"]) || !validString(b.fields["description"]) {
			return nil, &indexPageError{file, errors.New("index page requires a valid title and description")}
		}
		rel, err := filepath.Rel(filepath.FromSlash(dir), filepath.FromSlash(file))
		if err != nil {
			return nil, err
		}
		pages = append(pages, indexPage{filepath.ToSlash(rel), b.fields["title"].Value, b.fields["description"].Value})
	}
	return pages, nil
}

func refreshTrackedIndex(root, file string, files []string, skillDirs map[string]struct{}, excludes []string) ([]byte, bool, error) {
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
