// Package workflowretry checks workflow and composite-action downloads for retries.
package workflowretry

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/kit/lint/internal/yamlnode"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/expand"
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
			if shell == "" {
				runner, err := yaml.Marshal(value(job, "runs-on"))
				if err != nil {
					return nil, err
				}
				if strings.Contains(strings.ToLower(string(runner)), "windows") && !strings.Contains(string(runner), "${{") {
					shell = "pwsh"
				}
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

var (
	expressions = regexp.MustCompile(`(?s)\$\{\{.*?\}\}`)
	anyChar     = regexp.MustCompile(`.`)
	// installers matches the command words that must run under kit-retry.
	installers = regexp.MustCompile(`^(?:(?:apt|apt-get) (?:install|update|upgrade|dist-upgrade)|go (?:install|get|mod download)|(?:npm|pnpm|yarn|bun) (?:install|ci|i|add)|(?:pip|pip3) install|docker pull|apk (?:add|update)|(?:brew|cargo|pipx|dnf) install)(?: |$)`)
)

// scanScript applies the download and installer rules to executable shell calls.
func scanScript(source string) []string {
	// Filler keeps the parser away from Actions expressions and keeps line numbers.
	masked := expressions.ReplaceAllStringFunc(source, func(span string) string { return anyChar.ReplaceAllString(span, "x") })
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).Parse(strings.NewReader(masked), "")
	if err != nil {
		return nil
	}
	allowed := map[uint]bool{}
	lines := strings.Split(masked, "\n")
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
		wrapped := len(args) >= 4 && args[0] == "kit-retry"
		if wrapped {
			args = unprefix(args[3:])
		}
		if len(args) >= 3 && (args[0] == "python" || args[0] == "python3") && args[1] == "-m" && args[2] == "pip" {
			args = args[2:]
		}
		switch {
		case len(args) > 0 && (args[0] == "curl" || args[0] == "wget"):
			findings = append(findings, fmt.Sprintf("%s on script line %d: use kit-download", args[0], call.Pos().Line()))
		case !wrapped && installer(args):
			findings = append(findings, fmt.Sprintf("%s on script line %d: wrap in kit-retry", args[0], call.Pos().Line()))
		}
		return true
	})
	return findings
}

// installer stops at the first subcommand, allowing flags and their values before it.
func installer(args []string) bool {
	for i := 1; i < len(args); i++ {
		if installers.MatchString(args[0] + " " + strings.Join(args[i:], " ")) {
			return true
		}
		if !strings.HasPrefix(args[i], "-") && (!strings.HasPrefix(args[i-1], "-") || strings.Contains(args[i-1], "=")) {
			return false
		}
	}
	return false
}

// literal decodes a word's literal and quoted parts; any expansion makes it non-literal.
func literal(word *syntax.Word) string {
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			if literal(&syntax.Word{Parts: part.Parts}) == "\x00" {
				return "\x00"
			}
		default:
			return "\x00"
		}
	}
	fields, err := expand.Fields(nil, word)
	if err != nil || len(fields) != 1 {
		return "\x00"
	}
	return fields[0]
}

// unprefix normalizes executable names and skips command prefixes.
func unprefix(args []string) []string {
	for len(args) > 0 {
		args[0] = executable(args[0])
		if args[0] == "command" && len(args) > 1 && (args[1] == "-v" || args[1] == "-V") {
			return args
		}
		if args[0] != "sudo" && args[0] != "env" && args[0] != "command" && args[0] != "exec" {
			return args
		}
		args = args[1:]
		for len(args) > 0 && (strings.HasPrefix(args[0], "-") || strings.Contains(args[0], "=")) {
			args = args[1:]
		}
	}
	return args
}
