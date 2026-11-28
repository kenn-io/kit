package agenthook

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed omp_extension.js
var ompExtension string

func ompProfile() profileSpec {
	spec := newProfileSpec(Profile{
		Agent: AgentOmp, DisplayName: "omp", ConfigFilename: filepath.Join("extensions", "agenthook-omp.js"),
		SupportedEvents: []Event{EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventPostToolUse, EventPostToolUseFailure, EventStop, EventSessionEnd},
	}, formatScript, "bash", ompAgentDir)
	spec.script = ompExtension
	spec.scriptMatcherEvents = []Event{EventPreToolUse, EventPostToolUse, EventPostToolUseFailure}
	spec.responseFormat = responseOmp
	spec.sessionSourceRequirement, spec.sessionEndReasonRequirement = inputRequired, inputRequired
	spec.userPromptRequirement = inputOptional
	return spec
}

var ompProfileName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var ompReservedName = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\..*)?$`)

func ompName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || name == "default" {
		return "", nil
	}
	if !ompProfileName.MatchString(name) || strings.HasSuffix(name, ".") || ompReservedName.MatchString(name) {
		return "", fmt.Errorf("invalid omp profile %q", raw)
	}
	return name, nil
}

// OMP resolves opaque overrides and the inherited-profile exception as in:
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/utils/src/dirs.ts#L43-L137
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/utils/src/dirs.ts#L486-L523
func ompAgentDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	rootName := os.Getenv("PI_CONFIG_DIR")
	if rootName == "" {
		rootName = ".omp"
	}
	root := filepath.Join(home, rootName)
	raw, ok := os.LookupEnv("OMP_PROFILE")
	if !ok {
		raw = os.Getenv("PI_PROFILE")
	}
	name, err := ompName(raw)
	if err != nil {
		return "", err
	}
	if name != "" {
		return filepath.Join(root, "profiles", name, "agent"), nil
	}
	override := os.Getenv("PI_CODING_AGENT_DIR")
	legacy, _ := ompName(os.Getenv("PI_PROFILE"))
	if legacy != "" && override == filepath.Join(root, "profiles", legacy, "agent") {
		override = ""
	}
	if override != "" {
		return filepath.Abs(override)
	}
	return filepath.Join(root, "agent"), nil
}
