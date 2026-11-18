---
type: Guide
title: Adopting the Kenn documentation profile
description: Use the Kenn OKF metadata profile and check meaningful documentation edits.
generated: { by: codex, at: 2026-11-18T11:16:46Z }
---

# Adopting the Kenn documentation profile

Kenn repositories use the OKF documentation profile for tracked Markdown.
Each page has nonempty string `type`, `title`, and `description` fields, plus
`generated.by` identifying its actor (for example `human:salmonumbrella`) and
`generated.at` with the actual edit time in UTC, in RFC 3339 format ending in
`Z`. Use Guide for instructions, Reference for facts and formats, and
Explanation for architecture and research. Extra metadata is allowed.
Write provenance on one line: `generated: { by: codex, at: 2026-11-18T10:00:00Z }`.
The fixer emits a flow mapping; equivalent existing block mappings remain valid.

The optional top-level `status` is `stable`, `draft`, or `deprecated`. An
omitted status means stable; explicitly marking a page stable does not mean it
has been independently verified. Use draft for intentionally provisional
content. If a retired page is retained, set `status: deprecated` and link to
its replacement when one exists. The check validates values when present; it
cannot infer retirement from a filename or age. The fixer never sets or changes
status.

`README.md`, `AGENTS.md`, and `CLAUDE.md` files never take frontmatter at any
depth. The check also excludes `SKILL.md` files, release notes, changelogs,
`.github`, `testdata`, `vendor`, and `node_modules`.
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

## Sources and links

`sources` is optional. When present, it is a sequence of mappings, each with a
nonempty string `resource`. A resource can be a URL, a path, or an opaque scope
description such as `all queries in project X`. Optional string `id` values
must be nonempty and unique within the list. Other source fields are allowed.

```yaml
sources:
  - id: api-guide
    resource: references/api.md
  - resource: all queries in project X
```

For per-claim attribution, use a Markdown footnote with the source ID as its
label, for example `[^api-guide]`, and supply the ordinary footnote definition.
The declared ID links the note to its source. OKF uses the same syntax as
ordinary footnotes: an unknown label cannot reliably be identified as a
misspelled source ID. The check therefore validates declared source IDs, not
unmatched footnote labels, and does not require every source to be cited.

No sources or backlinks are required. Validation never opens source resources
or fetches remote links, including private URLs. Ordinary document links can
be relative or bundle-root-relative; missing targets are tolerated because
linked knowledge may not exist yet. Authored links are not rewritten.
