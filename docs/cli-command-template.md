# CLI command design template

Copy this worksheet for a command or command family. Replace bracketed prompts
with real application syntax and contracts; mark unsupported surfaces “not
applicable.” Use [Shared CLI design guidance](cli-design.md) for rationale.
Keep the completed design with the adopting application's maintained docs.

## Purpose and grammar

- User task and expected result: [one sentence]
- Canonical command paths: [root, group, leaf]
- Grouping decision: [related resource actions; any primary-root exceptions]
- Inputs and scope: [arguments, required flags/defaults, target/state selection]
- Identifier rules: [names/IDs/UIDs/revisions, scope, ambiguity resolution]
- Side effects and prerequisites: [what changes; what must already exist]

## Discovery and examples

| Surface | Help content or link |
| --- | --- |
| Root | [purpose, common workflow, scope, output/status/environment overview] |
| Group | [domain, available actions, shared scope, next-step example] |
| Leaf | [exact usage, required inputs/defaults, side effects, local flags] |
| Longer guidance | [quickstart/reference/schema location, when useful] |

Include the following as copyable examples beside the accepting command:

```text
[minimal actual invocation]
[complete minimal accepted request body, if applicable]
[expected result and process status]
[follow-up invocation using an identifier returned above]
```

For JSON/file/stdin inputs, state [required fields, defaults, schema discovery,
and fields provided by flags]. For workflows, include [complete body, dependency
keys, and resource creation order]. For updates/retries, explain [identifier
substitution, expected revision retrieval, conflict recovery, and idempotency].
All example values must be synthetic and internally consistent.

## Output and recovery contract

List only supported modes and their actual selectors; agent text is optional.

| Mode/selector and scope | Success stdout/stderr/status | Failure stdout/stderr/status |
| --- | --- | --- |
| [human] | [result and diagnostics] | [message and useful next action] |
| [JSON] | [fields/types or stream framing] | [envelope fields/types and status] |
| [other, if supported] | [contract] | [contract] |

For a new CLI, use the guide's recommended stderr JSON error envelope:
`error.code` (stable string), `message` (human string), optional `hint`
(actionable string), and `exit_code` (actual numeric process status).
For released commands, record the existing contract and any migration instead.
Document [exit categories/numbers, quiet behavior, progress/warnings/color,
partial-stream failures, callback exceptions, and recovery in each mode].

## Compatibility decision

Record [affected callers, compatibility window, canonical discovery/completion,
old spellings/flags, removal policy, and warning channel/suppression rules].
Include [stable telemetry/analytics identity, config selectors, permission/policy
keys, hooks/callbacks, daemon routes, and MCP names]. Name deliberate identity
changes and their consumer migrations. Check actual framework notice behavior;
if old/new commands use separate constructors, keep flag state independent.

## Review checklist

- [ ] Grammar and help make the common workflow discoverable; exceptions have reasons.
- [ ] Every visible command has an intended help group where grouping is used.
- [ ] Rendered help/quickstart commands parse; a stale example fails the check.
- [ ] Rendered request/workflow bodies pass real validation with synthetic fixtures;
      relevant create/update, dependency, identifier, and stale-revision cases pass.
- [ ] Success/error channels, fields/types, statuses, streams, and supported modes
      match the contract; quiet/callback rules and actual recovery syntax are checked.
- [ ] Affected callers and identity consumers are inventoried; old/new grammar,
      output, notices, flag state, callbacks, wire behavior, and completion are checked.
- [ ] Links, schema references, and synthetic values are current and consistent.

Evidence: [applicable checks and results; explain any not-applicable surfaces].
