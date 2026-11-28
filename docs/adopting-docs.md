---
type: Guide
title: Adopting the Kenn documentation profile
description: Use the Kenn OKF metadata profile and check meaningful documentation edits.
generated: { by: claude-code/claude-opus-5.5, at: "2026-11-25T15:44:30Z" }
---

# Adopting the Kenn documentation profile

Kenn applies a documentation profile to tracked Markdown based on the
[pinned OKF v0.3 specification](https://github.com/open-knowledge-format/open-knowledge-format/blob/527e81af/SPEC.md).
OKF itself requires only `type`. Kenn also requires a nonempty `title` and
`description`, and `generated` recording the actor and time of the last
meaningful edit. Use Guide for instructions, Reference for facts and formats,
and Explanation for architecture and research.

```yaml
---
type: Guide
title: Restore from backup
description: Restore and verify a backup after a failed upgrade.
generated: { by: codex/gpt-6.2-astra, at: "2026-11-17T19:00:00Z" }
---
```

`generated.at` accepts an RFC 3339 date-time with an offset or a
`YYYY-MM-DD` date; write the actual edit time in UTC. For an agent-authored
edit, `generated.by` names the actual coding tool and model as
`<producer>/<version>`, not the person who ran or approved the agent. Use
`human:<id>` for a hand-authored edit and `process:<id>` for an automated
process. If the actor is unknown, ask rather than infer it from Git
credentials. Generation does not imply review.

An omitted `status` means stable. Use `draft` for provisional content. If a
retired page is kept, set `status: deprecated` and link to its replacement.
Other keys, including `sources` and `x-` extensions, are allowed and
preserved.

## What is checked

`README.md`, `AGENTS.md`, `CLAUDE.md` and `SKILL.md` files never take
frontmatter at any depth. The check also skips Markdown inside skill bundles,
changelogs, release notes, `docs/releases`, `.github`, `testdata`, `vendor`,
and `node_modules`. Add `-exclude GLOB` for other exceptions; `**` matches
any number of directories. `index.md` files are OKF indexes, not concepts:
any frontmatter they carry only has to parse. `log.md` must have no
frontmatter. Path arguments limit the check to tracked files; put flags
before paths.

## Fixing

`kennlint frontmatter -fix` fills absent fields it can determine and never
changes present values or the body:

- `type` from repeatable path rules such as
  `-type-rule 'docs/reference/**=Reference'`, when every matching rule agrees.
- `title` from an H1 that is the first block.
- `description` from `-description TEXT` when one file is selected.
- `generated` from `-by ACTOR` and the current UTC time, when the mapping is
  absent.

Everything else is reported for you to fix by hand. Kit wraps this as
`make fix-frontmatter BY=<actor>`.

## Meaningful edits

Run the read-only check in CI with `-changed` set to the pull request base
SHA and full Git history. When a page's metadata or rendered content changes,
`generated.at` must move later; the same actor may edit again. Formatting-only
edits (rewrapping, list markers, table padding) and changes to
`generated.by`, `generated.at`, or `verified` alone do not count. `-fix` never bumps an existing timestamp.
