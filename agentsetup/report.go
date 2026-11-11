package agentsetup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	for _, row := range r.Rows {
		if row.State != StateOK {
			return 1
		}
	}
	return 0
}

// WriteText renders aligned rows and a readiness summary.
func (r Report) WriteText(w io.Writer, checkCommand string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	failures := 0
	for _, row := range r.Rows {
		state := string(row.State)
		if row.State == StateUntrusted || row.State == StateModified {
			state = "needs you"
		}
		detail := row.Reason
		if row.State == StateOK && row.HookCount > 0 {
			detail = fmt.Sprintf("%s in %s", hookCount(row.HookCount), homePath(row.ConfigPath))
		}
		if row.State == StateOK && row.ToolCount != nil {
			detail = fmt.Sprintf("%d tools", *row.ToolCount)
		}
		if row.State != StateOK {
			failures++
		}
		if row.State != StateOK && row.HookCount > 0 {
			detail = fmt.Sprintf("%s: %s", hookCount(row.HookCount), detail)
		}
		if row.State != StateOK && row.NextStep != "" {
			if detail != "" {
				detail += ": "
			}
			detail += row.NextStep
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", row.AgentName, row.Piece, state, detail); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if failures == 0 {
		_, err := fmt.Fprintln(w, "Done.")
		return err
	}
	summary := fmt.Sprintf("Done except %d steps for you.", failures)
	if failures == 1 {
		summary = "Done except 1 step for you."
	}
	if checkCommand != "" {
		summary += fmt.Sprintf(" Run `%s` afterwards.", checkCommand)
	}
	_, err := fmt.Fprintln(w, summary)
	return err
}

func hookCount(count int) string {
	if count == 1 {
		return "1 hook"
	}
	return fmt.Sprintf("%d hooks", count)
}

func homePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "~/" + filepath.ToSlash(rel)
	}
	return path
}
