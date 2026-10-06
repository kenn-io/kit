// Pi extension API: https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/extensions.md
export default function (pi) {
	for (const name of Object.keys(config.hooks ?? {})) {
		pi.on(name, async (event, ctx) => {
			// Subagents run Pi in json or print mode with global extensions loaded;
			// only the interactive session is one the user can resume:
			// https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/src/core/extensions/types.ts#L323-L335
			if (ctx.mode !== "tui") return;
			const payload = {
				hook_event_name: name,
				session_id: ctx.sessionManager.getSessionId(),
				cwd: ctx.cwd,
			};
			const transcript = ctx.sessionManager.getSessionFile();
			if (transcript) payload.transcript_path = transcript;
			if (typeof event.reason === "string") payload.reason = event.reason;
			if (name === "before_agent_start") {
				if (typeof event.prompt !== "string" || event.prompt === "") return;
				payload.prompt = event.prompt;
			}
			await runHooks(name, payload);
		});
	}
}
