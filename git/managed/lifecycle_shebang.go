package managedworktree

import (
	"bufio"
	"io"
	"os"
	"path"
	"strings"
)

const lifecycleShebangLimit = 4096

// lifecycleShebangCommand resolves the interpreter declared by a script so
// Windows can launch scripts through CreateProcess without relying on filename
// associations. It returns interpreter arguments followed by the script path.
func lifecycleShebangCommand(script string) (string, []string, bool) {
	file, err := os.Open(script)
	if err != nil {
		return "", nil, false
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, lifecycleShebangLimit+1))
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", nil, false
	}
	if len(line) > lifecycleShebangLimit || !strings.HasPrefix(line, "#!") {
		return "", nil, false
	}
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "#!")))
	if len(fields) == 0 {
		return "", nil, false
	}
	interpreter := shebangExecutableName(fields[0])
	arguments := fields[1:]
	if strings.EqualFold(strings.TrimSuffix(interpreter, ".exe"), "env") {
		if len(arguments) > 0 && arguments[0] == "-S" {
			arguments = arguments[1:]
		}
		if len(arguments) == 0 {
			return "", nil, false
		}
		interpreter = shebangExecutableName(arguments[0])
		arguments = arguments[1:]
	}
	if interpreter == "" || interpreter == "." || interpreter == "/" {
		return "", nil, false
	}
	commandArgs := make([]string, 0, len(arguments)+1)
	commandArgs = append(commandArgs, arguments...)
	commandArgs = append(commandArgs, script)
	return interpreter, commandArgs, true
}

func shebangExecutableName(value string) string {
	return path.Base(strings.ReplaceAll(strings.TrimSpace(value), `\`, "/"))
}
