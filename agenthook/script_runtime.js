import { spawn } from "node:child_process";

const defaultTimeoutSeconds = 60;

// Runs one argv without a shell, writing the payload to stdin. At its timeout
// the child is force-killed and the wait ends even if it has not exited, so a
// child that ignores SIGTERM cannot block later commands or the harness.
function runCommand(handler, payload) {
	return new Promise((resolve) => {
		let child;
		try {
			child = spawn(handler.command, Array.isArray(handler.args) ? handler.args : [], {
				stdio: ["pipe", "ignore", "ignore"],
			});
		} catch {
			resolve();
			return;
		}
		const seconds = handler.timeout > 0 ? handler.timeout : defaultTimeoutSeconds;
		const timer = setTimeout(() => {
			child.kill("SIGKILL");
			resolve();
		}, seconds * 1000);
		const done = () => {
			clearTimeout(timer);
			resolve();
		};
		child.once("error", done);
		child.once("close", done);
		if (child.stdin) {
			child.stdin.on("error", () => {});
			child.stdin.end(JSON.stringify(payload));
		}
	});
}

// Runs every registered command for a native event in order.
async function runHooks(event, payload) {
	const entries = (config.hooks ?? {})[event];
	for (const entry of Array.isArray(entries) ? entries : []) {
		for (const handler of Array.isArray(entry?.hooks) ? entry.hooks : []) {
			if (handler?.type === "command" && typeof handler.command === "string") {
				await runCommand(handler, payload);
			}
		}
	}
}
