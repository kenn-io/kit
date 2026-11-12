---
title: "Adopting Bombadil for UI property tests"
description: "Add bounded, isolated browser property testing to Kenn applications without replacing deterministic regression tests."
last_edited: 2026-11-12
---

# Adopting Bombadil for UI property tests

[Bombadil](https://github.com/antithesishq/bombadil) explores a running UI and
checks properties across generated interaction sequences. Use it in applications
that have a browser UI, alongside the unit and integration tests that exercise
their Go packages. Kit itself has no application UI to explore, so this guide
adds no browser dependency to Kit's Go module or default test targets.

This is a different layer from Go property testing with Rapid or Hegel. Bombadil
is useful for finding unexpected navigation, input and viewport interactions;
it does not prove that every workflow works or that a layout matches a design.
Keep deterministic regressions, accessibility checks and visual comparisons.
Do not delete a Playwright suite merely because a bounded random run passed.

## Start with an isolated target

The adopting application owns its fixture server, reset procedure, properties
and CI budget. Before exploration:

- Serve a disposable local instance with synthetic data and a dedicated test
  identity. Never attach a personal browser profile or production cookies.
- Disable or fake outbound email, payments, uploads and other external effects.
  A local frontend connected to a production backend is not a safe fixture.
- Reset application state before each run and before reproducing a failure.
- Keep the exploration boundary narrow. In v0.7.10 the initial origin is the
  default boundary; `--allow-url` replaces that boundary. Navigation limits are
  not a network firewall and do not make destructive same-origin actions safe.
- Treat traces and screenshots as potentially sensitive. Retain only synthetic
  artifacts, and never commit tokens or browser profiles.

## Install in the adopting application

The commands below target Bombadil **v0.7.10**. Install it in the application's
frontend package, not Kit's module root, and commit the lockfile:

```sh
npm install --save-dev --save-exact @antithesishq/bombadil@0.7.10
./node_modules/.bin/bombadil --version
./node_modules/.bin/bombadil browser test --help
```

Use the application's existing package manager if it is not npm. Upstream
supports local macOS and Linux execution and publishes platform binaries and
an npm package. See the [installation manual](https://antithesishq.github.io/bombadil/browser/2.1-installation.html)
for current platform requirements. Do not invent a Homebrew dependency or
silently disable the browser sandbox to make a test launch.

Pin the version and check its CLI help when upgrading. In v0.7.10 the
specification is a **positional argument**, not `--specification`.

## Write application properties

Create `bombadil/specification.ts` in the frontend package. Start with default
properties and actions, then add an invariant tied to an observable application
state. For an app that exposes `data-session` on its root element and a stable
logout test selector:

```typescript
import { always } from "@antithesishq/bombadil";
import { extract } from "@antithesishq/bombadil/browser";
export * from "@antithesishq/bombadil/browser/defaults";

const session = extract(({ document }) => {
  const authenticated = document.documentElement.dataset.session === "authenticated";
  const logout = document.querySelector<HTMLButtonElement>('[data-testid="logout"]');
  return {
    authenticated,
    logoutEnabled: !!logout && !logout.disabled && logout.getClientRects().length > 0,
  };
});
export const authenticatedSessionHasLogout = always(
  () => !session.current.authenticated || session.current.logoutEnabled,
);
```

Adapt both selectors to the actual UI contract. This checks for a rendered,
enabled logout control while authenticated; it does not prove the control is
unobscured or that logout invalidates the server session. Test those separately.
For a menu-based logout flow, model opening the menu and eventual access rather
than requiring the control to be rendered in every state.

A conditional invariant can pass vacuously if exploration never authenticates.
Start the fixture in the required state, assert that setup succeeded, and confirm
that the trace visited it. Deliberately remove or disable the logout control to
check that the property fails. Restore it and run again before trusting the gate.
Keep default error properties enabled unless a specific, documented application
behavior requires a narrower assertion.

## Run and retain evidence

Start and health-check the application's fixture server first. With that server
listening at `127.0.0.1:4173`, run a bounded smoke check:

```sh
mkdir -p .bombadil
./node_modules/.bin/bombadil browser test \
  --headless --exit-on-violation --time-limit 1m \
  --output-path .bombadil/smoke \
  http://127.0.0.1:4173 bombadil/specification.ts
```

Use a fresh output directory for each run; do not hide a prior failure by replacing
its evidence. Add `.bombadil/` to the application's `.gitignore`. In v0.7.10 the output path
is a directory containing `trace.jsonl` and screenshots. Later versions may use
a different format; check the pinned release rather than copying commands from
the development branch.

Inspect a failure and reproduce it against reset fixture state:

```sh
./node_modules/.bin/bombadil browser inspect .bombadil/smoke
./node_modules/.bin/bombadil browser test \
  --headless --reproduce .bombadil/smoke \
  http://127.0.0.1:4173 bombadil/specification.ts
```

Replay is best-effort and can diverge. Preserve the original trace, application
revision, fixture setup and tool version. Convert a confirmed failure into a
focused regression test as well as retaining or refining the property.

## Adopt as a bounded repository gate

Put server startup, health checks, reset, test invocation and guaranteed teardown
in one application-owned command, such as `npm run test:properties`. Propagate
startup errors, property violations and runner failures as nonzero exits. Give
the whole job a timeout in addition to Bombadil's exploration limit.

A repository using prek can opt into that command on pre-push after its fixture
is reliable; do not add an unbounded explorer to pre-commit. Run the same command
in CI so a skipped local hook does not bypass the check. Keep the existing unit,
integration and deterministic UI gates. Upload synthetic failure traces even
when the job fails, using a short retention period and restricted artifact
access. Avoid passing production secrets into PR jobs.

Start with short PR runs and a separately configured longer stress job. Longer
runs increase exploration, not certainty. Report runner/setup failures separately
from property violations, and never report an unstarted browser as a passing test.
