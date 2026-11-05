# clisearch invariants

- Search stays offline and app-neutral. Read Cobra metadata only; never run
  handlers, hooks, help functions, flag Value methods or normalization callbacks.
- Do not use LocalFlags, InheritedFlags or other Cobra flag-merge helpers.
  They mutate effective flags, import global flags and invoke caller callbacks.
  Enumerate Flags/PersistentFlags and ancestor persistent definitions instead.
  Deduplicate nearest stored Flag.Name definitions before filtering hidden/
  deprecated flags. Parser-equivalent shadowing requires consistently normalized
  registration throughout the tree; never evaluate custom normalizers.
- Built-in skips prune subtrees: hidden and deprecated commands and the
  annotated NewCommand group at any depth, help and completion only as root
  children. Options.Skip excludes a node only. Root and non-runnable groups are
  never results. Consumers own environment-specific skips.
- Index parent paths below the searched root, so the tool's own name does not
  match every command.
- Preserve the exported API and JSON tags. NewCommand owns only --limit;
  callers own JSON selectors, error types and configuration-free lifecycle gates.
  Nil UsageError must preserve inherited flag-error handling.
- Rank query candidate groups once per field, with pooled document frequency;
  average lengths over populated fields so sparse keywords retain their boost.
  Sort ties by canonical command path and never expose scores as API.
- Build usage with a shallow command copy and DisableFlagsInUseLine before
  UseLine; preserve authored syntax without flag merging or live mutation.
- Cobra trees are mutable and may lazily allocate/sort during reads; do not
  promise concurrent Search safety. Tests use synthetic trees and no user data.
