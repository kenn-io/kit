package agenthook

import (
	"errors"
	"runtime"
	"strings"
)

// Commands contains hook command lines for the current platform and Windows.
// Codex stores the Windows variant alongside the native command so the same
// config can be used from either environment.
type Commands struct {
	Native  string
	Windows string
}

// BuildCommand quotes an executable and arguments for agent hook config. It
// returns an error when executable is blank.
func BuildCommand(executable string, arguments ...string) (Commands, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return Commands{}, errors.New("agent hook executable is required")
	}
	argv := append([]string{executable}, arguments...)
	posix := make([]string, 0, len(argv))
	windows := make([]string, 0, len(argv))
	for _, arg := range argv {
		posix = append(posix, quotePOSIXArgument(arg))
		windows = append(windows, quoteWindowsArgument(arg))
	}
	commands := Commands{
		Native:  strings.Join(posix, " "),
		Windows: strings.Join(windows, " "),
	}
	if runtime.GOOS == "windows" {
		commands.Native = commands.Windows
	}
	return commands, nil
}

func resolveCommands(opts InstallOptions) (Commands, error) {
	hasExecutable := strings.TrimSpace(opts.Executable) != ""
	hasCommand := strings.TrimSpace(opts.Command) != ""
	if hasExecutable && hasCommand {
		return Commands{}, errors.New("agent hook executable and command override are mutually exclusive")
	}
	if hasExecutable {
		return BuildCommand(opts.Executable, opts.Arguments...)
	}
	if len(opts.Arguments) > 0 {
		return Commands{}, errors.New("agent hook arguments require an executable")
	}
	return Commands{Native: opts.Command, Windows: opts.CommandWindows}, nil
}

func quotePOSIXArgument(arg string) string {
	if arg != "" && strings.IndexFunc(arg, unsafePOSIXArgumentRune) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func unsafePOSIXArgumentRune(r rune) bool {
	return r != '/' && r != '.' && r != '-' && r != '_' && r != '+' && r != ':' &&
		(r < '0' || r > '9') &&
		(r < 'A' || r > 'Z') &&
		(r < 'a' || r > 'z')
}

// quoteWindowsArgument implements the CommandLineToArgvW quoting convention.
func quoteWindowsArgument(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\n\v\f\r\"") {
		return arg
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	backslashes := 0
	for _, r := range arg {
		if r == '\\' {
			backslashes++
			continue
		}
		if r == '"' {
			quoted.WriteString(strings.Repeat("\\", backslashes*2+1))
			quoted.WriteRune(r)
			backslashes = 0
			continue
		}
		quoted.WriteString(strings.Repeat("\\", backslashes))
		backslashes = 0
		quoted.WriteRune(r)
	}
	quoted.WriteString(strings.Repeat("\\", backslashes*2))
	quoted.WriteByte('"')
	return quoted.String()
}
