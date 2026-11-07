//go:build !windows

package agenthook

func rawHookExecutable(command, _ string) (string, error) {
	return posixHookExecutable(command)
}
