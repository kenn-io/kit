import { spawn } from "node:child_process";

const defaultTimeoutSeconds = 60;

// Runs one argv without a shell, writing the payload to stdin, and resolves to
// a failure description or null. At its timeout the child is force-killed and
// the wait ends even if it has not exited, so a child that ignores SIGTERM
// cannot block later commands or the harness.
function runCommand(handler, payload) {
	return new Promise((resolve) => {
		let child;
		try {
			child = spawn(handler.command, handler.args, { stdio: ["pipe", "ignore", "ignore"] });
		} catch (error) {
			resolve(`could not start: ${error.message}`);
			return;
		}
		const seconds = handler.timeout > 0 ? handler.timeout : defaultTimeoutSeconds;
		const timer = setTimeout(() => {
			child.kill("SIGKILL");
			resolve(`timed out after ${seconds}s`);
		}, seconds * 1000);
		const finish = (failure) => {
			clearTimeout(timer);
			resolve(failure);
		};
		child.once("error", (error) => finish(`could not start: ${error.message}`));
		child.once("close", (code, signal) => {
			if (signal) finish(`killed by ${signal}`);
			else finish(code === 0 ? null : `exited with status ${code}`);
		});
		child.stdin.on("error", () => {});
		child.stdin.end(JSON.stringify(payload));
	});
}

// Runs every registered command for a native event in order, then throws one
// error naming each command that failed so the harness reports it.
async function runHooks(event, payload) {
	const failures = [];
	for (const entry of config.hooks?.[event] ?? []) {
		for (const handler of entry.hooks) {
			const failure = await runCommand(handler, payload);
			if (failure) failures.push(`${[handler.command, ...handler.args].join(" ")}: ${failure}`);
		}
	}
	if (failures.length > 0) {
		throw new Error(`agenthook ${event} commands failed: ${failures.join("; ")}`);
	}
}
