---
type: Guide
title: Adopting the Kenn documentation profile
description: Use the Kenn OKF metadata profile and check meaningful documentation edits.
generated: { by: codex/gpt-6.1-sol, at: "2026-11-19T16:01:38Z" }
---

# Adopting the Kenn documentation profile

Kenn applies a documentation profile to tracked Markdown based on the
[pinned OKF v0.3 specification](https://github.com/open-knowledge-format/open-knowledge-format/blob/527e81af/SPEC.md).
OKF itself requires only `type`. Kenn also requires a nonempty `title` and
`description`, and `generated` recording the actor and the UTC time of the
last meaningful edit. Validation accepts RFC 3339 date-times with an offset
and quoted or native `YYYY-MM-DD` dates (midnight UTC). A date-time without
an offset is invalid. Kenn producers write the actual edit time in UTC,
ending in `Z`, rather than reducing precision to a date. Use Guide for instructions,
Reference for facts and formats, and Explanation for architecture and research.

Write provenance on one line, for example:
`generated: { by: codex/gpt-6.2-astra, at: "2026-11-19T08:00:00Z" }`.
For an agent-authored edit, name the actual coding tool and model/version as
`<producer>/<version>`. Use `human:<id>` only for a hand-authored edit, not
the owner of the repository or the person who approved an agent's change.
For an automated process, use `process:<id>`. If the actor is unknown, ask
rather than infer it from Git credentials. Generation does not imply review.
The fixer writes an inline mapping; equivalent block mappings remain readable.

The known top-level `status` values are `stable`, `draft`, and `deprecated`. An
omitted status means stable; explicitly marking a page stable does not mean it
has been independently verified. Use draft for intentionally provisional
content. If a retired page is retained, set `status: deprecated` and link to
its replacement when one exists, optionally using `superseded_by`. Unknown
status strings are preserved, never interpreted as stable; current-knowledge
views exclude them. Kenn requires status to be a nonempty string when present. The check
cannot infer retirement from a filename or age. The fixer never sets or changes
status.

`README.md`, `AGENTS.md`, and `CLAUDE.md` files never take frontmatter at any
depth. The check also excludes unlisted `SKILL.md` files, release notes, changelogs,
`.github`, `testdata`, `vendor`, and `node_modules`.
It also skips all Markdown inside skill bundles. Reserved `index.md` files
are not concepts: they need no type or generated fields. A bundle-root index
may contain `okf_version: "0.3"`, `title`, `description`, `license`, and
extension keys. Regeneration must preserve that frontmatter. `log.md` must
have no frontmatter, and the fixer never adds one. Add `-exclude GLOB` for other
exceptions. Globs use `**` for any number of directories. Path arguments limit
the check to tracked files; put flags before paths.

Configure repeatable path rules such as `-type-rule 'docs/reference/**=Reference'`.
Rules must agree where they overlap. Run `kennlint frontmatter -fix` with
those rules and `-by <actor>` to fill missing fields. When it adds
`generated.at`, the fixer stamps the current UTC time. Converting a page records
the actual converting actor and time, not an invented original author or old
edit time. Git retains earlier history. The fixer takes an H1 title only when it is the first block; supply a
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
rules, and `BY=<actor>` supplies the actor. Other repositories can use
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
    resource: /references/api.md
  - resource: all queries in project X
```

For per-claim attribution, use a Markdown footnote with the source ID as its
label, for example `[^api-guide]`, and supply the ordinary footnote definition.
The declared ID links the note to its source. OKF uses the same syntax as
ordinary footnotes: an unknown label cannot reliably be identified as a
misspelled source ID. The check therefore validates declared source IDs, not
unmatched footnote labels, and does not require every source to be cited.

Unknown keys are allowed at every level, including inside `generated` and
`sources`; the fixer preserves their values. Use `x-` for extension keys that
future OKF versions will never reserve.

No sources or backlinks are required. Validation never opens source resources
or fetches remote links. Links and path fields use the same resolution: without a leading `/`, resolve
from the directory of the file that contains the value; with `/`, resolve from
the bundle root. For example, a source in `guides/page.md` can name
`../references/api.md` or `/references/api.md`. This also applies to `resource`,
`computation`, `executor.resource`, `attester.resource`, `superseded_by`, and
bundle `license` paths. Both forms are valid; targets outside the bundle are
external, and consumers must not read them from the local filesystem. A
consumer may try the bundle root for an unresolved bare path only for v0.2,
earlier, or undeclared bundles. This validator checks metadata and does not
resolve or traverse those targets. Missing targets are tolerated because
linked knowledge may not exist yet. Authored links are not rewritten.

## Skills

List Kenn-managed skills with repeatable `-managed-skill 'skills/*/SKILL.md'`
only after confirming their loaders and sync targets preserve nested YAML
mappings. This
profile is a Kenn extension, not strict Agent Skills metadata, which allows
only string values. Portable, third-party, and imported skills stay unlisted
and untouched; symlinked skill files and all bundle resources, assets,
templates, and fixtures are skipped. A tracked repository-root `SKILL.md` makes
the repository root its bundle, whether or not the skill is listed.

Keep native `name` and `description` fields; do not require a separate
`title` on a skill. Apply the same actor and UTC-time rules to the Kenn
extension:

```yaml
metadata:
  type: Skill
  generated: { by: codex/gpt-6.2-astra, at: "2026-11-19T08:00:00Z" }
  status: deprecated # only when the skill has been retired
```

Keep the skill timestamp quoted so its YAML type is string; this Kenn
loader constraint is stricter than ordinary OKF documents, which also accept
native YAML timestamps. Quoted offsets and dates are accepted as above.
Omitted `metadata.status` means stable; use draft for provisional skills. If a
retired skill is retained, set `metadata.status: deprecated` and link to its
replacement when one exists. Unknown status strings remain unchanged and
are excluded from current knowledge, as for ordinary documents. Extension
keys are permitted inside `metadata` and `metadata.generated`, including `x-`
keys. The fixer leaves status and native fields alone.
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
Skills, skill bundle files, deprecated pages, README and AGENTS files, and
release notes are never listed. Retain deprecated pages as linked pointers to preserve public URLs. Repeat
the original headings in each pointer so old fragment links land on a heading
that links to the new section.
