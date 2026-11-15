---
title: "Post-release documentation checklist"
description: "Reconcile release notes, upgrade guidance, and contributor credit after a release."
last_edited: 2026-11-15
---

# Post-release documentation checklist

Use this checklist after a maintainer has published a release. It reconciles
public documentation with what shipped; it is not authority to tag or publish.
Follow each repository's own release procedures. Apply only the items relevant
to that release. [Kata PR #1040](https://github.com/kenn-io/kata/pull/1040)
illustrates a post-release documentation reconciliation.

## Release evidence

- [ ] Record the published tag, previous published tag, release date, and
  source range between them. Check the tag and release notes against the
  shipped commits and pull requests, not just the current branch.

## Documentation

- [ ] Reconcile the changelog with the shipped range: move only shipped entries
  into the release, retain later unreleased work, and add missing notable
  changes supported by the shipped source or pull requests.
- [ ] Replace unreleased references with the earliest release that contains
  the change, verified against tags. Leave references to unshipped work alone.
- [ ] Where relevant, update upgrade prerequisites, migration steps, rollback
  guidance, and component version requirements. Link to the source or release
  notes that support them. Distinguish application, API, schema, and dependency
  versions rather than treating them as interchangeable.
- [ ] Credit contributors from shipped pull requests and release notes; verify
  attribution rather than guessing from commit authorship or current work.
- [ ] If affected, update landing pages and documentation links or anchors so
  they point to the right release and existing sections.

## Final check

- [ ] Review the rendered documentation and links, and run the repository's
  applicable documentation checks. Confirm that release dates, versions,
  references, and credit agree with the release evidence above.
