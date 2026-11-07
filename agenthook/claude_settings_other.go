//go:build !unix

package agenthook

func claudeRootSettingsOwned(string) bool { return false }
