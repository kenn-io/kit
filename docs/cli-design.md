# Shared CLI design guidance

A CLI should help people discover the next command and let scripts depend on
its results. Use these conventions when designing a command or improving an
existing command family. Preserve released contracts and document deliberate
exceptions. These are design recommendations, independent of any CLI framework.

Start with the [command design template](cli-command-template.md). Fill it in
for the command family being changed; its checklist is also the review guide.

## Organize commands around the user's task

Prefer `<tool> <domain-noun> <verb>` when several actions operate on the same
resource. A group such as `job` makes related actions easier to find than a
growing list of unrelated root verbs. Use consistent resource names and avoid
grammar where an identifier can be mistaken for a subcommand.

Keep clear, frequent primary workflows and conventional utilities at root when
that makes them easier to discover. Roborev groups job management while keeping
its primary `review` and `fix` workflows at root
([grouping change](https://github.com/kenn-io/roborev/pull/1589)).
Choose nesting for the task, without a fixed command-count or depth threshold.

Help headings and parser nesting solve different problems. Kata organizes
root help by purpose while retaining direct issue verbs
([root help][kata-help]). A heading need not introduce another command level.

## Make root, group, and leaf help useful

| Surface | What a reader should learn |
| --- | --- |
| Root | Purpose, common workflow, target/state selection, identifier scope, output modes, exit categories, and relevant environment variables. |
| Group | Domain, available actions, shared scope, and an example pointing to the next command. |
| Leaf | Exact usage, required arguments, flags/defaults, side effects, accepted inputs, examples, and recovery or reference links. |

Put the common path first. Keep inherited flags compact in leaf help and point
back to full global descriptions. Link long references and schemas from the
accepting command. A quickstart is useful when setup and several commands form
a workflow; a small CLI may need only a few examples in help.

The following `examplectl` commands, fields, identifiers, responses, and statuses
are entirely fictitious. They illustrate documentation structure, not a kit
executable or any existing application's grammar. No service is implied.

```sh
examplectl --help
examplectl job --help
examplectl job create --help
```

Show how to carry identifiers through a workflow, including their scope and
where they come from. Distinguish a local identifier, a durable UID, a name,
and a revision if the application accepts more than one kind of reference.

## Put complete request examples beside accepting commands

For JSON, file, or stdin inputs, give a complete minimal body and its command
together. Name required fields and defaults; explain any fields supplied by
flags. Link the current schema or reference through the application's actual
discovery surface. Add schema commands or flags only when the app provides them.

For the fictitious `examplectl job create`, save this body as `request.json`:

```json
{"name":"demo","enabled":false}
```

```sh
examplectl job create --file request.json --json
```

Illustrative success on stdout, with process status 0:

```json
{"id":"demo-01","name":"demo","enabled":false}
```

Use the returned identifier in the follow-up command:

```sh
examplectl job show demo-01 --json
```

For workflows, show dependency references and the creation order of resources
that refer to one another. For example, this fictitious `workflow.json` defines
two steps; `finish` depends on the earlier key `prepare`:

```json
{
  "name": "demo",
  "steps": [
    {"key": "prepare", "command": "printf 'ready\\n'"},
    {"key": "finish", "command": "printf 'done\\n'", "after": ["prepare"]}
  ]
}
```

```sh
examplectl workflow create --file workflow.json --json
```

Explain which sample identifiers need replacing. For updates, show how to get
the expected revision and what to do after a conflict. Describe retry and
idempotency behavior; a create retry and an update retry may differ.
Kata's cron help pairs create/update commands with complete bodies and tests
them through its CLI and daemon validation ([request examples][cron-help],
[tests][cron-tests]).

## Define output and error contracts

For new CLIs, put success data on stdout and diagnostics on stderr. Document
each supported mode, its selector, and whether that selector is global or local.
Human output should be readable; JSON fields and types should be dependable.
An optional concise agent mode can offer stable, scannable text, but a third
mode is not required. Do not assume `--agent` selects output: Kata uses it that
way, while Roborev also uses it to select providers or hook profiles
([Kata][kata-help], [Roborev][roborev-help]).

For a new single-result JSON command, recommend this default:

| Outcome | stdout | stderr | Process status |
| --- | --- | --- | --- |
| Success | One JSON result | Diagnostics, if any | 0 |
| Failure | Empty | One JSON error envelope | Documented nonzero status |

For this new single-result default, failure stderr must parse as the envelope
alone. Buffer early diagnostics and emit them only on success; on failure,
discard them and emit no warnings or prose before or after the envelope.
Roborev buffers JSON-mode lifecycle/warning diagnostics this way
([error routing][roborev-errors]).

The recommended envelope has `error.code` (stable string), `message` (human
string), optional `hint` (actionable string), and `exit_code` (the actual numeric
process status). For a separate fictitious empty-name input, `invalid-request.json`
contains `{"name":"","enabled":false}`. Its illustrative validation failure has
process status 4:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "Cannot create job: name must not be empty.",
    "hint": "Set name in invalid-request.json, then rerun examplectl job create --file invalid-request.json --json.",
    "exit_code": 4
  }
}
```

Both source CLIs emit stderr error envelopes with different field semantics:
Roborev's `code` is a category and `hint` is present even when empty; Kata uses
`kind` as the category, an optional more-specific `code`, omits empty `hint`, and
may include `data` ([Kata][kata-errors], [Roborev][roborev-errors]). Preserve a
released app's envelope, channels, and numeric map. Document usage, validation,
not-found/conflict, and runtime failure without prescribing universal numbers:
Kata uses 3 for validation; Roborev uses 4, with 3 for export-cursor reset.

Make errors name the failed operation and useful context. Give a short recovery
hint using real CLI syntax, not API field names disguised as flags. Point usage
errors to the relevant command's `--help`; omit hints when no useful action is
known. Keep the same recovery information in each supported output mode.
Kata tests both hint content and real flag syntax ([hint tests][hint-tests]).

Keep progress, warnings, color, and migration notices out of machine-result
stdout. When stderr is a structured error channel, do not mix prose into the
error envelope. Specify quiet behavior and callback exceptions. For streaming
commands, document framing, partial output on failure, and terminal status;
the single-result table does not describe a partially emitted stream.

## Migrate released surfaces deliberately

Inventory affected callers before moving a command or changing a flag. Choose
and publish a compatibility window and removal policy. Hidden compatibility
invocations can keep old callers working while help and completion advertise
the canonical path. If separate constructors serve old and new paths, keep
their flag values independent.

Test the framework's real alias and deprecation behavior. In Cobra v1.10.2,
`Deprecated` prints before execution via `Printf` and `OutOrStderr`: stderr by
default, but stdout when `SetOut` directs output there ([Cobra source][cobra]).
It does not automatically follow an app's quiet/callback policy. Roborev's
custom notice goes to stderr after success and is suppressed for quiet commands
and hook callbacks ([implementation][grammar]). Choose explicit warning and
suppression rules that preserve machine and callback contracts.

A command's spelling may also be its identity in telemetry/analytics, config
selectors, and permission or policy keys. Preserve those identities when only
the public spelling changes, or explicitly migrate their consumers. Include
shell scripts, completions, installed hooks, daemon routes, and MCP names in
the caller inventory. Roborev uses a stable command-path mapping and checks
old/new behavior ([compatibility tests][grammar-tests]). Its promise of at
least one release is an application choice, not a required duration here
([migration policy][migration-policy]).

## Verify examples and contracts

Test rendered help and quickstart examples against the command parser: command
paths, flags, and argument counts must remain valid. Include a stale-example
case so the check cannot pass without inspecting examples. Where help uses
groups, check that every visible command has an intended help group. Kata
demonstrates these checks ([help tests][help-tests]).

Parsing alone proves neither request validity nor runtime success. Extract
JSON bodies from rendered help and validate them through the real request path
with synthetic fixtures. Cover required fields, identifier substitution,
workflow dependencies, create/update variants, and stale revisions as relevant.
Check output channels, structured errors, statuses, and migration parity too.
Use the [template checklist](cli-command-template.md#review-checklist) to record
the applicable evidence; never run documentation examples against real accounts.

## Source examples

These merged changes provide observed examples, not a framework requirement:
[Roborev #1589](https://github.com/kenn-io/roborev/pull/1589) (command grouping),
[Roborev #1582](https://github.com/kenn-io/roborev/pull/1582) (help and recovery),
[Kata #530](https://github.com/kenn-io/kata/pull/530) (help discovery),
[Kata #531](https://github.com/kenn-io/kata/pull/531) (error hints), and
[Kata #631](https://github.com/kenn-io/kata/pull/631) (validated request examples).
The links below pin the source used for this guidance.

[grammar]: https://github.com/kenn-io/roborev/blob/81478e28b1ee5cb1654a36c45fb7e90a4dce29e1/cmd/roborev/grammar.go
[cobra]: https://github.com/spf13/cobra/blob/88b30ab89da2d0d0abb153818746c5a2d30eccec/command.go#L910
[grammar-tests]: https://github.com/kenn-io/roborev/blob/81478e28b1ee5cb1654a36c45fb7e90a4dce29e1/cmd/roborev/grammar_test.go
[migration-policy]: https://github.com/kenn-io/roborev/blob/81478e28b1ee5cb1654a36c45fb7e90a4dce29e1/docs/commands.md?plain=1#L53
[roborev-help]: https://github.com/kenn-io/roborev/blob/81478e28b1ee5cb1654a36c45fb7e90a4dce29e1/cmd/roborev/help_root.go
[roborev-errors]: https://github.com/kenn-io/roborev/blob/9f6326c493b74c6cb960161a87fa9707b6a4234c/cmd/roborev/errors.go
[kata-help]: https://github.com/kenn-io/kata/blob/efdd3e7e0054635a25795a209dc91935eacb98a6/cmd/kata/help_root.go
[help-tests]: https://github.com/kenn-io/kata/blob/efdd3e7e0054635a25795a209dc91935eacb98a6/cmd/kata/help_test.go
[kata-errors]: https://github.com/kenn-io/kata/blob/aaba4f9e9ece51b136c0ce8e0c158080248e6947/cmd/kata/main.go
[hint-tests]: https://github.com/kenn-io/kata/blob/aaba4f9e9ece51b136c0ce8e0c158080248e6947/cmd/kata/error_hints_test.go
[cron-help]: https://github.com/kenn-io/kata/blob/77a3c205dce07cf915c806a2912e88eb310ee237/cmd/kata/cron_help.go
[cron-tests]: https://github.com/kenn-io/kata/blob/77a3c205dce07cf915c806a2912e88eb310ee237/cmd/kata/cron_help_test.go
