---
type: Guide
title: Adopting the Kenn documentation profile
description: Use the Kenn OKF metadata profile and check meaningful documentation edits.
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-26T16:15:39Z" }
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

## Sources and links

Other keys, including `sources` and `x-` extensions, are allowed and
preserved.

## What is checked

`README.md`, `AGENTS.md` and `CLAUDE.md` never take Kenn frontmatter.
The check skips unlisted skills and Markdown inside skill bundles,
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

## Skills

Kenn metadata for an Agent Skill is opt-in. First verify that the skill’s
loader and sync targets preserve nested YAML. Pass a repeatable
`-managed-skill 'skills/*/SKILL.md'` to checks and fixes to name tracked
skills. A pattern that matches no eligible skill is an error. Imported
symlinks, portable skills, and unlisted skills are never rewritten.

Keep the skill's native `name` and `description`. Do not add a document title.
The Kenn extension lives under `metadata`:

```yaml
metadata:
  type: Skill
  status: stable
  generated: { by: human:editor, at: "2026-11-17T19:00:00Z" }
```

`metadata.status` is required on a managed skill, directly under `type`.
OKF reads an absent `status` as stable, but a skill is instructions that an
agent follows, so its standing must be visible. Use one of OKF's three values:

- `draft`: not yet reviewed and possibly incomplete. Agents should use it
  with care.
- `stable`: ready to use.
- `deprecated`: kept for links and history, but not current. Agents should
  not follow it. When a replacement exists, also set `metadata.superseded_by`
  to the replacing skill's path.

`status` is lifecycle, not freshness: `metadata.generated.at` records the
last meaningful change. Writing `status: stable` on a skill that had none is
not a meaningful edit and needs no new timestamp; any other status change is.

With `-fix -by ACTOR`, the checker fills missing metadata, including a missing
`status` as `stable`, only when the resulting profile is complete.
It preserves user fields and never advances an existing valid timestamp.

A managed skill's timestamp covers its entire tracked bundle. Adding,
deleting, or meaningfully changing a resource requires a later
`metadata.generated.at` and the actual editor in `metadata.generated.by`.
Mode-only changes and Markdown formatting changes that render the same do
not count. An unmanaged nested `SKILL.md` and its resources belong to the
enclosing managed skill. Do not add Kenn frontmatter to bundle resources.

When a live tracked `SKILL.md` is at the repository root, unscoped checks
require explicitly opting in the root skill. Select ordinary document paths
for read-only checks; `-fix` does not rewrite root-bundle descendants.

## Layout and indexes

Keep the root README as the project overview and link it to `docs/index.md`.
Move a nested landing-page README to `index.md`. Organize pages in `tutorials/`
(learn), `guides/` (do), `reference/` (look up), and `explanation/`
(understand), creating sections only where content exists. No folder needs an
index, and no new `log.md` is required. Indexes are not concepts. A bundle-root
index may carry v0.3 bundle metadata; nested indexes can be plain Markdown.
Logs have no frontmatter. Kit treats `docs/` as its documentation
bundle and declares `okf_version: "0.3"` in `docs/index.md`.

A parent managed index also lists pages under a nested managed index.

A folder can opt into its own recursive list by adding these whole-line markers
to its `index.md`:

```markdown
<!-- BEGIN kennlint index -->
<!-- END kennlint index -->
```

Each entry is `- [Title](relative/path.md) - description`; only tracked files
are listed. A missing target inside the marked block makes it stale; links
outside the block are neither checked nor rewritten. Run `make fix-frontmatter`
in Kit to regenerate the list. The read-only check reports stale lists and the
Make target prints the local fix command. Unmarked indexes are left alone.
Skills, skill bundle files, deprecated pages, pages with unknown status values,
README and AGENTS files, release notes, and dot-prefixed files or directories
are never listed. Omitted status,
`stable`, and `draft` remain eligible. Index regeneration preserves all bytes
outside the marked body block, including the complete bundle metadata and
nested extension values. Resolved symlink targets obey the same exclusions
as their aliases. Symlinked indexes are never regenerated, including aliases
into imported bundles. Retain deprecated pages as linked pointers to preserve public URLs. Repeat
the original headings in each pointer so old fragment links land on a heading
that links to the new section.
