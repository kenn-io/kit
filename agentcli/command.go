package agentcli

import "strings"

type optionSpec struct {
	canonical string
	values    int
	forbidden string
}

type optionGrammar map[string]optionSpec

func flag(canonical string) optionSpec {
	return optionSpec{canonical: canonical}
}

func value(canonical string) optionSpec {
	return optionSpec{canonical: canonical, values: 1}
}

func forbidden(reason string) optionSpec {
	return optionSpec{forbidden: reason}
}

func validateConfiguredOptions(agent Name, options []string, grammar optionGrammar) (map[string]bool, error) {
	configured := make(map[string]bool)
	for i := 0; i < len(options); i++ {
		token := options[i]
		if token == "--" {
			return nil, invalidCommand(agent, i, token, "prompt boundary is not a configured option", "put prompts in Request.Prompt")
		}
		name, inline, hasInline := strings.Cut(token, "=")
		if !strings.HasPrefix(name, "-") || name == "-" {
			return nil, invalidCommand(agent, i, token, "positional operands and subcommands are not allowed", "put prompts in Request.Prompt and let Start or Resume select the command shape")
		}
		spec, ok := grammar[name]
		if !ok {
			return nil, invalidCommand(agent, i, token, "unknown or ambiguous option", "use an option documented for this agent, with its value as a separate token or --option=value")
		}
		if spec.forbidden != "" {
			return nil, invalidCommand(agent, i, token, spec.forbidden, "express this through Start or Resume instead")
		}
		if hasInline {
			if spec.values != 1 || inline == "" {
				return nil, invalidCommand(agent, i, token, "option does not accept this inline value", "use the option's documented form")
			}
			configured[spec.canonical] = true
			continue
		}
		if spec.values == 0 {
			configured[spec.canonical] = true
			continue
		}
		if i+1 >= len(options) || options[i+1] == "" || options[i+1] == "--" {
			return nil, invalidCommand(agent, i, token, "option requires one value", "add the value immediately after the option")
		}
		if nextName, _, _ := strings.Cut(options[i+1], "="); grammar[nextName].canonical != "" || grammar[nextName].forbidden != "" {
			return nil, invalidCommand(agent, i, token, "option requires one value before the next option", "use --option=value when the intended value begins like a documented option")
		}
		i++
		configured[spec.canonical] = true
	}
	return configured, nil
}

func invalidCommand(agent Name, index int, token, reason, hint string) error {
	return &InvalidCommandError{Agent: agent, Token: token, Index: index, Reason: reason, Hint: hint}
}
