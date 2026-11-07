# CLI command design template

Copy this worksheet for a command or command family. Replace bracketed prompts
with the real syntax and contracts, and mark unsupported surfaces "not
applicable." [Shared CLI design guidance](cli-design.md) explains each section
and links to Kata, Roborev, and Docbank code that implements it. Keep the
completed worksheet with the adopting application's docs.

## Purpose and grammar

- User task and expected result: [one sentence]
- Canonical command paths: [root, group, leaf]
- Help group or noun: [the root help heading this command belongs to, or the
  noun it sits under; why a primary workflow command stays at root]
- Inputs and scope: [arguments, required flags, defaults, how the target
  project, vault, repository, or daemon is selected]
- Identifiers: [every accepted form (short ID, qualified ID, ULID, path, name,
  revision), its scope, and which forms are rejected]
- Side effects, retries, and conflicts: [what changes; idempotency key or UID
  behavior; the exit code for each conflict]

## Help

| Surface | Content |
| --- | --- |
| Root | [purpose and target; numbered workflow; identifiers; target selection; output modes; exit codes; environment] |
| Group | [domain in `Long`; actions; a next-step example] |
| Leaf | [side effects and retry semantics; where the result ID appears in each mode; 2 to 4 examples that use this command] |
| Longer guidance | [quickstart, reference doc, or schema command, linked from help] |

For JSON, file, or stdin input, put a complete minimal body beside the
command that accepts it:

```text
[complete minimal request body]
[invocation that submits it]
[result and exit status]
[follow-up invocation that uses the returned identifier]
```

State [required fields, defaults, fields supplied by flags, dependency keys and
creation order for workflows, and how to retry an uncertain create or a stale
update]. Use synthetic, internally consistent values.

## Output and errors

| Mode and selector | Success | Failure |
| --- | --- | --- |
| Human (default) | [stdout result] | [stderr message, then `hint:` line] |
| JSON (`--json` / `--format json`) | [stdout envelope and version field] | [stderr envelope, empty stdout] |
| Agent (`--agent` / `--format agent`), optional | [`OK <command> ...` on stdout] | [`ERR <command> <kind>: ...`, then `Hint:` line] |

- JSON error envelope for a new CLI: `error.kind` (category), optional
  `error.code` (specific reason), `error.message`, `error.hint` (omitted when
  empty), `error.exit_code`, optional `error.data`. A released CLI records
  its existing envelope instead.
- Exit codes: [the map; a new CLI starts from Kata's 0 to 8 and adds codes
  after 8].
- Recovery hints: [each expected failure, its exit code, and the real command
  or flag the hint names; failures with no useful hint].
- Agent mode, if offered: [field order, omitted values, format version].
- Stderr rules: [progress, warnings, color, quiet behavior, partial output on
  a failed stream, and hook-callback exceptions].

## Compatibility

Record [affected callers: scripts, completions, installed hooks, bundled
skills, daemon routes, MCP names], [old spellings and flags, and the release
that removes them], [whether the deprecation notice prints before parsing or
after success, and when it is suppressed], and [stable identities for
telemetry, hooks, config selectors, and permission keys]. Record whether
old and new spellings share flag values or use separate constructors.

## Review checklist

- [ ] Every visible top-level command has a help group; primary workflow
      commands at root have a stated reason.
- [ ] Root help covers purpose, workflow, identifiers, target selection, output
      modes, exit codes, and environment, within the help byte budget.
- [ ] Every command in help, examples, quickstart, and emitted hints parses,
      and a known-stale example fails the check.
- [ ] Request bodies shown in help pass real validation with synthetic
      fixtures, including create, update, retry, and dependency cases.
- [ ] Each expected error has the right exit code and the same hint in human,
      agent, and JSON modes; stdout is empty on failure.
- [ ] A panic in `main`'s goroutine emits `kind: "internal"`, a report-bug hint,
      and `exit_code: 1` in the active mode, then exits 1. Command goroutines
      return recovered errors to the caller, or document crashes that exit 2.
- [ ] A mistyped group action exits 2.
- [ ] Old and new spellings match on output, exit codes, wire calls, help, and
      completion; the notice goes to stderr per the recorded rule.

Evidence: [checks run and results; why any surface is not applicable].
