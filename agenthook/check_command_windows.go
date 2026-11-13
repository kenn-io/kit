package agenthook

func rawHookWords(command, commandWindows string) ([]string, error) {
	if commandWindows == "" {
		return posixHookWords(command)
	}
	return powerShellHookWords(commandWindows)
}
