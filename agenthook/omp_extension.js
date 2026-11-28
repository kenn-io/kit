import { existsSync } from "node:fs";

const identity = (ctx) => ({ id: ctx.sessionManager.getSessionId(), file: ctx.sessionManager.getSessionFile(), cwd: ctx.cwd });
const lifecycle = (name, { id, file, cwd }, extra = {}) => ({ hook_event_name: name, session_id: id, cwd, ...(file ? { transcript_path: file } : {}), ...extra });

const valid = (name, value) => {
	const allowed = name === "PreToolUse" ? ["block", "reason", "additionalContext"] : name === "Stop" ? ["decision", "reason"] : name === "PostToolUse" || name === "PostToolUseFailure" ? ["additionalContext"] : [];
	return Object.keys(value).every((key) => allowed.includes(key)) &&
		(value.block === undefined || typeof value.block === "boolean") &&
		(value.reason === undefined || typeof value.reason === "string") &&
		(value.additionalContext === undefined || typeof value.additionalContext === "string") &&
		(value.decision === undefined || value.decision === "block") &&
		(value.reason === undefined || (name === "Stop" ? value.decision === "block" : value.block === true));
};

// OMP v19.2.1 event contracts:
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/extensibility/shared-events.ts#L26-L83
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/extensibility/extensions/types.ts#L1455-L1461
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/extensibility/extensions/types.ts#L1585-L1650
// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/extensibility/shared-events.ts#L121-L132
export default function (omp) {
	let announced = null;
	const payload = (name, ctx, extra) => lifecycle(name, identity(ctx), extra);
	const on = (name, fn) => omp.on(name, async (event, ctx) => {
		if (ctx.mode !== "tui" || ctx.agent?.kind !== "main") return;
		const deadline = Date.now() + 25000;
		const run = async (eventName, data, reserveStart = false) => {
			const remaining = Math.max(0, deadline - Date.now());
			try { await runHooks(eventName, data, { budgetMs: reserveStart ? remaining / 2 : remaining }); }
			catch (error) { ctx.ui.notify("agenthook: " + error.message, "error"); }
		};
		try { return await fn(event, ctx, run); }
		catch (error) { ctx.ui.notify("agenthook: " + error.message, "error"); }
	});
	const decision = (native, name, makePayload) => {
		for (const entry of config.hooks?.[name] ?? []) for (const handler of entry.hooks) {
			omp.on(native, async (event, ctx) => {
				try {
					if (ctx.mode !== "tui" || ctx.agent?.kind !== "main" || (entry.matcher && entry.matcher !== event.toolName)) return;
					const data = makePayload(event, ctx);
					if (!data) return;
					const timeout = Math.min(handler.timeout > 0 ? handler.timeout : 25, 25);
					const { failure, response } = await runCommand({ ...handler, timeout }, data, { collect: true, signal: event.signal });
					if (failure) { ctx.ui.notify("agenthook: " + [handler.command, ...handler.args].join(" ") + ": " + failure, "error"); return; }
					if (response === undefined) return;
					if (!valid(name, response)) { ctx.ui.notify("agenthook: invalid " + name + " response", "error"); return; }
					return response;
				} catch (error) { ctx.ui.notify("agenthook: " + error.message, "error"); }
			});
		}
	};
	// OMP assigns a session-manager ID before its lazy transcript write:
	// https://github.com/can1357/oh-my-pi/blob/v19.2.1/packages/coding-agent/src/session/session-manager.ts#L3357-L3374
	on("session_start", async (_event, ctx, run) => {
		announced = identity(ctx);
		await run("SessionStart", payload("SessionStart", ctx, { source: announced.file && existsSync(announced.file) ? "resume" : "startup" }));
	});
	const replace = async (event, ctx, run) => {
		const now = identity(ctx);
		if (announced?.id === now.id) return;
		const ended = announced;
		announced = now;
		const hasCommands = (name) => (config.hooks?.[name] ?? []).some((entry) => entry.hooks.length > 0);
		// Reserve half the remaining callback time for Start when both phases have work.
		const reserveStart = ended && hasCommands("SessionEnd") && hasCommands("SessionStart");
		if (ended) {
			const reason = event.reason === "new" ? "clear" : event.reason === "resume" ? "resume" : "other";
			await run("SessionEnd", lifecycle("SessionEnd", ended, { reason }), reserveStart);
		}
		const source = event.reason === "new" ? "clear" : event.reason === "resume" ? "resume" : "fork";
		await run("SessionStart", lifecycle("SessionStart", now, { source }));
	};
	// Native /move keeps the session ID but changes cwd and transcript. OMP emits
	// the before events while the old session is still current, so refresh the
	// announced location there and leave End/Start to the after events.
	const relocate = async (_event, ctx) => {
		const current = identity(ctx);
		if (announced?.id === current.id) announced = current;
	};
	on("session_before_switch", relocate);
	on("session_before_branch", relocate);
	on("session_switch", replace);
	on("session_branch", replace);
	on("input", async (event, ctx, run) => {
		if (event.source !== "interactive" || typeof event.text !== "string" || (!event.text && !(Array.isArray(event.images) && event.images.length))) return;
		await run("UserPromptSubmit", payload("UserPromptSubmit", ctx, { prompt: event.text }));
	});
	const toolFields = (event) => ({ tool_name: event.toolName, tool_use_id: event.toolCallId, tool_input: event.input });
	decision("tool_call", "PreToolUse", (event, ctx) => payload("PreToolUse", ctx, toolFields(event)));
	for (const name of ["PostToolUse", "PostToolUseFailure"]) decision("tool_result", name, (event, ctx) => {
		if ((name === "PostToolUseFailure") !== !!event.isError) return;
		const text = event.content?.filter((part) => part.type === "text").map((part) => part.text).join("\n");
		const result = event.isError ? { error: text || (event.details?.exitCode != null ? `exit code ${event.details.exitCode}` : "tool failed") } : { tool_response: { content: event.content, details: event.details } };
		return payload(name, ctx, { ...toolFields(event), ...result });
	});
	decision("session_stop", "Stop", (event, ctx) => {
		const content = event.last_assistant_message?.content;
		const text = typeof content === "string" ? content : Array.isArray(content) ? content.filter((part) => part.type === "text").map((part) => part.text).join("\n") : "";
		return payload("Stop", ctx, { ...(event.session_file ? { transcript_path: event.session_file } : {}), stop_hook_active: !!event.stop_hook_active, ...(text ? { last_assistant_message: text } : {}) });
	});
}
