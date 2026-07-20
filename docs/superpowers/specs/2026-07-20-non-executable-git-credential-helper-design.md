# Non-executable Git credential helper design

## Problem

`gitcmd.Runner.WithBasicAuth` currently creates a new executable shell script
for every Git command. The script contains the username and password, returns
them through Git's credential-helper protocol, and is removed when the command
finishes.

The short lifetime and private permissions protect the credentials, but every
invocation creates and executes a previously unseen program. Endpoint security
software can consequently scan each helper at first execution. On macOS,
XProtect performs this scan for new or changed executables, so Git-heavy test
and automation workloads can spend substantial CPU time assessing helpers that
exist only for a single command.

## Goals

- Stop creating executable credential-helper files.
- Keep usernames, passwords, and derived authorization values out of child
  process arguments and environment variables.
- Preserve per-command credential lifetime and cleanup.
- Keep the `Runner` and `WithBasicAuth` public API unchanged.
- Preserve behavior on Unix, macOS, and Windows.

## Non-goals

- Suppressing or configuring endpoint security software.
- Caching credentials across separate Git commands.
- Introducing persistent credential storage or integrating with platform
  keychains.
- Changing authentication behavior in downstream applications.

## Considered approaches

### Private data file with an inline helper

Write the credential response to a private, non-executable file. Configure
Git's `credential.helper` with its supported `!` shell-snippet form. The
snippet handles the `get` operation by reading the response file and emits
nothing for `store` and `erase`.

This retains the current security and cleanup boundaries while ensuring that
the only newly created object is data, not executable code. It is the selected
approach.

### Credentials embedded in an inline helper

Putting credential values directly in the shell snippet removes the temporary
file, but Git config is injected through `GIT_CONFIG_VALUE_*` environment
variables. The credentials would therefore become visible in the Git process
environment and violate the existing secret-isolation guarantee.

### Pipe or socket credential broker

A broker could keep credentials entirely in memory and serve Git's helper over
an IPC endpoint. This introduces synchronization, helper lifetime, failure,
and cross-platform concerns that are disproportionate to the short-lived data
file already accepted by the current design.

### Reusable executable helper

Reusing an executable would reduce the number of scans, but it would not remove
the executable boundary. Passing per-command credentials to it would still
require another protected channel, and caching credential-bearing scripts
would lengthen secret lifetime.

## Detailed design

### Credential response file

For each `Run` or `Output` call made by a runner with basic authentication:

1. Reject usernames or passwords containing LF, CR, or NUL before creating a
   file. Those bytes cannot be represented safely in Git's line-oriented
   credential protocol.
2. Create a uniquely named file in the operating system's temporary directory.
   `os.CreateTemp` creates it atomically with mode `0600` on Unix, so there is
   no post-creation interval with broader permissions. On Windows, retain the
   existing reliance on the current user's private temporary directory and
   inherited owner access.
3. Write a Git credential-protocol response containing `username` and
   `password`, one attribute per line. EOF terminates the response; a trailing
   blank line is optional.
4. Close the file before starting Git.
5. Return both the helper configuration and an idempotent cleanup function.

The file contains data only: it has no shebang, commands, or executable bits.

### Helper configuration

The injected `credential.helper` value begins with `!`, which Git defines as a
shell snippet. The snippet:

- consumes the credential request from standard input through its terminating
  blank line or EOF;
- responds with credentials only when its first argument is `get`;
- reads the response file without evaluating its contents;
- writes the attributes to standard output unchanged; and
- returns success without output for `store` and `erase`.

Only the quoted response-file path appears in the Git process environment.
Credentials remain confined to the private file and the helper's standard
output pipe. The path must be safely quoted for spaces, apostrophes, dollar
signs, backslashes, and other shell metacharacters. The implementation should
use portable shell constructs available in the shell Git uses on supported
Unix platforms and Git for Windows. On Windows, the quoted path remains in
drive-letter and backslash form; Git for Windows invokes the snippet through
its bundled MSYS shell, whose filesystem layer resolves that native path when
the snippet opens the response file.

### Cleanup and errors

The response file is removed through the existing deferred cleanup path after
Git exits, whether Git succeeds, fails, or the context is cancelled. Cleanup
is idempotent and remains best effort, matching current behavior.

Validation, creation, write, or close failures continue to produce a helper
that fails the Git operation without including credentials in its diagnostic.
Partially created files are removed before the failure is returned to Git.

## Security properties

- Credentials do not appear in Git arguments, config environment values,
  process error strings, or panic text.
- The response file is private, non-executable, and scoped to one Git command.
- Credential values containing protocol delimiters are rejected. All accepted
  file contents are treated as protocol data, not shell source, so credential
  characters cannot alter helper or protocol behavior.
- The temporary file is closed before Git reads it and removed immediately
  after Git completes.
- The change does not enable ambient Git credential helpers or relax the
  runner's existing config isolation.

## Testing

Package tests will verify:

- `WithBasicAuth` still supplies working credentials through Git's helper
  protocol.
- Username, password, Basic authorization material, and extra-header forms do
  not appear in child arguments or environment variables.
- The temporary object exists while Git runs, is a regular non-executable file,
  and has `0600` permissions on Unix.
- The response file is removed after successful and failed Git commands.
- Credentials and temporary paths containing spaces and shell metacharacters
  are handled as data.
- Usernames and passwords containing LF, CR, or NUL are rejected without
  creating a credential response file or leaking the rejected value.
- The helper consumes Git's complete stdin request before responding.
- `store` and `erase` operations do not disclose credentials.
- A targeted Windows test invokes the real Git credential plumbing command and
  verifies that an inline helper can read a response file through a quoted
  native drive-letter path and round-trip both credential fields.
- Existing Windows tests and the full cross-platform suite continue to pass.

On macOS, a targeted diagnostic may additionally confirm that repeated
authenticated Git commands no longer produce XProtect scan entries for
`gitcmd-credential-helper-*`. This is supporting evidence rather than a test
suite dependency.

## Compatibility

The public Go API is unchanged. Callers continue using `WithBasicAuth` followed
by `Run` or `Output`. The helper remains per-command and noninteractive. No
credential files survive successful cleanup, and no storage-format or caller
migration is required. Credentials containing LF, CR, or NUL now fail closed
instead of producing an ambiguous or injectable credential-protocol stream.
