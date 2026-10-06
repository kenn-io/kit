import { existsSync } from "node:fs";

// Pi extension API: https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md
// Pi reports a handler's thrown error as an extension error and still runs the
// event and its other handlers:
// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/extensions/runner.ts#L1089-L1117
// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/extensions/runner.ts#L1458-L1466
const replacedSessionReasons = ["new", "resume", "fork"];

// Pi calls this once per session runtime and tears the runtime down when
// another session replaces it, so this state belongs to one session.
export default function (pi) {
	// A SessionStart held until the session file exists, so a reported ID always
	// resumes. Pi writes the file once the session holds a user message:
	// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/session-manager.ts#L1160-L1185
	let held = null;
	let reported = false;

	const payloadFor = (name, ctx) => {
		const payload = {
			hook_event_name: name,
			session_id: ctx.sessionManager.getSessionId(),
			cwd: ctx.cwd,
		};
		const transcript = ctx.sessionManager.getSessionFile();
		if (transcript) payload.transcript_path = transcript;
		return payload;
	};

	// Subagents run Pi in json or print mode with global extensions loaded;
	// only the interactive session is one the user can resume:
	// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/extensions/types.ts#L323-L335
	const on = (name, handler) => pi.on(name, (event, ctx) => (ctx.mode === "tui" ? handler(event, ctx) : undefined));

	on("session_start", async (event, ctx) => {
		const payload = { ...payloadFor("session_start", ctx), reason: event.reason };
		if (!payload.transcript_path || !existsSync(payload.transcript_path)) {
			held = payload;
			return;
		}
		held = null;
		reported = true;
		await runHooks("session_start", payload);
	});

	// before_agent_start runs just before Pi persists the turn's user message:
	// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/agent-session.ts#L2060-L2109
	// https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/agent-session.ts#L1154-L1161
	on("before_agent_start", async (event, ctx) => {
		const failures = [];
		if (held && held.session_id === ctx.sessionManager.getSessionId()) {
			const start = held;
			held = null;
			reported = true;
			await runHooks("session_start", start).catch((error) => failures.push(error.message));
		}
		if (typeof event.prompt === "string" && event.prompt !== "") {
			const payload = { ...payloadFor("before_agent_start", ctx), prompt: event.prompt };
			await runHooks("before_agent_start", payload).catch((error) => failures.push(error.message));
		}
		if (failures.length > 0) throw new Error(failures.join("; "));
	});

	on("agent_settled", (_event, ctx) => (reported ? runHooks("agent_settled", payloadFor("agent_settled", ctx)) : undefined));

	// Report a shutdown only for a reported session that another session
	// replaces; quit and reload keep the session resumable.
	on("session_shutdown", async (event, ctx) => {
		if (!reported || !replacedSessionReasons.includes(event.reason)) return;
		await runHooks("session_shutdown", { ...payloadFor("session_shutdown", ctx), reason: event.reason });
	});
}
