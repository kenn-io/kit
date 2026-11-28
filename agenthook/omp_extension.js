import { existsSync } from "node:fs";

// OMP v19.2.1 event contracts:
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/extensibility/shared-events.ts#L26-L83
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/extensibility/extensions/types.ts#L1455-L1461
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/extensibility/extensions/types.ts#L1585-L1650
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/extensibility/shared-events.ts#L121-L132
export default function (omp) {
 let held = null, prompts = [], announced = null;
 const identity = (ctx) => ({ id: ctx.sessionManager.getSessionId(), file: ctx.sessionManager.getSessionFile() });
 const payload = (name, ctx, extra = {}) => {
  const {id, file} = identity(ctx);
  return {hook_event_name:name, session_id:id, cwd:ctx.cwd, ...(file ? {transcript_path:file} : {}), ...extra};
 };
 const valid = (name, value) => {
  const allowed = name === "PreToolUse" ? ["block", "reason", "additionalContext"] : name === "Stop" ? ["decision", "reason"] : name === "PostToolUse" || name === "PostToolUseFailure" ? ["additionalContext"] : [];
  return Object.keys(value).every((k) => allowed.includes(k)) &&
   (value.block === undefined || typeof value.block === "boolean") &&
   (value.reason === undefined || typeof value.reason === "string") &&
   (value.additionalContext === undefined || typeof value.additionalContext === "string") &&
   (value.decision === undefined || value.decision === "block") &&
   (value.reason === undefined || (name === "Stop" ? value.decision === "block" : value.block === true));
 };
 const on = (name, fn) => omp.on(name, async (event, ctx) => {
  if (ctx.mode !== "tui" || ctx.agent?.kind !== "main") return;
  const deadline = Date.now() + 25000;
  const run = async (eventName, data, matcher, signal) => {
   const invalid = [];
   const stop = (value) => {
    if (!valid(eventName, value)) { invalid.push("invalid " + eventName + " response"); return false; }
    return value.block === true || value.decision === "block";
   };
   const result = await runHooks(eventName, data, {collect:true, matcher, signal, budgetMs:Math.max(0, deadline-Date.now()), stop});
   for (const failure of [...result.failures, ...invalid]) ctx.ui.notify("agenthook: " + failure, "error");
   const responses = result.responses.filter((value) => valid(eventName, value));
   const block = responses.find((value) => value.block === true || value.decision === "block");
   if (block) return block;
   const seen = new Set(), contexts = [];
   for (const value of responses) {
    const context = value.additionalContext, key = context?.trim();
    if (key && !seen.has(key)) { seen.add(key); contexts.push(context); }
   }
   return contexts.length ? {additionalContext:contexts.join("\n\n")} : undefined;
  };
  const flush = async () => {
   const now = identity(ctx);
   if (!now.file || !existsSync(now.file)) return;
   if (held) { const start = held; held = null; announced = now; await run("SessionStart", start); }
   for (const prompt of prompts.splice(0)) await run("UserPromptSubmit", prompt);
  };
  try { return await fn(event,ctx,run,flush); }
  catch (error) { ctx.ui.notify("agenthook: " + error.message, "error"); }
 });
 on("session_start", async (_event,ctx,_run,flush) => {
  const now = identity(ctx); if (!now.file) return;
  held = payload("SessionStart",ctx,{source:existsSync(now.file) ? "resume" : "startup"});
  await flush();
 });
 const replace = async (event,ctx,run,flush) => {
  const now = identity(ctx);
  if ((announced && announced.id === now.id && announced.file === now.file) || (held && held.session_id === now.id && held.transcript_path === now.file)) return;
  if (announced) await run("SessionEnd", {hook_event_name:"SessionEnd",session_id:announced.id,transcript_path:announced.file,cwd:ctx.cwd,reason:event.reason === "new" ? "clear" : event.reason === "resume" ? "resume" : "other"});
  announced = null; prompts = []; held = now.file ? payload("SessionStart",ctx,{source:event.reason === "new" ? "clear" : event.reason === "resume" ? "resume" : "fork"}) : null;
  await flush();
 };
 on("session_switch",replace); on("session_branch",replace);
 on("input", async (event,ctx,_run,flush) => {
  if (!identity(ctx).file || event.source !== "interactive" || typeof event.text !== "string" || !event.text) return;
  prompts.push(payload("UserPromptSubmit",ctx,{prompt:event.text})); await flush();
 });
 const toolFields = (event) => ({tool_name:event.toolName, tool_use_id:event.toolCallId, tool_input:event.input});
 on("tool_call", async (event,ctx,run,flush) => {
  const result = await run("PreToolUse",payload("PreToolUse",ctx,toolFields(event)),event.toolName);
  if (result?.block) return result;
  await flush(); return result;
 });
 on("tool_result", async (event,ctx,run,flush) => {
  await flush(); const name = event.isError ? "PostToolUseFailure" : "PostToolUse";
  const text = event.content?.filter((part) => part.type === "text").map((part) => part.text).join("\n");
  const result = event.isError ? {error:text || (event.details?.exitCode != null ? `exit code ${event.details.exitCode}` : "tool failed")} : {tool_response:{content:event.content,details:event.details}};
  return run(name,payload(name,ctx,{...toolFields(event),...result}),event.toolName);
 });
 on("session_stop", async (event,ctx,run,flush) => {
  const content = event.last_assistant_message?.content;
  const text = typeof content === "string" ? content : Array.isArray(content) ? content.filter((part) => part.type === "text").map((part) => part.text).join("\n") : "";
  const result = await run("Stop",payload("Stop",ctx,{...(event.session_file ? {transcript_path:event.session_file} : {}),stop_hook_active:!!event.stop_hook_active,...(text ? {last_assistant_message:text} : {})}),undefined,event.signal);
  if (result?.decision === "block" || event.signal?.aborted) return result;
  await flush(); return result;
 });
}
