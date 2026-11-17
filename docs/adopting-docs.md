---
type: Guide
title: Adopting the Kenn documentation profile
description: Use the Kenn OKF metadata profile and check meaningful documentation edits.
generated:
  by: human:salmonumbrella
  at: "2026-11-17T21:35:03Z"
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

The check excludes README, AGENTS, CLAUDE and SKILL files at every depth,
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
files before checking them. Remove `last_edited` by hand when converting a page.
Correct malformed or invalid metadata manually.

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
