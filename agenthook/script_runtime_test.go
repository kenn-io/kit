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
	dir := t.TempDir()
	driver := filepath.Join(dir, "driver.mjs")
	require.NoError(t, os.WriteFile(driver, []byte(scriptRuntime+scriptResponseDriver), 0o600))
	cmd := exec.CommandContext(t.Context(), node, "--expose-gc", driver, dir)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

const scriptResponseDriver = `
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
const dir = process.argv[2];
const command = (script) => ({ hooks: [{ command: process.execPath, args: ["-e", script] }] });
const config = { hooks: { test: [
 command('const b=Buffer.from(JSON.stringify({n:1, text:"é"})); const i=b.indexOf(0xc3); process.stdout.write(b.subarray(0,i+1)); setTimeout(()=>process.stdout.write(b.subarray(i+1)), 20)'),
 command('process.stdout.write("  ")'), command('process.stdout.write("{}")'),
 command('process.stdout.write("bad")'), command('process.stdout.write("[]")'),
 command('process.stdout.write(Buffer.from([123,34,120,34,58,34,255,34,125]))'),
 command('process.stdout.write(JSON.stringify({p:"x".repeat(16*1024*1024)}))'),
 command('process.stdout.write("{\\"lost\\":true}"); process.exitCode=2'),
 command('process.stdout.write("{\\"n\\":2}")'),
] } };
const result = await runHooks("test", {cwd: dir}, {collect: true});
assert.deepEqual(result.responses, [{n:1, text:"é"}, {}, {n:2}]);
assert.equal(result.failures.length, 5);
assert.match(result.failures[2], /invalid stdout/);
assert.match(result.failures[3], /stdout exceeds/);
// Observational profiles ignore even malformed or oversized stdout.
config.hooks.test = [command('process.stdout.write("junk".repeat(5*1024*1024))')];
assert.equal(await runHooks("test", {cwd:dir}), undefined);
// A descendant holds stdout open after the parent writes a complete response.
config.hooks.test = [command(` + "`" + `const {spawn} = require("node:child_process");
const child = spawn(process.execPath, ["-e", "setTimeout(()=>{},60000)"], {stdio:["ignore", "inherit", "ignore"]});
require("fs").writeFileSync(process.argv[1], String(child.pid));
process.stdout.write(" ".repeat(8*1024*1024) + JSON.stringify({held:true})); child.unref();` + "`" + `)];
const pidfile = dir + "/pid";
config.hooks.test[0].hooks[0].args.push(pidfile);
let captured;
const concat = Buffer.concat;
Buffer.concat = (chunks, ...args) => { captured = new WeakRef(chunks); return concat(chunks, ...args); };
try {
 const held = await runHooks("test", {cwd:dir}, {collect:true});
 assert.deepEqual(held.responses, [{held:true}]);
 assert.deepEqual(held.failures, []);
 // The descendant still holds the pipe; completion must release its chunk array.
 for (let i = 0; i < 10; i++) {
  await new Promise(resolve => setTimeout(resolve, 10));
  globalThis.gc();
 }
 await new Promise(resolve => setTimeout(resolve, 10));
 process.kill(Number(readFileSync(pidfile, "utf8")), 0);
 assert.equal(captured.deref() === undefined, true, "completed command retained stdout chunks");
} finally { Buffer.concat = concat; process.kill(Number(readFileSync(pidfile, "utf8"))); }
`

func TestScriptRuntimeOmpControls(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	runtime, err := os.ReadFile("script_runtime.js")
	require.NoError(t, err)
	dir := t.TempDir()
	driver := filepath.Join(dir, "controls.mjs")
	require.NoError(t, os.WriteFile(driver, append(runtime, []byte(scriptOmpControlsDriver)...), 0o600))
	cmd := exec.CommandContext(t.Context(), node, driver, dir)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

const scriptOmpControlsDriver = `
import assert from "node:assert/strict";
import {existsSync} from "node:fs";
const dir=process.argv[2], marker=dir+"/marker";
const command=(script,matcher)=>({matcher,hooks:[{command:process.execPath,args:["-e",script,marker]}]});
const write='require("fs").appendFileSync(process.argv[1],"spawned")';
const config={hooks:{test:[command(write,"bash")]}};
await runHooks("test",{cwd:dir},{matcher:"read"});
assert.equal(existsSync(marker),false);
const abort=new AbortController();abort.abort();
let result=await runHooks("test",{cwd:dir},{collect:true,matcher:"bash",signal:abort.signal});
assert.equal(existsSync(marker),false);assert.match(result.failures.join(" "),/cancel/i);
result=await runHooks("test",{cwd:dir},{collect:true,matcher:"bash",budgetMs:0});
assert.equal(existsSync(marker),false);assert.match(result.failures.join(" "),/budget/i);
config.hooks.test=[command(write,"other"),command('process.stdout.write(JSON.stringify({block:true}))',"bash"),command(write,"bash")];
result=await runHooks("test",{cwd:dir},{collect:true,matcher:"bash",budgetMs:3000,stop:r=>r.block===true});
assert.deepEqual(result.responses,[{block:true}]);assert.equal(existsSync(marker),false);
config.hooks.test=[command('setTimeout(()=>{},10000)',"bash"),command(write,"bash")];
const live = new AbortController(); setTimeout(()=>live.abort(),100);
result=await runHooks("test",{cwd:dir},{collect:true,matcher:"bash",signal:live.signal});
assert.match(result.failures.join(" "),/cancel/i);assert.equal(existsSync(marker),false);
result=await runHooks("test",{cwd:dir},{collect:true,matcher:"bash",budgetMs:100});
assert.match(result.failures.join(" "),/budget/i);assert.equal(existsSync(marker),false);
`
