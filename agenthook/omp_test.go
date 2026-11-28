package agenthook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOmpProfileAndMatcher(t *testing.T) {
	p, ok := LookupProfile(Agent("omp"))
	require.True(t, ok)
	assert.Contains(t, p.SupportedEvents, EventPostToolUseFailure)
	path := filepath.Join(t.TempDir(), "extension.js")
	opts := InstallOptions{ConfigPath: path, Executable: "hook", Arguments: []string{"marker"}, Marker: "marker", Hooks: []Hook{{Event: EventPreToolUse, Matcher: ToolBash}}}
	result, err := PlanInstall(Agent("omp"), opts)
	require.NoError(t, err)
	assert.Contains(t, string(result.Data), `"matcher": "bash"`)
	opts.Hooks[0].Event = EventStop
	_, err = PlanInstall(Agent("omp"), opts)
	require.Error(t, err)
}

func TestOmpResponse(t *testing.T) {
	spec := profiles[Agent("omp")]
	tests := []struct {
		name  string
		event Event
		value any
		want  string
		bad   bool
	}{
		{"deny", EventPreToolUse, PreToolUseOutput{PermissionDecision: PermissionDecisionDeny, PermissionDecisionReason: "no"}, `{"block":true,"reason":"no"}`, false},
		{"context", EventPostToolUse, PostToolUseOutput{AdditionalContext: "context"}, `{"additionalContext":"context"}`, false},
		{"stop", EventStop, StopOutput{Decision: DecisionBlock, Reason: "continue"}, `{"decision":"block","reason":"continue"}`, false},
		{"rewrite", EventPreToolUse, PreToolUseOutput{UpdatedInput: []byte(`{}`)}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodeResponse(spec, tt.event, handledOutput{value: tt.value})
			if tt.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(got))
		})
	}
}

func TestOmpExtensionStopUsesTranscriptIdentity(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agenthook.mjs")
	log := filepath.Join(dir, "events")
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `let s="";process.stdin.on("data",x=>s+=x).on("end",()=>{require("fs").appendFileSync(process.env.OMP_TEST_LOG,s+"\n");if(JSON.parse(s).hook_event_name==="Stop")process.stdout.write('{"decision":"block","reason":"continue"}')})`, "fixture-marker"}, Marker: "fixture-marker", Hooks: []Hook{{Event: EventSessionStart}, {Event: EventStop}}})
	require.NoError(t, err)
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", "process.exit(1)", "other-marker"}, Marker: "other-marker", Hooks: []Hook{{Event: EventStop}}})
	require.NoError(t, err)
	_, err = Uninstall(AgentOmp, path, "other-marker")
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url"; import {writeFileSync} from "node:fs";
 let h={}; (await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});
 let f=process.argv[2]+"/session";writeFileSync(f,"fixture");let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>"transcript",getSessionFile:()=>f}};
 await h.session_start({reason:"startup"},c); let r=await h.session_stop({session_id:"provider",session_file:f,stop_hook_active:true},c); console.log(JSON.stringify(r));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.JSONEq(t, `{"decision":"block","reason":"continue"}`, strings.TrimSpace(string(out)))
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"session_id":"transcript"`)
	assert.NotContains(t, string(data), `"session_id":"provider"`)
	assert.Contains(t, string(data), `"stop_hook_active":true`)
}

func TestOmpConfigPathProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PI_CONFIG_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("OMP_PROFILE", "")
	t.Setenv("PI_PROFILE", "")
	tail := filepath.Join("extensions", "agenthook-omp.js")
	got, err := ConfigPath(AgentOmp)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".omp", "agent", tail), got)
	t.Setenv("PI_PROFILE", "work")
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".omp", "profiles", "work", "agent"))
	got, err = ConfigPath(AgentOmp)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".omp", "agent", tail), got)
	t.Setenv("OMP_PROFILE", "team")
	got, err = ConfigPath(AgentOmp)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".omp", "profiles", "team", "agent", tail), got)
	t.Setenv("OMP_PROFILE", "bad/name")
	_, err = ConfigPath(AgentOmp)
	require.Error(t, err)
}

type ompFixtureHandler struct{ NoopHandler }

func (ompFixtureHandler) PreToolUse(_ context.Context, input PreToolUseInput) (PreToolUseOutput, error) {
	if input.ToolName != ToolBash || input.ToolUseID != "call-1" || !strings.Contains(string(input.ToolInput), "git commit") {
		return PreToolUseOutput{}, fmt.Errorf("unexpected tool input: %+v", input)
	}
	return PreToolUseOutput{AdditionalContext: "fixture context"}, nil
}
func (ompFixtureHandler) Stop(_ context.Context, input StopInput) (StopOutput, error) {
	if input.SessionID != "transcript" || !input.StopHookActive {
		return StopOutput{}, fmt.Errorf("unexpected stop: %+v", input)
	}
	return StopOutput{Decision: DecisionBlock, Reason: "keep working"}, nil
}
func TestOmpFixtureHandler(t *testing.T) {
	if os.Getenv("OMP_FIXTURE_HANDLER") != "1" {
		return
	}
	if err := Handle(t.Context(), AgentOmp, os.Stdin, os.Stdout, ompFixtureHandler{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestOmpExtensionTypedToolAndStop(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "extension.mjs")
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: os.Args[0], Arguments: []string{"-test.run=^TestOmpFixtureHandler$", "fixture-marker"}, Marker: "fixture-marker", Hooks: []Hook{{Event: EventPreToolUse, Matcher: ToolBash}, {Event: EventStop}}})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";import {writeFileSync} from "node:fs";let h={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let f=process.argv[2]+"/session";writeFileSync(f,"fixture");let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:(x)=>{throw Error(x)}},sessionManager:{getSessionId:()=>"transcript",getSessionFile:()=>f}};await h.session_start({},c);console.log(JSON.stringify(await h.tool_call({toolName:"bash",toolCallId:"call-1",input:{command:"git commit -m test"}},c)));console.log(JSON.stringify(await h.session_stop({session_id:"provider",session_file:f,stop_hook_active:true},c)));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_FIXTURE_HANDLER=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"additionalContext":"fixture context"}`, lines[0])
	assert.JSONEq(t, `{"decision":"block","reason":"keep working"}`, lines[1])
}

func TestPiExtensionIgnoresOmpContext(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pi.mjs")
	log := filepath.Join(dir, "log")
	_, err = Install(AgentPi, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `require("fs").appendFileSync(process.env.OMP_TEST_LOG,"x")`, "marker"}, Marker: "marker", Hooks: []Hook{{Event: EventSessionStart}}})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";import {writeFileSync} from "node:fs";let h={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let f=process.argv[2]+"/session";writeFileSync(f,"x");let c={mode:"tui",cwd:process.argv[2],sessionManager:{getSessionId:()=>"id",getSessionFile:()=>f}};await h.session_start({reason:"startup"},{...c,agent:{kind:"main"}});await h.session_start({reason:"startup"},c);`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))
}

func TestOmpToolFailuresAndDecision(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	for _, tc := range []struct {
		marker, script string
		matcher        string
	}{
		{"bad-marker", `process.stdin.resume();process.stdin.on("end",()=>{process.stdout.write('{"block":"yes"}')})`, ""},
		{"failure-marker", `process.stdin.resume();process.stdin.on("end",()=>{process.stderr.write("fixture failure");process.exitCode=1})`, ""},
		{"deny-marker", `process.stdin.resume();process.stdin.on("end",()=>{process.stdout.write('{"block":true,"reason":"denied"}')})`, ToolBash},
	} {
		_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", tc.script, tc.marker}, Marker: tc.marker, Hooks: []Hook{{Event: EventPreToolUse, Matcher: tc.matcher}}})
		require.NoError(t, err)
	}
	driver := `import {pathToFileURL} from "node:url";let h={},errors=[];(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:(x)=>errors.push(x)},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>process.argv[2]+"/missing"}};let e={toolName:"bash",toolCallId:"1",input:{command:"git commit"}};console.log(JSON.stringify(await h.tool_call(e,c)));console.log(JSON.stringify(errors));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"block":true,"reason":"denied"}`, lines[0])
	assert.Contains(t, lines[1], "invalid PreToolUse response")
	assert.Contains(t, lines[1], "fixture failure")
}

func TestOmpSessionAndToolLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	log := filepath.Join(dir, "events")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "other"), 0o700))
	events := []Event{EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventPostToolUse, EventPostToolUseFailure, EventSessionEnd}
	hooks := make([]Hook, 0, len(events))
	for _, event := range events {
		hooks = append(hooks, Hook{Event: event})
	}
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `let s="";process.stdin.on("data",x=>s+=x).on("end",()=>require("fs").appendFileSync(process.env.OMP_TEST_LOG,s+"\n"))`, "lifecycle-marker"}, Marker: "lifecycle-marker", Hooks: hooks})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";import {writeFileSync} from "node:fs";let h={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let id="a",f=process.argv[2]+"/a",c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>id,getSessionFile:()=>f}};await h.session_start({},c);await h.input({source:"interactive",text:"/local"},c);await h.input({source:"extension",text:"hidden"},c);await h.tool_call({toolName:"bash",toolCallId:"1",input:{command:"git commit"}},c);writeFileSync(f,"saved");await h.tool_result({toolName:"bash",toolCallId:"1",input:{command:"git commit"},content:[{type:"text",text:"done"}],isError:false},c);await h.tool_result({toolName:"bash",toolCallId:"2",input:{command:"git commit"},content:[{type:"text",text:"failed"}],isError:true},c);await h.session_switch({reason:"resume"},c);id="b";f=process.argv[2]+"/b";c.cwd=process.argv[2]+"/other";writeFileSync(f,"saved");await h.session_branch({reason:"fork"},c);await h.input({source:"interactive",text:"skip"},{...c,agent:{kind:"subagent"}});`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var entries []map[string]any
	for _, line := range lines {
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &e))
		entries = append(entries, e)
	}
	require.Len(t, entries, 7)
	assert.Equal(t, "SessionStart", entries[0]["hook_event_name"])
	assert.Equal(t, "/local", entries[1]["prompt"])
	assert.Equal(t, "PreToolUse", entries[2]["hook_event_name"])
	assert.Equal(t, "PostToolUse", entries[3]["hook_event_name"])
	assert.Equal(t, "failed", entries[4]["error"])
	assert.Equal(t, "a", entries[5]["session_id"])
	assert.Equal(t, "SessionEnd", entries[5]["hook_event_name"])
	assert.Equal(t, dir, entries[5]["cwd"])
	assert.Equal(t, "b", entries[6]["session_id"])
}

func TestOmpUnsavedLocalInputBeforeBlockedStop(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	log := filepath.Join(dir, "events")
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `let s="";process.stdin.on("data",x=>s+=x).on("end",()=>{const e=JSON.parse(s);require("fs").appendFileSync(process.env.OMP_TEST_LOG,s+"\n");if(e.hook_event_name==="Stop")process.stdout.write('{"decision":"block","reason":"continue"}')})`, "local-marker"}, Marker: "local-marker", Hooks: []Hook{{Event: EventSessionStart}, {Event: EventUserPromptSubmit}, {Event: EventStop}}})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";let h={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>"unsaved",getSessionFile:()=>undefined}};await h.session_start({},c);await h.input({source:"interactive",text:"/help"},c);await h.input({source:"interactive",text:"/quit"},c);console.log(JSON.stringify(await h.session_stop({session_id:"provider",stop_hook_active:false},c)));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.JSONEq(t, `{"decision":"block","reason":"continue"}`, strings.TrimSpace(string(out)))
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 4)
	for i, want := range []string{"SessionStart", "UserPromptSubmit", "UserPromptSubmit", "Stop"} {
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(lines[i]), &e))
		assert.Equal(t, want, e["hook_event_name"])
		assert.Equal(t, "unsaved", e["session_id"])
		assert.NotContains(t, e, "transcript_path")
		if i == 1 {
			assert.Equal(t, "/help", e["prompt"])
		}
		if i == 2 {
			assert.Equal(t, "/quit", e["prompt"])
		}
	}
}

func TestOmpContextMergeAndMatcher(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	for _, tc := range []struct{ marker, output, matcher string }{{"first", `{"additionalContext":" first "}`, ""}, {"duplicate", `{"additionalContext":"first"}`, ""}, {"second", `{"additionalContext":"second"}`, ""}, {"bash", `{"additionalContext":"bash only"}`, ToolBash}} {
		script := fmt.Sprintf(`process.stdin.resume();process.stdin.on("end",()=>process.stdout.write(%q))`, tc.output)
		_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", script, tc.marker}, Marker: tc.marker, Hooks: []Hook{{Event: EventPreToolUse, Matcher: tc.matcher}, {Event: EventPostToolUse, Matcher: tc.matcher}, {Event: EventPostToolUseFailure, Matcher: tc.matcher}}})
		require.NoError(t, err)
	}
	driver := `import {pathToFileURL} from "node:url";let h={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};let e={toolName:"read",toolCallId:"1",input:{},content:[],isError:false};for (const [name,event] of [["tool_call",e],["tool_result",e],["tool_result",{...e,isError:true}]]) console.log(JSON.stringify(await h[name](event,c)));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	require.Len(t, lines, 3)
	for _, line := range lines {
		assert.JSONEq(t, `{"additionalContext":" first \n\nfirst\n\nsecond"}`, line)
	}
}

func TestOmpStopHonorsCancellation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	marker := filepath.Join(dir, "spawned")
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `require("fs").writeFileSync(process.env.OMP_TEST_MARKER,"spawned")`, "stop-marker"}, Marker: "stop-marker", Hooks: []Hook{{Event: EventStop}}})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";let h={},errors=[];(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:x=>errors.push(x)},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};let a=new AbortController();a.abort();await h.session_stop({signal:a.signal,stop_hook_active:false},c);console.log(JSON.stringify(errors));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_MARKER="+marker)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "cancelled")
	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err))
}

func TestOmpDetectsLegacyPiAfterInstall(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	piPath := filepath.Join(dir, filepath.Base(piProfile().profile.ConfigFilename))
	ompPath := filepath.Join(dir, "agenthook-omp.js")
	log := filepath.Join(dir, "events")
	piOpts := InstallOptions{ConfigPath: piPath, Executable: node, Arguments: []string{"-e", "process.stdin.resume()", "pi-marker", "// agenthook:omp-guarded"}, Marker: "pi-marker", Hooks: []Hook{{Event: EventSessionStart}}}
	_, err = Install(AgentPi, piOpts)
	require.NoError(t, err)
	modern, err := os.ReadFile(piPath)
	require.NoError(t, err)
	legacy := strings.Replace(string(modern), "\t\t\t// agenthook:omp-guarded\n", "", 1)
	legacy = strings.Replace(legacy, `if (ctx.mode !== "tui" || ctx.agent?.kind) return;`, `if (ctx.mode !== "tui") return;`, 1)
	require.NotEqual(t, string(modern), legacy)
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: ompPath, Executable: node, Arguments: []string{"-e", `process.stdin.resume();process.stdin.on("end",()=>require("fs").appendFileSync(process.env.OMP_TEST_LOG,"event\n"))`, "omp-marker"}, Marker: "omp-marker", Hooks: []Hook{{Event: EventSessionStart}, {Event: EventUserPromptSubmit}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(piPath, []byte(legacy), 0o600))
	modernPath := filepath.Join(dir, "modern-fixture")
	require.NoError(t, os.WriteFile(modernPath, modern, 0o600))
	driver := `import {pathToFileURL} from "node:url";import {copyFileSync} from "node:fs";let mod=await import(pathToFileURL(process.argv[1])),warnings=[];const init=()=>{let h={};mod.default({on:(n,f)=>h[n]=f});return h};let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:(x)=>warnings.push(x)},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};let h=init();await h.session_start({},c);await h.input({source:"interactive",text:"first"},c);copyFileSync(process.argv[3],process.argv[4]);await h.input({source:"interactive",text:"still blocked"},c);h=init();await h.session_start({},c);await h.input({source:"interactive",text:"recovered"},c);console.log(JSON.stringify(warnings));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, ompPath, dir, modernPath, piPath)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	var warnings []string
	require.NoError(t, json.Unmarshal(out, &warnings))
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], piPath)
	assert.Contains(t, warnings[0], "reinstall")
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, "event\nevent\n", string(data))
}

func TestOmpAndRefreshedPiCoexist(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	piPath := filepath.Join(dir, filepath.Base(piProfile().profile.ConfigFilename))
	ompPath := filepath.Join(dir, "agenthook-omp.js")
	log := filepath.Join(dir, "events")
	for _, tc := range []struct {
		agent        Agent
		path, marker string
	}{{AgentPi, piPath, "pi-marker"}, {AgentOmp, ompPath, "omp-marker"}} {
		script := fmt.Sprintf(`process.stdin.resume();process.stdin.on("end",()=>require("fs").appendFileSync(process.env.OMP_TEST_LOG,%q+"\n"))`, tc.marker)
		_, err = Install(tc.agent, InstallOptions{ConfigPath: tc.path, Executable: node, Arguments: []string{"-e", script, tc.marker}, Marker: tc.marker, Hooks: []Hook{{Event: EventSessionStart}}})
		require.NoError(t, err)
	}
	driver := `import {pathToFileURL} from "node:url";import {writeFileSync} from "node:fs";let pi={},omp={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>pi[n]=f});(await import(pathToFileURL(process.argv[2]))).default({on:(n,f)=>omp[n]=f});let f=process.argv[3]+"/session";writeFileSync(f,"fixture");let c={mode:"tui",cwd:process.argv[3],ui:{notify:console.error},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>f}};await pi.session_start({reason:"startup"},c);await omp.session_start({},c);let o={...c,agent:{kind:"main"}};await pi.session_start({reason:"startup"},o);await omp.session_start({},o);`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, piPath, ompPath, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, "pi-marker\nomp-marker\n", string(data))
}

func TestOmpIgnoresCustomSiblingModule(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	ompPath := filepath.Join(dir, "agenthook-omp.js")
	piPath := filepath.Join(dir, "agenthook.js")
	log := filepath.Join(dir, "events")
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: ompPath, Executable: node, Arguments: []string{"-e", `process.stdin.resume();process.stdin.on("end",()=>require("fs").appendFileSync(process.env.OMP_TEST_LOG,"event\n"))`, "omp-marker"}, Marker: "omp-marker", Hooks: []Hook{{Event: EventSessionStart}}})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";import {writeFileSync} from "node:fs";let mod=await import(pathToFileURL(process.argv[1]));let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:x=>{throw Error(x)}},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};for(const s of ["// custom module\n", "// Generated by go.kenn.io/kit/agenthook; Install and Uninstall rewrite this file.\n// agenthook:begin\nconst config = {};\n// agenthook:end\nexport default function (custom) {}\n"]){writeFileSync(process.argv[3],s);let h={};mod.default({on:(n,f)=>h[n]=f});await h.session_start({},c)}`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, ompPath, dir, piPath)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, "event\nevent\n", string(data))
}

func TestOmpReportsWhenSiblingCannotBeRead(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	ompPath := filepath.Join(dir, "agenthook-omp.js")
	log := filepath.Join(dir, "events")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "agenthook.js"), 0o700))
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: ompPath, Executable: node, Arguments: []string{"-e", `process.stdin.resume();process.stdin.on("end",()=>require("fs").appendFileSync(process.env.OMP_TEST_LOG,"event\n"))`, "omp-marker"}, Marker: "omp-marker", Hooks: []Hook{{Event: EventSessionStart}}})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";let warnings=[],h={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:x=>warnings.push(x)},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};await h.session_start({},c);await h.session_start({},c);console.log(JSON.stringify(warnings));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, ompPath, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	var warnings []string
	require.NoError(t, json.Unmarshal(out, &warnings))
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "cannot inspect")
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, "event\nevent\n", string(data))
}
