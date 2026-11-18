# Documentation

Find Kit guides and explanations below, along with package documentation and
release notes. Package APIs are documented in Go documentation.

## Pages

<!-- BEGIN kennlint index -->
- [Streaming pack I/O](explanation/streaming-pack-io.md) - Architecture and memory bounds for streaming pack reads and writes.
- [Vite Task's sandbox-compatible IPC changes](explanation/vite-task-sandbox-ipc.md) - Research on sandbox-compatible IPC behavior in Vite Task.
- [Adopting the Kenn documentation profile](guides/adopting-docs.md) - Use the Kenn OKF metadata profile and check meaningful documentation edits.
- [Adopting the shared Go lint policy (kennlint)](guides/adopting-kennlint.md) - Adopt the shared Go lint policy, workflow checks, and Markdown frontmatter validation.
- [Adopting packed content storage](guides/adopting-packstore.md) - Integrate packed content storage and maintain its publication and recovery guarantees.
- [Adopting prek Git hooks](guides/adopting-prek-hooks.md) - Give a repository commit and push hooks that run the same checks as CI, and tell contributors how to use them.
- [CLI command design template](guides/cli-command-template.md) - Worksheet and review checklist for designing a CLI command or command family.
- [Shared CLI design guidance](guides/cli-design.md) - Help, output, result hints, error, exit code, and migration conventions for kenn-io CLIs, grounded in Kata, Roborev, and Docbank.
- [Developing kit](guides/development.md) - Development tooling, build conventions, and debugging guidance for kit.
- [Installing the local embedding server](guides/kennembed.md) - Install and operate the local embedding server and its model assets.
- [Secret sources](guides/secrets.md) - Configure literal, environment, and private-file secrets with secretref.
<!-- END kennlint index -->

## Package documentation

- [Task-based command discovery](../clisearch/README.md)
- [Backup repository and pack format](../backup/FORMAT.md)
- [Daily screen reports](../telemetry/posthog/daily.md)
- [Confirmed milestone delivery](../telemetry/posthog/delivery.md)
- [PostHog event fields](../telemetry/posthog/fields.md)

## Releases

- [Kit v0.8.0](releases/v0.8.0.md)
- [Kit v0.37.1](releases/v0.37.1.md)
