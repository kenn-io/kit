---
title: "Adopting prek Git hooks"
description: "Give a repository commit and push hooks that run the same checks as CI, and tell contributors how to use them."
last_edited: 2026-11-12
---

# Adopting prek Git hooks

kenn-io repositories use [prek](https://github.com/j178/prek) to run checks
before a commit and before a push. This document describes the shared pattern
so a new repository starts with it and an existing one can compare.

The pattern is a convention, not a shared configuration. Each repository owns
its `prek.toml` and decides which checks it runs.

## What you get

- A contributor finds a formatting, lint, or test failure before the push,
  instead of after a CI run.
- One definition per check. A hook calls the `make` target that CI calls, so
  the two cannot drift.
- One tracked file, `prek.toml`, that lists every hook and its stage.

Hooks are a convenience. A contributor can skip them, and they check a working
tree, not a pushed commit. CI on the pull request stays the check that counts.

## Adopting a repository

1. Add `prek.toml` at the repository root. Pin the oldest prek version the
   file needs and install both hook types:

   ```toml
   minimum_prek_version = "0.5.7"
   default_install_hook_types = ["pre-commit", "pre-push"]
   default_stages = ["pre-commit"]
   ```

2. Put fast checks in the commit stage: formatting and file hygiene. A commit
   hook that takes more than a few seconds gets skipped.

   ```toml
   [[repos]]
   repo = "builtin"

   [[repos.hooks]]
   id = "check-merge-conflict"

   [[repos.hooks]]
   id = "detect-private-key"

   [[repos.hooks]]
   id = "end-of-file-fixer"

   [[repos]]
   repo = "local"

   [[repos.hooks]]
   id = "gofmt"
   name = "gofmt"
   language = "system"
   entry = "gofmt -w"
   files = "\\.go$"
   ```

3. Put slower checks in the push stage, and make each one a `make` target
   that CI also runs. Replace the targets below with the repository's own:

   ```toml
   [[repos.hooks]]
   id = "lint"
   name = "make lint"
   language = "system"
   entry = "make lint"
   stages = ["pre-push"]
   files = "^(go\\.mod|go\\.sum|.*\\.go)$"
   pass_filenames = false
   require_serial = true
   ```

   `files` skips the hook when the push changes nothing it checks. Use
   `always_run = true` instead for a check that must run on every push.

4. Pin prek where the repository pins its other tools, so contributors and CI
   agree on the version.

5. Tell contributors how to use the hooks in the contributing guide:

   ```sh
   prek install
   prek run --stage pre-push --from-ref origin/main --to-ref HEAD
   ```

   The second command runs the push hooks without pushing. Say that hooks
   that call `make` check the working tree, so uncommitted changes are
   included.

The kit repository's `prek.toml` and `Makefile` are a working example.

## Adopting in a repository that already has hooks

Keep the hooks and checks the repository has. Compare it with the steps above
and add only what is missing. Do not move a check between stages or replace a
repository's `make` target to match another repository.

## What stays out of hooks

- **AI review.** A review costs money, can fail on a rate limit, and is not
  deterministic. A repository must not require one in a hook. A contributor
  who runs a reviewer does so by choice.
- **Checks CI does not run.** A hook-only check passes for some contributors
  and is never run for others.
- **Anything that needs credentials or the network** beyond fetching pinned
  tools.
