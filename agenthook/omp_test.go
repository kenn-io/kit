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
	events := []Event{EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventPostToolUse, EventPostToolUseFailure, EventSessionEnd}
	hooks := make([]Hook, 0, len(events))
	for _, event := range events {
		hooks = append(hooks, Hook{Event: event})
	}
	_, err = Install(AgentOmp, InstallOptions{ConfigPath: path, Executable: node, Arguments: []string{"-e", `let s="";process.stdin.on("data",x=>s+=x).on("end",()=>require("fs").appendFileSync(process.env.OMP_TEST_LOG,s+"\n"))`, "lifecycle-marker"}, Marker: "lifecycle-marker", Hooks: hooks})
	require.NoError(t, err)
	driver := `import {pathToFileURL} from "node:url";import {writeFileSync} from "node:fs";let h={};(await import(pathToFileURL(process.argv[1]))).default({on:(n,f)=>h[n]=f});let id="a",f=process.argv[2]+"/a",c={mode:"tui",agent:{kind:"main"},cwd:process.argv[2],ui:{notify:console.error},sessionManager:{getSessionId:()=>id,getSessionFile:()=>f}};await h.session_start({},c);await h.input({source:"interactive",text:"/local"},c);await h.input({source:"extension",text:"hidden"},c);await h.tool_call({toolName:"bash",toolCallId:"1",input:{command:"git commit"}},c);writeFileSync(f,"saved");await h.tool_result({toolName:"bash",toolCallId:"1",input:{command:"git commit"},content:[{type:"text",text:"done"}],isError:false},c);await h.tool_result({toolName:"bash",toolCallId:"2",input:{command:"git commit"},content:[{type:"text",text:"failed"}],isError:true},c);await h.session_switch({reason:"resume"},c);id="b";f=process.argv[2]+"/b";writeFileSync(f,"saved");await h.session_branch({reason:"fork"},c);await h.input({source:"interactive",text:"skip"},{...c,agent:{kind:"subagent"}});`
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
	assert.Equal(t, "PreToolUse", entries[0]["hook_event_name"])
	assert.Equal(t, "SessionStart", entries[1]["hook_event_name"])
	assert.Equal(t, "/local", entries[2]["prompt"])
	assert.Equal(t, "PostToolUse", entries[3]["hook_event_name"])
	assert.Equal(t, "failed", entries[4]["error"])
	assert.Equal(t, "a", entries[5]["session_id"])
	assert.Equal(t, "SessionEnd", entries[5]["hook_event_name"])
	assert.Equal(t, "b", entries[6]["session_id"])
}
