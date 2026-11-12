package agentsetup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
)

// State describes the readiness of a setup item.
type State string

const (
	StateOK        State = "ok"
	StateMissing   State = "missing"
	StateOutdated  State = "outdated"
	StateUntrusted State = "untrusted"
	StateModified  State = "modified"
	StateDisabled  State = "disabled"
	StateFailed    State = "failed"
	StateUnknown   State = "unknown"
)

// Status is the native readiness state and any action required.
type Status struct {
	State      State  `json:"state"`
	Reason     string `json:"reason,omitempty"`
	NextStep   string `json:"next_step,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	HookCount  int    `json:"hook_count,omitzero"`
	ToolCount  *int   `json:"tool_count,omitzero"`
}

// Row is one agent's setup item.
type Row struct {
	Agent     string `json:"agent"`
	AgentName string `json:"agent_name"`
	Piece     string `json:"piece"`
	Status
}

// Report holds the same rows for setup and status commands.
type Report struct {
	Rows []Row `json:"rows"`
}

// ExitCode returns zero only when every row is ready.
func (r Report) ExitCode() int {
	if slices.ContainsFunc(r.Rows, func(row Row) bool { return row.State != StateOK }) {
		return 1
	}
	return 0
}

// WriteText renders aligned rows and a readiness summary.
func (r Report) WriteText(w io.Writer, checkCommand string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	failures := 0
	for _, row := range r.Rows {
		state, detail := string(row.State), row.Reason
		if row.State == StateUntrusted || row.State == StateModified {
			state = "needs you"
		}
		switch {
		case row.State == StateOK && row.ToolCount != nil:
			detail = fmt.Sprintf("%d tools", *row.ToolCount)
		case row.State == StateOK && row.HookCount > 0:
			detail = fmt.Sprintf("%s in %s", count(row.HookCount, "hook"), homePath(row.ConfigPath))
		case row.State != StateOK:
			failures++
			if row.HookCount > 0 {
				detail = fmt.Sprintf("%s: %s", count(row.HookCount, "hook"), detail)
			}
			if row.NextStep != "" && detail != "" {
				detail += ": "
			}
			detail += row.NextStep
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", textCell(row.AgentName), textCell(row.Piece), textCell(state), textCell(detail)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	summary := "Done."
	if failures > 0 {
		summary = "Attention needed for " + count(failures, "step") + "."
	}
	if failures > 0 && checkCommand != "" {
		summary += fmt.Sprintf(" Run `%s` afterwards.", checkCommand)
	}
	_, err := fmt.Fprintln(w, summary)
	return err
}

func textCell(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ", "\v", " ", "\f", " ", "\xff", " ").Replace(s)
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func homePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && filepath.IsLocal(rel) {
		return "~/" + filepath.ToSlash(rel)
	}
	return path
}
