// Package workflowretry checks workflow and composite-action downloads for retries.
package workflowretry

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kit/lint/internal/yamlnode"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

// Finding identifies a download and its owning run field.
type Finding struct {
	Line, Column       int
	Job, Step, Message string
}

// Scan checks run steps using Bash or sh, including steps with no explicit shell.
func Scan(src []byte) ([]Finding, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 {
		return nil, nil
	}
	root := doc.Content[0]
	var findings []Finding
	check := func(steps *yaml.Node, job, shell string) {
		if steps == nil {
			return
		}
		for i, step := range steps.Content {
			for step.Kind == yaml.AliasNode {
				step = step.Alias
			}
			run := value(step, "run")
			if run == nil {
				continue
			}
			selected := text(step, "shell")
			if selected == "" {
				selected = shell
			}
			if skipShell(selected) {
				continue
			}
			name := text(step, "name")
			if name == "" {
				name = strconv.Itoa(i + 1)
			}
			for _, message := range scanScript(run.Value) {
				line, column := run.Line, run.Column
				for i := 0; i+1 < len(step.Content); i += 2 {
					if step.Content[i].Value == "run" {
						line, column = step.Content[i].Line, step.Content[i].Column
						break
					}
				}
				findings = append(findings, Finding{line, column, job, name, message})
			}
		}
	}
	if jobs := value(root, "jobs"); jobs != nil {
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			job := jobs.Content[i+1]
			shell := text(value(value(job, "defaults"), "run"), "shell")
			if shell == "" {
				shell = text(value(value(root, "defaults"), "run"), "shell")
			}
			check(value(job, "steps"), jobs.Content[i].Value, shell)
		}
	}
	if runs := value(root, "runs"); text(runs, "using") == "composite" {
		check(value(runs, "steps"), "composite", "")
	}
	return findings, nil
}

func value(node *yaml.Node, key string) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	result := yamlnode.MappingValue(node, key)
	for result != nil && result.Kind == yaml.AliasNode {
		result = result.Alias
	}
	return result
}

func text(node *yaml.Node, key string) string {
	if v := value(node, key); v != nil {
		return v.Value
	}
	return ""
}

// skipShell reports a shell known not to be Bash or sh; an Actions expression may resolve to Bash.
func skipShell(shell string) bool {
	fields := strings.Fields(shell)
	if len(fields) == 0 || strings.Contains(shell, "${{") {
		return false
	}
	return executable(fields[0]) != "bash" && executable(fields[0]) != "sh"
}

func executable(name string) string {
	return strings.TrimSuffix(path.Base(strings.ReplaceAll(name, `\`, "/")), ".exe")
}

var expressions = regexp.MustCompile(`(?s)\$\{\{.*?\}\}`)

// scanScript applies the download and installer rules to executable shell calls.
func scanScript(source string) []string {
	masked := expressions.ReplaceAllStringFunc(source, func(span string) string {
		masked := []byte(span)
		for i, c := range masked {
			if c != '\n' {
				masked[i] = 'x'
			}
		}
		return string(masked)
	})
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).Parse(strings.NewReader(masked), "")
	if err != nil {
		return []string{"parse shell: " + err.Error()}
	}
	allowed := map[uint]bool{}
	lines := strings.Split(source, "\n")
	syntax.Walk(file, func(node syntax.Node) bool {
		if c, ok := node.(*syntax.Comment); ok {
			if reason, ok := strings.CutPrefix(c.Text, " workflowretry: allow "); ok && strings.TrimSpace(reason) != "" {
				line := c.Pos().Line()
				allowed[line] = true
				if strings.TrimSpace(lines[line-1][:c.Pos().Col()-1]) == "" {
					allowed[line+1] = true
				}
			}
		}
		return true
	})
	var findings []string
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || allowed[call.Pos().Line()] {
			return true
		}
		var args []string
		for _, word := range call.Args {
			args = append(args, literal(word))
		}
		args = unprefix(args)
		wrapped := len(args) >= 4 && executable(args[0]) == "kit-retry"
		if wrapped {
			args = unprefix(args[3:])
		}
		if len(args) == 0 {
			return true
		}
		tool := executable(args[0])
		if tool == "curl" || tool == "wget" {
			findings = append(findings, "use kit-download")
		} else if !wrapped {
			words := append([]string{tool}, skipOptions(args[1:], optionValues[tool])...)
			for _, row := range installerCommands {
				if len(words) >= len(row) && slices.Equal(words[:len(row)], row) {
					findings = append(findings, "wrap in kit-retry")
					break
				}
			}
		}
		return true
	})
	return findings
}

func literal(word *syntax.Word) string {
	var result strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			result.WriteString(part.Value)
		case *syntax.SglQuoted:
			result.WriteString(part.Value)
		case *syntax.DblQuoted:
			result.WriteString(literal(&syntax.Word{Parts: part.Parts}))
		default:
			return "\x00"
		}
	}
	return result.String()
}

func unprefix(args []string) []string {
	for len(args) > 0 && (executable(args[0]) == "sudo" || executable(args[0]) == "env") {
		args = skipOptions(args[1:], optionValues[executable(args[0])])
		for len(args) > 0 && strings.Contains(args[0], "=") {
			args = args[1:]
		}
	}
	return args
}

func skipOptions(args, values []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		option := args[0]
		args = args[1:]
		if option == "--" {
			break
		}
		if slices.Contains(values, option) && len(args) > 0 {
			args = args[1:]
		}
	}
	return args
}

var optionValues = map[string][]string{
	"sudo": {"-u", "-g", "--user", "--group"}, "env": {"-u", "--unset", "-C", "--chdir"},
	"apt": {"-o", "--option", "-c", "--config-file"}, "apt-get": {"-o", "--option", "-c", "--config-file"},
	"go": {"-C"},
}

var installerCommands = func() [][]string {
	var rows [][]string
	for _, family := range []string{"apt|apt-get install|update|upgrade|dist-upgrade", "go install|get", "go mod download", "npm|pnpm|yarn|bun install|ci|i|add", "pip|pip3 install", "docker pull", "apk add|update", "brew|cargo|pipx|dnf install"} {
		fields := strings.Fields(family)
		for tool := range strings.SplitSeq(fields[0], "|") {
			for command := range strings.SplitSeq(fields[len(fields)-1], "|") {
				rows = append(rows, append(append([]string{tool}, fields[1:len(fields)-1]...), command))
			}
		}
	}
	return rows
}()
