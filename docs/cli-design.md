---
title: "Shared CLI design guidance"
description: "Help, output, result hints, error, exit code, and migration conventions for kenn-io CLIs, grounded in Kata, Roborev, and Docbank."
last_edited: 2026-11-09
---

# Shared CLI design guidance

A kenn-io CLI should let a person or a coding agent find the next command, run
it, and recover from a failure using only the installed binary: no README, no
skill pack, no repository checkout. Kata is the reference design. Roborev and
Docbank applied the same pattern to larger command trees and added noun
groups. This guide describes what the three share, where they differ, and what
a new CLI should copy. All three use Cobra, so examples name Cobra fields such
as `Short`, `Long`, and `Deprecated`; the conventions do not depend on it.

Start a design with the [command design template](cli-command-template.md).
Its checklist doubles as the review guide. Preserve released contracts; the
guidance below is for new commands and deliberate migrations.

## Source changes

Each convention below links to the code that implements it. These merged pull
requests introduced them:

| Change | Kata | Roborev | Docbank |
| --- | --- | --- | --- |
| Help that teaches the workflow | [#530][kata-530] | [#1582][roborev-1582] | [#1184][docbank-1184] |
| Errors with recovery hints | [#531][kata-531] | [#1582][roborev-1582] | [#1184][docbank-1184] |
| Request bodies in help | [#631][kata-631] | | |
| Commands grouped under nouns | | [#1589][roborev-1589] | [#1191][docbank-1191] |

Code links are pinned to the `main` commits current when this guide was
written, so line anchors stay valid.

## Root help is the agent's manual

An agent's first move is `<tool> --help`. Kata's root help
([source][kata-root-help]) answers, in order:

1. **What the tool is and what it acts on.** One sentence, then where commands
   send their work: "Commands talk to a kata daemon ... and act on the project
   bound to the current directory."
2. **The workflow, as copyable commands in the order an agent runs them.**
   Each line is a real invocation with placeholders; numbered comments mark
   the steps that must come first. The workflow ends by pointing to the full
   contract (`kata quickstart --format contract`).
3. **Identifiers.** Every accepted form and its scope: `abc4` in the bound
   project, `project#abc4` in any project, or the ULID. It also says what is
   rejected (legacy issue numbers).
4. **Target selection.** How the project, vault, repository, or daemon is
   chosen, and the flag or command that overrides it.
5. **Output modes and channels.** Human by default; `--agent` and `--json`
   described in one line each; errors on stderr.
6. **Exit codes.** The full numeric map on two lines.
7. **Environment variables.** Each one with its effect.

Roborev ([source][roborev-root-help]) and Docbank ([source][docbank-root-help],
[golden][docbank-root-golden]) cover the same items. Docbank moves vault
selection ahead of the workflow and adds a short `Concepts:` block for
vocabulary its commands share (`path-or-id`, `version`, `rendition`, and
others). Roborev adds `Repository and server:` because commands can target
another checkout or a remote daemon. Add a block only when commands depend on
it.

Keep root help small. Docbank's help lint caps root help at 6,500 bytes and
leaf help at 2,500 ([lint][docbank-help-lint]); grouping in #1191 cut its root
from 4,635 to 3,521 bytes, and it has since grown back to about 4,800. Long
references belong in `quickstart`, a reference doc, or a schema command,
linked from help.

## Group commands by task

Every top-level command belongs to a help heading. Kata has one main noun (the
issue), so its verbs stay at root (`kata create`, `kata close`) and help
headings group them by purpose: *Find and read work*, *Change issues*,
*Coordinate agents*, *Set up and diagnose*, and *Administer (rarely needed by
agents)* ([groups][kata-groups]). The last heading tells an agent what it can
skip.

When several actions share a resource, put them under a noun:
`<tool> <noun> <verb>`. Roborev moved 26 root commands under `job`, `agent`,
`hook`, `report`, `storage`, `signing`, and `daemon`, cutting root help from 47
entries to 27 ([moves][roborev-moves]). Docbank grouped its document, vault,
and email commands, and renamed `search-export` to `report`. Both kept their
primary workflow at root: `roborev review`, `fix`, `refine`; `docbank search`.

Rules all three follow:

- A test fails when a visible top-level command has no group, so new commands
  never land in Cobra's "Additional Commands" ([Kata][kata-help-tests],
  [Docbank][docbank-help-lint]).
- A group parent explains the domain in `Long` and shows a next-step example
  (`roborev job list`, `roborev job show --job 42`). Docbank's lint requires
  `Long` on every parent.
- A mistyped action under a group is a usage error (exit 2), never group help
  with exit 0 ([Docbank #1184][docbank-1184]).
- Use one vocabulary for shared arguments. Docbank's lint rejects old spellings
  such as `<node-id>` in favour of `<path-or-id>`.

## Leaf help says what happens and what comes back

Kata's leaf help (`kata create --help`) leads with semantics an agent cannot
guess from flag names:

```text
Create an issue in the bound project (or --project). Search first.
--idempotency-key makes retries safe for seven days: the same key returns
the original issue; the same key with different fields fails with conflict (exit 5).
...
--agent prints "OK create <ref>"; --json has .issue.short_id.
```

Copy these properties:

- **Side effects, retry, and conflict behavior**, with the exit code each
  outcome produces.
- **Where the result identifier comes from in each output mode**, so the next
  command can use it.
- **Two to four copyable examples** that use the command itself. Docbank's lint
  requires an example on every runnable leaf, at most four lines, including
  the command being documented.
- **Meanings, not only names.** `kata meta --help` lists the keys that carry
  meaning (`work.attention`, `work.branch`, `someday`) and their values.
- **Compact inherited flags.** Kata and Roborev replace Cobra's
  inherited-flags block with one line (`Global: <flags>. Details: kata
  --help`), so leaf help stays short ([template][kata-usage-template],
  [Roborev test][roborev-help-tests]).

Docbank's lint also enforces the house style: `Short` is at most 80
characters, has no trailing period, and avoids non-imperative openings such as
"Status a"; `Long` wraps at 80 columns; flag
descriptions start lowercase and have no trailing period; `--json` always reads
"print JSON ..." ([lint][docbank-help-lint]).

## Put complete request bodies beside the command that accepts them

When a command takes JSON, a file, or stdin, its help shows a complete minimal
body together with the invocation. `kata cron job create --help` lists the
required fields and defaults, explains which fields flags supply (`--parent`
sets `definition.issue.parent_uid`), and gives a body for each job kind,
followed by its command:

```text
kata cron job create --uid <job-uid> --parent <parent-ref> --file recurring.json --json
```

It also explains retries: reuse the same `--uid` after an uncertain response,
and inspect `kata cron job show` first, because a create that already
succeeded conflicts instead of turning into an update. The test extracts every
body from rendered help and the reference doc and submits it through the real
CLI and daemon validation ([bodies][kata-cron-help],
[retry help][kata-cron-retry], [test][kata-cron-tests]).
A parse check alone would miss a body the daemon rejects.

## Output modes

| Mode | Kata | Roborev | Docbank |
| --- | --- | --- | --- |
| Human | default | default | default |
| JSON | global `--json` / `--format json` | per-command `--json` | per-command `--json` |
| Agent text | global `--agent` / `--format agent` | `job search --agent` only | `search --agent` only |

Kata's model is the one to copy for a new CLI. Output mode is one global
selector; `--json` and `--agent` are aliases for `--format`, and conflicting
selectors are a usage error before the command runs. `--quiet` suppresses
output but is not a mode, and it never hides an error. `--format` is reserved
for output; Kata's import uses `--source-format` for its input type
([agent output][kata-agent-output]).

The channels are the same in all three: results on stdout; errors, hints,
progress, and warnings on stderr.

**JSON** is the complete machine API. Kata versions its success envelope
(`kata_api_version: 1`) and never mixes diagnostics into it.

**Agent mode** is optional. When you offer it, give it a written contract, as
Kata does ([contract][kata-agent-stability]):

- The first token is `OK` (stdout) or `ERR` (stderr, nonzero exit). The second
  token is the command or record kind.
- Then positional fields where documented, `key=value` elsewhere. Field order
  is part of the contract; new fields are only appended.
- Absent values are omitted, never shown as a sentinel such as `owner=null`.
- The format has a version (`kata version --agent` prints `agent_format=1`).
- An `Undo:` line appears only when the follow-up is factual. Generic `Next:`
  guidance is deliberately excluded.

Do not give `--agent` a second meaning. In Roborev, most `--agent` flags pick
an AI provider or hook profile, and root help has to explain that only
`job search --agent` selects output.

## Empty and partial results say why

When a successful result is empty or incomplete, report each measured cause
on its own stderr line in human and agent output, in the shape
`Hint: <what was left out or missing>; <next command>`. Use the CLI's
established `Hint:` or `hint:` prefix. Include a next command only when it
exists and returns the missing results. Keep stdout bytes unchanged in human,
agent, and bare-array JSON output, and keep existing JSON fields unchanged.

JSON objects carry a `hints` array with `code` and `message`, and `next` for
the next command when applicable. Do not duplicate these hints on stderr. MCP
results carry a `hints` array of objects with `code` and `message`, and
`retry_with` only when the measured arguments return the missing result. Merge
its changed arguments into the original request. Each `code` is a stable
snake_case reason identifier. A bare JSON array stays an array and carries its
hints on stderr.

Report only measured causes. Run extra counts only for empty results; complete
pages need no extra query. Hints do not change search behavior.
These codes have shipped in the following changes:

| Source | Codes |
| --- | --- |
| [AgentsView][agentsview-2797] | `excluded_one_shot`, `excluded_automated`, `excluded_children`, `excluded_combined`, `try_fts` |
| [MsgVault][msgvault-1941] vector/hybrid JSON | `more_available`, `ranking_limit_reached`, `candidate_limit_reached` |
| [Docbank][docbank-1347] | `no_searchable_text`, `indexing_pending`, `every_word_required` |
| [Roborev][roborev-1709] | `every_word_required` |
| [Kata][kata-973] | `every_word_required`, `excluded_closed`, `excluded_open` |
| [Forge][forge-1869] | `excluded_drafts`, `excluded_closed`, `excluded_default_state`, `every_word_required`, `limit_lowered` |

Add new structured success-hint codes when their app change merges.

MsgVault's `sender_name_requires_address` is historical: it shipped in
[msgvault#1936][msgvault-1936] and was removed after sender matching changed
in [msgvault#2022][msgvault-2022].

`clisearch.RenderText` currently writes its `--help` suggestion to stdout,
contrary to the channel rule above.

## Errors tell the agent what to run next

A failure names the operation and its context, then gives at most one recovery
hint written as a real command or flag. Kata #531 is the model:

- A duplicate `create` says `pass --force-new`, not `pass force_new=true`. A
  hint in request-field syntax produces a command that fails.
- An uninitialized workspace points to `kata init --project <name>`.
- An unknown flag points to that command's `--help`.
- Argument mistakes are usage or validation errors, never internal errors.
- With no useful action, omit the hint.

The hint appears in every mode: a second `hint:` line in human output, a
separate `Hint:` line in agent output, and `error.hint` in JSON. Kata tests the
same failure in all three modes ([hint tests][kata-hint-tests]); Docbank checks
that every quoted command in an emitted hint parses against the command tree
([hint check][docbank-hint-check]). On a legacy spelling, the hint teaches the
canonical path.

```text
$ kata list --agent
ERR list not_found: no .kata.toml ancestor and no git ancestor
Hint: run "kata init --project <name>" in the workspace root, or pass --project <name>
```

### JSON error envelope

All three write one JSON object to stderr and nothing to stdout:

| Field | Kata | Roborev | Docbank |
| --- | --- | --- | --- |
| Category | `kind` (`usage`, `validation`, `not_found`, `conflict`, ...) | `code` | `code` |
| Specific reason | `code`, optional (`project_not_initialized`) | | |
| Message | `message` | `message` | `message` |
| Hint | `hint`, omitted when empty | `hint`, always present | `hint`, when present |
| Exit status | `exit_code` | `exit_code` | `exit` |
| Details | `data`, optional | | |

Sources: [Kata][kata-json-error], [Roborev][roborev-errors],
[Docbank][docbank-json-error].

For a new CLI, use Kata's shape. `kind` gives scripts a small, stable category
that tracks the exit code. `code` adds a specific reason without growing the
category list. `exit_code` repeats the process status for consumers that only
see the stream.

```json
{"error":{"kind":"not_found","code":"project_not_initialized","hint":"run \"kata init --project <name>\" in the workspace root, or pass --project <name>","message":"no .kata.toml ancestor and no git ancestor","exit_code":4}}
```

Make the envelope the only JSON on stderr. Roborev buffers lifecycle
diagnostics in JSON mode and drops them on failure
([test][roborev-json-buffer]).
Docbank instead lets previews and progress precede a final error record, and
documents it. Its flag-parse failures stay human text, because they happen
before `--json` is read. If your CLI cannot avoid either case, document it in
root help as Docbank does.

### Exit codes

| Code | Kata | Roborev | Docbank |
| --- | --- | --- | --- |
| 0 | ok | success | ok |
| 1 | internal | runtime failure, or a completed FAIL review | operational or internal |
| 2 | usage | usage | usage or request validation |
| 3 | validation | export cursor from another database | not found |
| 4 | not found | invalid user-supplied value | stale revision |
| 5 | conflict | | busy or locked |
| 6 | confirmation required | | integrity |
| 7 | daemon unavailable | | claim required |
| 8 | wait timeout | | |

Only 0, 1, and 2 agree across the three. A released CLI keeps its map. A new
CLI should start from Kata's numbering and add its own codes after 8, so an
agent that knows one kenn-io CLI can read another. Print the map in root help
and repeat it as `exit_code` in the JSON envelope.

### Crashes

An unrecovered Go panic exits 2, the same status this map gives usage errors.
The [runtime hardcodes that status](https://github.com/golang/go/blob/go1.27.3/src/runtime/panic.go#L1508);
no setting remaps it to exit 1. In `main`, defer a recover that emits the normal
error envelope to stderr in the active output mode: `kind: "internal"`, a hint
to report the bug, and `exit_code: 1`. Then exit 1:

```go
func main() {
	defer func() {
		if recover() != nil {
			emitError(outputModeFromArgs(os.Args[1:]), Error{
				Kind: "internal", Message: "internal error",
				Hint: "report this bug", ExitCode: 1,
			})
			os.Exit(1)
		}
	}()
	os.Exit(run())
}
```

`outputModeFromArgs` reads the output selectors from the raw arguments, because
a panic can happen before flag parsing has chosen a mode. Kata tests a panic
during Cobra initialization for this reason. `emitError` is the CLI's existing
human, agent, or JSON error renderer.

Keep the panic value and request data out of the error record: they can hold
credentials. A bug report still needs the trace. If the CLI has a state or log
directory, write `debug.Stack()` to a file there and name that file in the
hint. Kata's CLI drops the trace.

Kata [#819](https://github.com/kenn-io/kata/blob/af584bf321abc89e95a2efb3233bf4ff01ba718d/cmd/kata/main.go#L182-L201) implements this rule for
the CLI's main goroutine, with tests for human, agent, and JSON output; the
foreground daemon keeps its existing panic diagnostics.

[`recover` only covers the goroutine that panicked](https://go.dev/ref/spec#Handling_panics).
Goroutines a command starts should recover and return an internal error to the
caller for the same rendering, or the CLI must document that those crashes
still exit 2. For agents, the envelope's `kind` is the contract; the exit code
is a coarse hint. In JSON or agent mode, exit 2 with no error record means a
crash, not a usage error.

## Migrate released commands deliberately

Roborev #1589 and Docbank #1191 moved dozens of commands without breaking
callers. The shared approach:

- Every old spelling, with its flags and subcommands, stays callable for at
  least one release ([Roborev policy][roborev-migration]).
- Old spellings are hidden from help and completion; help, quickstart,
  bundled skills, and docs show only the new path.
- Flags, JSON records, exit codes, HTTP routes, and MCP tool names keep their
  contracts.
- The deprecation notice goes to stderr, never into JSON or streamed output.
  Both projects print their own notice instead of using Cobra's `Deprecated`
  field ([Roborev][roborev-legacy-warning],
  [Docbank][docbank-legacy-warning]). That field prints through the command's
  output writer, which is stdout once a CLI calls `SetOut(stdout)`, as Docbank
  does; Roborev also needs control over when the notice appears.
- Telemetry and hook behavior key on a stable command identity, not the
  spelling the caller typed ([Roborev][roborev-stable-path]). Check config
  selectors and permission keys that embed command paths the same way.

They differ on when to warn. Roborev warns after the command succeeds,
including `--help`, and skips the notice for `--quiet` and installed hook
callbacks. Docbank warns before parsing, so the notice also appears on
argument and flag errors. Choose one, write it down, and test it.

They also differ on flag state. Roborev builds old and new spellings from
separate constructors, so their flag values and completion state stay
independent ([Roborev][roborev-moves]). Docbank clones each legacy command and
deliberately shares flag values with the new handler. Either works; record
which one you use.

Inventory callers before moving anything: scripts, shell completions,
installed hooks, bundled skills, daemon routes, and MCP names.

## Test the help, not just the code

Agents copy help text verbatim, so a stale example breaks them. These checks
are cheap and catch real regressions:

| Check | Example |
| --- | --- |
| Every command line in help, examples, and quickstart parses: the path exists, the flags exist, and the argument count is accepted. Nothing runs. | [Kata][kata-help-tests], [Roborev][roborev-help-tests], [Docbank][docbank-help-lint] |
| The parser rejects known-stale examples, so the check cannot pass vacuously. | [Kata][kata-stale-test] |
| Every visible top-level command has a help group. | [Kata][kata-help-tests], [Docbank][docbank-help-lint] |
| Request bodies in help pass real daemon validation. | [Kata][kata-cron-tests] |
| Error hints appear in human, agent, and JSON modes. | [Kata][kata-hint-tests], [Roborev][roborev-errors-tests] |
| Recovery commands quoted in hints parse. | [Docbank][docbank-hint-check] |
| Root help matches a golden file; help stays within byte and line budgets. | [Docbank][docbank-help-lint] |
| Old and new spellings match on output, exit codes, wire calls, help, and completion. | [Roborev][roborev-grammar-tests], [Docbank][docbank-groups-tests] |

Use synthetic fixtures for every check. Never run documentation examples
against real accounts or data.

<!-- Pull requests -->
[agentsview-2797]: https://github.com/kenn-io/agentsview/pull/2797
[msgvault-1941]: https://github.com/kenn-io/msgvault/pull/1941
[msgvault-1936]: https://github.com/kenn-io/msgvault/pull/1936
[msgvault-2022]: https://github.com/kenn-io/msgvault/pull/2022
[docbank-1347]: https://github.com/kenn-io/docbank/pull/1347
[roborev-1709]: https://github.com/kenn-io/roborev/pull/1709
[kata-973]: https://github.com/kenn-io/kata/pull/973
[forge-1869]: https://github.com/kenn-io/forge/pull/1869
[kata-530]: https://github.com/kenn-io/kata/pull/530
[kata-531]: https://github.com/kenn-io/kata/pull/531
[kata-631]: https://github.com/kenn-io/kata/pull/631
[roborev-1582]: https://github.com/kenn-io/roborev/pull/1582
[roborev-1589]: https://github.com/kenn-io/roborev/pull/1589
[docbank-1184]: https://github.com/kenn-io/docbank/pull/1184
[docbank-1191]: https://github.com/kenn-io/docbank/pull/1191

<!-- Kata at 585e82c078893bbfad4114d429cbd694e335617a -->
[kata-root-help]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/help_root.go#L108-L134
[kata-groups]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/help_root.go#L11-L31
[kata-usage-template]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/help_root.go#L159
[kata-help-tests]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/help_test.go
[kata-stale-test]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/help_test.go#L49-L69
[kata-agent-output]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/docs/reference/agent-output.md#selecting-the-mode
[kata-agent-stability]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/docs/reference/agent-output.md#stability-contract
[kata-json-error]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/main.go#L273-L301
[kata-hint-tests]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/error_hints_test.go
[kata-cron-help]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/cron_help.go
[kata-cron-retry]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/cron.go#L155-L166
[kata-cron-tests]: https://github.com/kenn-io/kata/blob/585e82c078893bbfad4114d429cbd694e335617a/cmd/kata/cron_help_test.go

<!-- Roborev at 09e5809a3f6f9cffb1959b0e888c9646cf50b9dd -->
[roborev-root-help]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/help_root.go#L116-L170
[roborev-errors]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/errors.go#L165-L218
[roborev-errors-tests]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/errors_test.go
[roborev-json-buffer]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/errors_test.go#L158
[roborev-help-tests]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/help_commands_test.go
[roborev-moves]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/grammar.go#L15-L47
[roborev-stable-path]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/grammar.go#L113-L122
[roborev-legacy-warning]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/grammar.go#L124-L138
[roborev-grammar-tests]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/cmd/roborev/grammar_test.go
[roborev-migration]: https://github.com/kenn-io/roborev/blob/09e5809a3f6f9cffb1959b0e888c9646cf50b9dd/docs/commands.md?plain=1#L53

<!-- Docbank at de5bcd5c543513e1bbd04c8fbd015ac961ebce12 -->
[docbank-root-help]: https://github.com/kenn-io/docbank/blob/de5bcd5c543513e1bbd04c8fbd015ac961ebce12/cmd/docbank/root.go#L25-L68
[docbank-json-error]: https://github.com/kenn-io/docbank/blob/de5bcd5c543513e1bbd04c8fbd015ac961ebce12/cmd/docbank/main.go#L83-L88
[docbank-root-golden]: https://github.com/kenn-io/docbank/blob/de5bcd5c543513e1bbd04c8fbd015ac961ebce12/cmd/docbank/testdata/help/root.golden
[docbank-help-lint]: https://github.com/kenn-io/docbank/blob/de5bcd5c543513e1bbd04c8fbd015ac961ebce12/cmd/docbank/help_lint_test.go
[docbank-hint-check]: https://github.com/kenn-io/docbank/blob/de5bcd5c543513e1bbd04c8fbd015ac961ebce12/cmd/docbank/help_lint_test.go#L481-L489
[docbank-legacy-warning]: https://github.com/kenn-io/docbank/blob/de5bcd5c543513e1bbd04c8fbd015ac961ebce12/cmd/docbank/command_groups.go#L13-L36
[docbank-groups-tests]: https://github.com/kenn-io/docbank/blob/de5bcd5c543513e1bbd04c8fbd015ac961ebce12/cmd/docbank/command_groups_test.go
