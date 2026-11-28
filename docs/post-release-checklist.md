---
type: Guide
title: "Post-release documentation checklist"
description: "Reconcile release notes, upgrade guidance, and contributor credit after a release."
generated: { by: codex/gpt-6.2-sol, at: "2026-11-28T16:24:49Z" }
---

# Post-release documentation checklist

Use this checklist to reconcile public documentation with an already published
release. Start from the published tag and the GitHub release notes, as Kit does.
Follow your repository’s own release procedure.

- [ ] Compare the release notes with the commits and pull requests between the
  previous published tag and the new tag. Keep entries for later work out of
  this release. If the repository maintains a changelog, reconcile it against
  the same range.
- [ ] Verify version claims and upgrade guidance against the shipped source.
  Update prerequisites, migration steps, or rollback guidance where they changed.
- [ ] Check contributor credit against shipped pull requests and release notes.
- [ ] Open any release pages and links you updated. Check the version, date, and
  anchors against the published release.

[Kata’s 0.23.0 update](https://github.com/kenn-io/kata/pull/1040) shows how a
repository with a changelog can apply the same checks.
