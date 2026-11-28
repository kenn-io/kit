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
	"time"

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
	opts.Hooks[0].Event = EventSessionStart
	_, err = PlanInstall(Agent("omp"), opts)
	require.ErrorContains(t, err, "omp hooks accept the Bash matcher only on tool events")
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
	require.NoError(t, os.Unsetenv("OMP_PROFILE"))
	got, err = ConfigPath(AgentOmp)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".omp", "profiles", "work", "agent", tail), got)
	t.Setenv("OMP_PROFILE", "")
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".omp", "profiles", "work", "agent"))
	got, err = ConfigPath(AgentOmp)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".omp", "agent", tail), got)
	t.Setenv("OMP_PROFILE", "team")
	got, err = ConfigPath(AgentOmp)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".omp", "profiles", "team", "agent", tail), got)
	t.Setenv("PI_CONFIG_DIR", ".custom")
	got, err = ConfigPath(AgentOmp)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".custom", "profiles", "team", "agent", tail), got)
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
	driver := ompHost + `import {writeFileSync} from "node:fs";let f=process.argv[2]+"/session";writeFileSync(f,"fixture");let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:(x)=>{throw Error(x)}},sessionManager:{getSessionId:()=>"transcript",getSessionFile:()=>f}};await h.session_start({},c);console.log(JSON.stringify(await h.tool_call({toolName:"bash",toolCallId:"call-1",input:{command:"git commit -m test"}},c)));console.log(JSON.stringify(await h.session_stop({session_id:"provider",session_file:f,stop_hook_active:true},c)));`
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
	driver := ompHost + `import {writeFileSync} from "node:fs";let f=process.argv[2]+"/session";writeFileSync(f,"x");let c={mode:"tui",cwd:process.argv[2],sessionManager:{getSessionId:()=>"id",getSessionFile:()=>f}};await h.session_start({reason:"startup"},{...c,agent:{kind:"main"}});await h.session_start({reason:"startup"},c);`
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
	driver := ompHost + `let errors=[];let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:(x)=>errors.push(x)},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>process.argv[2]+"/missing"}};let e={toolName:"bash",toolCallId:"1",input:{command:"git commit"}};console.log(JSON.stringify(await h.tool_call(e,c)));console.log(JSON.stringify(errors));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"block":true,"reason":"denied"}`, lines[0])
	assert.Contains(t, lines[1], "invalid PreToolUse response")
	assert.Contains(t, lines[1], "fixture failure")
	t.Run("context and matcher", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node unavailable")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "omp.mjs")
		for _, tc := range []struct{ marker, output, matcher string }{{"first", `{"additionalContext":" first "}`, ""}, {"second", `{"additionalContext":"second"}`, ""}, {"bash", `{"additionalContext":"bash only"}`, ToolBash}} {
			script := fmt.Sprintf(`process.stdin.resume();process.stdin.on("end",()=>process.stdout.write(%q))`, tc.output)
			_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", script, tc.marker}, Marker: tc.marker, Hooks: []Hook{{Event: EventPreToolUse, Matcher: tc.matcher}}})
			require.NoError(t, err)
		}
		driver := ompHost + `let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};let e={toolName:"read",toolCallId:"1",input:{}};for(const fn of handlers.tool_call)console.log(JSON.stringify((await fn(e,c)) ?? null));`
		cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		require.Len(t, lines, 3)
		assert.JSONEq(t, `{"additionalContext":" first "}`, lines[0])
		assert.JSONEq(t, `{"additionalContext":"second"}`, lines[1])
		assert.JSONEq(t, `null`, lines[2])
	})
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
	events := []Event{EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventPostToolUse, EventPostToolUseFailure, EventSessionEnd, EventStop}
	hooks := make([]Hook, 0, len(events))
	for _, event := range events {
		hooks = append(hooks, Hook{Event: event})
	}
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `let s="";process.stdin.on("data",x=>s+=x).on("end",()=>require("fs").appendFileSync(process.env.OMP_TEST_LOG,s+"\n"))`, "lifecycle-marker"}, Marker: "lifecycle-marker", Hooks: hooks})
	require.NoError(t, err)
	driver := ompHost + `import {writeFileSync} from "node:fs";let id="a",f=process.argv[2]+"/a",c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>id,getSessionFile:()=>f}};await h.session_start({},c);await h.input({source:"interactive",text:"/local"},c);await h.input({source:"extension",text:"hidden"},c);await h.tool_call({toolName:"bash",toolCallId:"1",input:{command:"git commit"}},c);writeFileSync(f,"saved");await h.tool_result({toolName:"bash",toolCallId:"1",input:{command:"git commit"},content:[{type:"text",text:"done"}],isError:false},c);await h.tool_result({toolName:"bash",toolCallId:"2",input:{command:"git commit"},content:[{type:"text",text:"failed"}],isError:true},c);await h.session_switch({reason:"resume"},c);f=process.argv[2]+"/other/a-moved";c.cwd=process.argv[2]+"/other";writeFileSync(f,"saved");await h.session_before_switch?.({reason:"resume"},c);id="b";f=process.argv[2]+"/b";c.cwd=process.argv[2];writeFileSync(f,"saved");await h.session_switch({reason:"resume"},c);await h.input({source:"interactive",text:"skip"},{...c,agent:{kind:"subagent"}});id="unsaved";f=undefined;await h.session_switch({reason:"new"},c);await h.input({source:"interactive",text:"/help"},c);await h.session_stop({stop_hook_active:false},c);`
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
	require.Len(t, entries, 11)
	assert.Equal(t, "SessionStart", entries[0]["hook_event_name"])
	assert.Equal(t, "/local", entries[1]["prompt"])
	assert.Equal(t, "PreToolUse", entries[2]["hook_event_name"])
	assert.Equal(t, "PostToolUse", entries[3]["hook_event_name"])
	assert.Equal(t, "failed", entries[4]["error"])
	assert.Equal(t, "a", entries[5]["session_id"])
	assert.Equal(t, "SessionEnd", entries[5]["hook_event_name"])
	assert.Equal(t, filepath.Join(dir, "other"), entries[5]["cwd"])
	assert.Equal(t, filepath.Join(dir, "other", "a-moved"), entries[5]["transcript_path"])
	assert.Equal(t, "SessionStart", entries[6]["hook_event_name"])
	assert.Equal(t, "b", entries[6]["session_id"])
	assert.Equal(t, dir, entries[6]["cwd"])
	assert.Equal(t, filepath.Join(dir, "b"), entries[6]["transcript_path"])
	assert.Equal(t, "SessionEnd", entries[7]["hook_event_name"])
	for _, entry := range entries[8:] {
		assert.Equal(t, "unsaved", entry["session_id"])
		assert.NotContains(t, entry, "transcript_path")
	}
	assert.Equal(t, "SessionStart", entries[8]["hook_event_name"])
	assert.Equal(t, "UserPromptSubmit", entries[9]["hook_event_name"])
	assert.Equal(t, "/help", entries[9]["prompt"])
	assert.Equal(t, "Stop", entries[10]["hook_event_name"])
}

func TestOmpImageOnlyInteractiveInput(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	log := filepath.Join(dir, "events")
	require.NoError(t, os.WriteFile(log, nil, 0o600))
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `let s="";process.stdin.on("data",x=>s+=x).on("end",()=>require("fs").appendFileSync(process.env.OMP_TEST_LOG,s+"\n"))`, "image-marker"}, Marker: "image-marker", Hooks: []Hook{{Event: EventUserPromptSubmit}}})
	require.NoError(t, err)
	driver := ompHost + `let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};let images=[{type:"image",data:"private-image-data",mimeType:"image/png"}];await h.input({source:"interactive",text:"",images},c);await h.input({source:"interactive",text:"",images:[]},c);await h.input({source:"extension",text:"",images},c);await h.input({source:"rpc",text:"",images},c);`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	require.NotEmpty(t, data, "image-only input must reach the hook")
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 1)
	var event map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &event))
	assert.Equal(t, "UserPromptSubmit", event["hook_event_name"])
	assert.Equal(t, "", event["prompt"])
	assert.NotContains(t, string(data), "private-image-data")
	assert.NotContains(t, event, "images")
}

func TestOmpHandleEmptyPrompt(t *testing.T) {
	var output strings.Builder
	handler := &agyRecorder{}
	err := Handle(t.Context(), AgentOmp, strings.NewReader(`{"session_id":"id","hook_event_name":"UserPromptSubmit","prompt":""}`), &output, handler)
	require.NoError(t, err)
	require.NotNil(t, handler.prompt)
	assert.Empty(t, handler.prompt.Prompt)
	assert.JSONEq(t, `{}`, output.String())
}

const ompHost = `import {pathToFileURL} from "node:url";let h={},handlers={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>{(handlers[n]??=[]).push(f);h[n]=async(e,c)=>{let result;for(const fn of handlers[n]){const r=await fn(e,c);if(r?.block||r?.decision==="block")return r;if(r)result=r}return result}}});`

func TestOmpStopHonorsCancellation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	marker := filepath.Join(dir, "spawned")
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `setTimeout(()=>require("fs").writeFileSync(process.env.OMP_TEST_MARKER,"spawned"),1000)`, "stop-marker"}, Marker: "stop-marker", Hooks: []Hook{{Event: EventStop}}})
	require.NoError(t, err)
	driver := ompHost + `let errors=[];let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:x=>errors.push(x)},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};let a=new AbortController();a.abort();await h.session_stop({signal:a.signal,stop_hook_active:false},c);let live=new AbortController();setTimeout(()=>live.abort(),100);await h.session_stop({signal:live.signal,stop_hook_active:false},c);await new Promise(r=>setTimeout(r,1100));console.log(JSON.stringify(errors));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_MARKER="+marker)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, 2, strings.Count(string(out), "aborted"))
	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err))
}

func TestOmpStopWithLegacyPiStopOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	piPath := filepath.Join(dir, "agenthook.js")
	ompPath := filepath.Join(dir, "omp.mjs")
	_, err = Install(AgentPi, InstallOptions{ConfigPath: piPath, Executable: node, Arguments: []string{"-e", "process.stdin.resume()", "pi-marker"}, Marker: "pi-marker", Hooks: []Hook{{Event: EventStop}}})
	require.NoError(t, err)
	pi, err := os.ReadFile(piPath)
	require.NoError(t, err)
	legacy := strings.Replace(string(pi), `if (ctx.mode !== "tui" || ctx.agent?.kind) return;`, `if (ctx.mode !== "tui") return;`, 1)
	require.NotEqual(t, string(pi), legacy)
	require.NoError(t, os.WriteFile(piPath, []byte(legacy), 0o600))
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: ompPath, Executable: node, Arguments: []string{"-e", `process.stdin.resume();process.stdin.on("end",()=>process.stdout.write('{"decision":"block","reason":"continue"}'))`, "omp-marker"}, Marker: "omp-marker", Hooks: []Hook{{Event: EventStop}}})
	require.NoError(t, err)
	driver := ompHost + `let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:()=>{}},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}};console.log(JSON.stringify(await h.session_stop({stop_hook_active:false},c)));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, ompPath, dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.JSONEq(t, `{"decision":"block","reason":"continue"}`, strings.TrimSpace(string(out)))
}

func TestOmpDecisionTimeoutCap(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `process.stdin.resume();process.stdin.on("end",()=>process.stdout.write('{"block":true,"reason":"no"}'))`, "first"}, Marker: "first", Hooks: []Hook{{Event: EventPreToolUse, Timeout: 60 * time.Second}}})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";let handlers=[],timers=[],original=globalThis.setTimeout;globalThis.setTimeout=(f,ms,...args)=>{timers.push(ms);return original(f,ms,...args)};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>{if(n==="tool_call")handlers.push(f)}});let c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>"id",getSessionFile:()=>undefined}},result;result=await handlers[0]({toolName:"bash",input:{}},c);console.log(JSON.stringify({result,timeout:timers[0]}));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.JSONEq(t, `{"result":{"block":true,"reason":"no"},"timeout":25000}`, strings.TrimSpace(string(out)))
}

func TestOmpReplacementSharesDeadlineAndKillsChild(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "omp.mjs")
	marker := filepath.Join(dir, "late")
	log := filepath.Join(dir, "starts")
	script := `let s="";process.stdin.on("data",x=>s+=x).on("end",()=>{let e=JSON.parse(s);if(e.hook_event_name==="SessionEnd")setTimeout(()=>require("fs").writeFileSync(process.env.OMP_TEST_MARKER,"alive"),1000);else require("fs").appendFileSync(process.env.OMP_TEST_LOG,e.session_id)})`
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", script, "marker"}, Marker: "marker", Hooks: []Hook{{Event: EventSessionStart}, {Event: EventSessionEnd}}})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";let h={},errors=[],original=globalThis.setTimeout,now=Date.now,offset=0;Date.now=()=>now()+offset;(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let id="a",c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:x=>errors.push(x)},sessionManager:{getSessionId:()=>id,getSessionFile:()=>undefined}};await h.session_start({},c);globalThis.setTimeout=(f,ms,...args)=>ms>=12400&&ms<=25000?original(()=>{offset=ms+1;f()},200,...args):original(f,ms,...args);id="b";await h.session_switch({reason:"new"},c);await new Promise(r=>original(r,1200));console.log(JSON.stringify(errors));`
	cmd := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", driver, path, dir)
	cmd.Env = append(os.Environ(), "OMP_TEST_MARKER="+marker, "OMP_TEST_LOG="+log)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "callback budget exceeded")
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, "ab", string(data))
	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err))
}
