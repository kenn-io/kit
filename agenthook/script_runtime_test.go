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
import { readFileSync } from "node:fs";
const dir = process.argv[2];
const marker = dir + "/marker";
const command = (script) => ({ hooks: [{ command: process.execPath, args: ["-e", script] }] });
const config = { hooks: { test: [
 command('const b=Buffer.from(JSON.stringify({n:1, text:"é"})); const i=b.indexOf(0xc3); process.stdout.write(b.subarray(0,i+1)); setTimeout(()=>process.stdout.write(b.subarray(i+1)), 20)'),
 command('process.stdout.write("  ")'), command('process.stdout.write("{}")'),
 command('process.stdout.write("bad")'), command('process.stdout.write("[]")'),
 command('process.stdout.write("x".repeat(16*1024*1024+1))'),
 command('process.stdout.write("{\\"lost\\":true}"); process.exitCode=2'),
 command('process.stdout.write("{\\"n\\":2}")'),
 command('require("fs").appendFileSync(process.argv[1], "first")'),
 command('require("fs").appendFileSync(process.argv[1], "second")'),
] } };
// Supply the marker path to the two commands which use it.
for (const entry of config.hooks.test.slice(-2)) entry.hooks[0].args.push(marker);
const result = await runHooks("test", {cwd: dir}, {collect: true});
assert.deepEqual(result.responses, [{n:1, text:"é"}, {}, {n:2}]);
assert.equal(result.failures.length, 4);
assert.equal(readFileSync(marker, "utf8"), "firstsecond");
config.hooks.test = [command('setTimeout(()=>{}, 10000)')];
config.hooks.test[0].hooks[0].timeout = 0.1;
const timed = await runHooks("test", {cwd:dir}, {collect:true});
assert.match(timed.failures[0], /timed out/);
// Observational profiles ignore even malformed or oversized stdout.
config.hooks.test = [command('process.stdout.write("junk".repeat(5*1024*1024))')];
assert.equal(await runHooks("test", {cwd:dir}), undefined);
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
