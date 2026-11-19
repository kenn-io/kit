# Agent hook commands

For an application with its own hook handler, attach
`agentsetup.NewHookCommand(options)` to its Cobra root. The callback returns
`agenthook.InstallOptions` for one harness: the executable and arguments to
run, the events to register, and a stable application-specific marker. Reject
agents your handler does not support. For `HookUninstall`, return just the
marker and config path so removal works after the executable is gone. Use the same options with
`agentsetup.Hooks` if your application also has an aggregate setup command.
See the [compiling example](hook_command_test.go).

```console
example agent-hook install --harness claude --dry-run
example agent-hook install --harness claude
example agent-hook status --harness claude
example agent-hook uninstall --harness claude --dry-run
example agent-hook uninstall --harness claude
```

The hook command selects a runtime with `--harness`, leaving any inherited
`--agent` output flag untouched. No command runs at application startup. A bare `agent-hook` shows help.
Installation and removal affect only registrations identified by your marker;
other hooks and harness trust decisions remain the user's. Dry runs report the
target and whether it would change, without writing files, running the harness,
or printing its config. Serialize mutations of the same config path.

`status` (alias `check`) checks **readiness**, not just presence. As with
aggregate setup, a non-ready result, including unknown, returns
`agentsetup.ErrNotReady`. Kit currently inspects Claude Code and Codex active
user configurations. A different config path or an unsupported inspection
reports unknown. Checking Codex may start its app-server, which can create its
own state and install bundled skills; it does not grant trust.

To preserve an existing `example agent hook` spelling, change the returned
command's `Use` to `"hook"` and attach it to the application's `agent` group.
Application-specific scope flags belong on the returned command as persistent
flags; resolve them to the appropriate config path in the options callback.
If `status` already lists live sessions, first find the readiness child with
`hook.Find([]string{"status"})`. Set its `Use` to `"check"` and clear its
`Aliases` before attaching the existing session-status command.
Git hooks and managed instruction-file blocks are separate integrations: do
not register command hooks as a substitute for an explicitly hookless setup.
