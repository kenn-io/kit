package agenthook

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScriptRuntimeResponses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	runtime, err := os.ReadFile("script_runtime.js")
	require.NoError(t, err)
	dir := t.TempDir()
	driver := filepath.Join(dir, "driver.mjs")
	require.NoError(t, os.WriteFile(driver, append(runtime, []byte(scriptResponseDriver)...), 0o600))
	cmd := exec.CommandContext(t.Context(), node, driver, dir)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

const scriptResponseDriver = `
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { setTimeout as delay } from "node:timers/promises";
const dir = process.argv[2];
const marker = dir + "/marker";
const command = (script, matcher) => ({ matcher, hooks: [{ command: process.execPath, args: ["-e", script] }] });
const config = { hooks: { test: [
 command('const b=Buffer.from(JSON.stringify({n:1, text:"é"})); const i=b.indexOf(0xc3); process.stdout.write(b.subarray(0,i+1)); setTimeout(()=>process.stdout.write(b.subarray(i+1)), 20)'),
 command('process.stdout.write("  ")'), command('process.stdout.write("{}")'),
 command('process.stdout.write("bad")'), command('process.stdout.write("[]")'),
 command('process.stdout.write("x".repeat(16*1024*1024+1))'),
 command('process.stdout.write("{\\"lost\\":true}"); process.exitCode=2'),
 command('process.stdout.write("{\\"n\\":2}")'),
 command('require("fs").appendFileSync(process.argv[1], "wrong")', "other"),
 command('require("fs").appendFileSync(process.argv[1], "matched")', "bash"),
] } };
// Supply the marker path to the two commands which use it.
for (const entry of config.hooks.test.slice(-2)) entry.hooks[0].args.push(marker);
const result = await runHooks("test", {cwd: dir}, {collect: true, matcher: "bash"});
assert.deepEqual(result.responses, [{n:1, text:"é"}, {n:2}]);
assert.equal(result.failures.length, 4);
assert.equal(readFileSync(marker, "utf8"), "matched");
config.hooks.test = [command('process.stdout.write("{\\"stop\\":true}")'), command('throw Error("should not run")')];
const stopped = await runHooks("test", {cwd:dir}, {collect:true, stop: r => r.stop});
assert.deepEqual(stopped.responses, [{stop:true}]);
assert.deepEqual(stopped.failures, []);
// Synchronize on the second child actually starting before firing either
// deadline. A fake budget clock makes the shared timer deterministic even on
// a loaded runner; the command timeout remains a real timer.
const waitFor = async (path) => {
 const deadline = Date.now() + 30000;
 while (!existsSync(path)) {
  if (Date.now() >= deadline) throw new Error("child did not start: " + path);
  await delay(20);
 }
};
const entries = () => [
 command('process.stdout.write("{\\"first\\":true}")'),
 command('require("fs").writeFileSync(process.argv[1], "started"); setTimeout(()=>{}, 60000)'),
 command('require("fs").writeFileSync(process.argv[1], "unexpected")'),
];
const budgetStarted = dir + "/budget-started";
const budgetLater = dir + "/budget-later";
config.hooks.test = entries();
config.hooks.test[1].hooks[0].args.push(budgetStarted);
config.hooks.test[2].hooks[0].args.push(budgetLater);
const realSetTimeout = globalThis.setTimeout;
let expireBudget, budgetTimers = 0;
globalThis.setTimeout = (callback, ms, ...args) => {
 if (ms === 43210) { budgetTimers++; expireBudget = callback; return realSetTimeout(callback, 30000); }
 return realSetTimeout(callback, ms, ...args);
};
let budget;
try {
 const running = runHooks("test", {cwd:dir}, {collect:true, budgetMs:43210});
 await waitFor(budgetStarted);
 expireBudget();
 budget = await running;
} finally {
 if (expireBudget) expireBudget();
 globalThis.setTimeout = realSetTimeout;
}
assert.equal(budgetTimers, 1);
assert.deepEqual(budget.responses, [{first:true}]);
assert.equal(budget.failures.length, 1);
assert.match(budget.failures[0], /callback budget exceeded/);
assert.equal(existsSync(budgetLater), false);
config.hooks.test = [command('setTimeout(()=>{}, 10000)')];
config.hooks.test[0].hooks[0].timeout = 0.1;
const timed = await runHooks("test", {cwd:dir}, {collect:true});
assert.match(timed.failures[0], /timed out/);
const cancelStarted = dir + "/cancel-started";
const cancelLater = dir + "/cancel-later";
config.hooks.test = entries();
config.hooks.test[1].hooks[0].args.push(cancelStarted);
config.hooks.test[2].hooks[0].args.push(cancelLater);
const controller = new AbortController();
const running = runHooks("test", {cwd:dir}, {collect:true, signal:controller.signal});
try { await waitFor(cancelStarted); } finally { controller.abort(); }
const cancelled = await running;
assert.deepEqual(cancelled.responses, [{first:true}]);
assert.equal(cancelled.failures.length, 1);
assert.match(cancelled.failures[0], /cancelled/);
assert.equal(existsSync(cancelLater), false);
// A descendant holds stdout open after the parent writes a complete response.
config.hooks.test = [command(` + "`" + `const {spawn} = require("node:child_process");
const child = spawn(process.execPath, ["-e", "setTimeout(()=>{},10000)"], {stdio:["ignore", "inherit", "ignore"]});
require("fs").writeFileSync(process.argv[1], String(child.pid));
process.stdout.write(JSON.stringify({held:true})); child.unref();` + "`" + `)];
const pidfile = dir + "/pid";
config.hooks.test[0].hooks[0].args.push(pidfile);
try {
 const held = await runHooks("test", {cwd:dir}, {collect:true});
 assert.deepEqual(held.responses, [{held:true}]);
 assert.deepEqual(held.failures, []);
} finally { process.kill(Number(readFileSync(pidfile, "utf8"))); }
`
