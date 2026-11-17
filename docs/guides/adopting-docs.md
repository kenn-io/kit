---
type: Guide
title: Adopting the Kenn documentation profile
description: Use the Kenn OKF metadata profile and check meaningful documentation edits.
generated:
  by: human:salmonumbrella
  at: "2026-11-17T22:20:21Z"
---

# Adopting the Kenn documentation profile

Kenn repositories use the OKF documentation profile for tracked Markdown.
Each page has nonempty string `type`, `title`, and `description` fields, plus
`generated.by` identifying its actor (for example `human:salmonumbrella`) and
`generated.at` with the actual edit time in UTC, in RFC 3339 format ending in
`Z`. Use Guide for instructions, Reference for facts and formats, and
Explanation for architecture and research. Extra metadata is allowed.

The optional top-level `status` is `stable`, `draft`, or `deprecated`. An
omitted status means stable; explicitly marking a page stable does not mean it
has been independently verified. Use draft for intentionally provisional
content. If a retired page is retained, set `status: deprecated` and link to
its replacement when one exists. The check validates values when present; it
cannot infer retirement from a filename or age. The fixer never sets or changes
status.

`README.md`, `AGENTS.md`, and `CLAUDE.md` files never take frontmatter at any
depth. The check also excludes `index.md`, `log.md`, unlisted `SKILL.md` files,
release notes, changelogs, `.github`, `testdata`, `vendor`, and `node_modules`.
It also skips all Markdown inside skill bundles. Add `-exclude GLOB` for other
exceptions. Globs use `**` for any number of directories. Path arguments limit
the check to tracked files; put flags before paths.

Configure repeatable path rules such as `-type-rule 'docs/reference/**=Reference'`.
Rules must agree where they overlap. Run `kennlint frontmatter -fix` with
those rules and `-by human:your-login` to fill missing fields. When it adds
`generated.at`, the fixer stamps the current UTC time. Converting a page records
the converting actor and time in `generated`; its earlier history remains in
Git. The fixer takes an H1 title only when it is the first block; supply a
missing description with
`-description 'Text'` for a single selected file. It preserves valid values
and the Markdown body and reports metadata it cannot determine. Track new
files before checking them. When it completes a conversion and adds generated
provenance, the fixer removes `last_edited`. Correct malformed or invalid
metadata manually.

Run the read-only check in CI with `-changed` set to the pull request base SHA
and a full Git history. A meaningful content or metadata edit must change
`generated.at` to a later edit time and set `generated.by` to the editor; the
same actor may edit again. The actor is advisory: the check verifies the
timestamp, not who made the edit. Formatting-only changes do not require a
timestamp change. Fix a stale timestamp by hand; `-fix` never bumps an existing
timestamp. Kit runs the read-only `make lint-frontmatter` or
`make lint-frontmatter-changed` check. On failure the Make target prints
`fix locally: make fix-frontmatter`; that target runs the fixer with Kit’s
rules, and `BY=human:<login>` supplies the actor. Other repositories can use
the same check and fix target pattern. CI does not write files.

Repositories migrating after Kit can temporarily pass `-legacy` to retain the
`last_edited` profile. Remove this bridge as each repository adopts the Kenn
OKF profile; it is not an alternative standard.

## Skills

List Kenn-managed skills with repeatable `-managed-skill 'skills/*/SKILL.md'`
only when their loaders and sync targets preserve nested YAML mappings. This
profile is a Kenn extension, not strict Agent Skills metadata, which allows
only string values. Portable, third-party, and imported skills stay unlisted
and untouched; symlinked skill files and all bundle resources, assets,
templates, and fixtures are skipped. A tracked repository-root `SKILL.md` makes
the repository root its bundle, whether or not the skill is listed.

Keep native `name` and `description` fields. The managed profile adds:

```yaml
metadata:
  type: Skill
  generated:
    by: human:alex
    at: "2026-11-17T19:00:00Z"
  status: deprecated # only when the skill has been retired
```

Omitted `metadata.status` means stable; use draft for provisional skills. If a
retired skill is retained, set `metadata.status: deprecated` and link to its
replacement when one exists. The fixer leaves status and native fields alone.
The `-fix` option re-encodes the header, preserving native values while their
layout, such as indentation and folded scalars, may change.

Under `-changed`, meaningful skill edits, including status changes, require
updating `metadata.generated.at` to a later edit time and
`metadata.generated.by` to the editor.

## Layout and indexes

Keep the root README as the project overview and link it to `docs/index.md`.
Move a nested landing-page README to `index.md`. Organize pages in `tutorials/`
(learn), `guides/` (do), `reference/` (look up), and `explanation/`
(understand), creating sections only where content exists. No folder needs an
index, and no new `log.md` is required. Indexes and logs have no concept header.

A folder can opt into its own recursive list by adding these whole-line markers
to its `index.md`:

```markdown
<!-- BEGIN kennlint index -->
<!-- END kennlint index -->
```

Each entry is `- [Title](relative/path.md) - description`; only tracked files
are listed. Run `make fix-frontmatter` in Kit to regenerate it; the read-only
check reports stale lists and the Make target prints the local fix command. Unmarked indexes are left alone. Skills, skill bundle
files, deprecated pages, README and AGENTS files, and release notes are never
listed. Retain deprecated pages as linked pointers to preserve public URLs. Repeat
the original headings in each pointer so old fragment links land on a heading
that links to the new section.
