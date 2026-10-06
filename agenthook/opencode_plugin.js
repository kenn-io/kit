// OpenCode 2.x TUI plugin API:
// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/plugin/src/tui/context.ts
const routePollMilliseconds = 500;

export default {
	id: "kenn.agenthook",
	setup(api) {
		// The TUI hears every terminal's sessions, so report only the root of the
		// session this terminal shows; subagents are child sessions with parentID.
		let root;
		let cwd;
		let pending = Promise.resolve();
		const emit = (event, fields) => {
			const payload = { hook_event_name: event, session_id: root, cwd, ...fields };
			// A failed report must not block later ones; OpenCode shows plugin
			// errors as error toasts, so this one does too:
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/plugin/src/tui/context.ts#L271-L282
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/tui/src/plugin/context.tsx#L235-L238
			pending = pending
				.then(() => runHooks(event, payload))
				.catch((error) => {
					try {
						api.ui.toast.show({ variant: "error", title: "Plugin", message: `kenn.agenthook: ${error.message}` });
					} catch {}
				});
		};
		const sync = () => {
			const route = api.ui.router.current();
			if (route?.type !== "session") return;
			const id = api.data.session.root(route.sessionID);
			const info = api.data.session.get(id);
			if (!info || info.parentID || id === root) return;
			root = id;
			cwd = info.location?.directory;
			emit("SessionStart", {});
		};
		// Interrupted and failed runs stay unmapped: Claude's Stop skips user
		// interrupts, and API failures are StopFailure:
		// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/schema/src/session-event.ts#L215-L258
		const unsubscribe = api.data.listen(({ details }) => {
			sync();
			if (!root || details?.data?.sessionID !== root) return;
			if (details.type === "session.inbox.enqueued") {
				// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/schema/src/session-inbox.ts#L37
				const item = details.data.item;
				const text = item?.payload?.text;
				if (item?.type === "user" && typeof text === "string" && text !== "") {
					emit("UserPromptSubmit", { prompt: text });
				}
			} else if (details.type === "session.execution.succeeded") {
				emit("Stop", {});
			}
		});
		const timer = setInterval(sync, routePollMilliseconds);
		sync();
		// SessionEnd stays unmapped: cleanup also runs on every hot reload,
		// including the one a reinstall triggers.
		return () => {
			clearInterval(timer);
			unsubscribe();
			return pending;
		};
	},
};
