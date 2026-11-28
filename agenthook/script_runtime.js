import { spawn } from "node:child_process";

const defaultTimeoutSeconds = 60;
// A failure quotes at most this many characters from the end of the command's stderr.
const stderrTailLength = 2000;
// A background process the command starts can inherit a pipe and hold it open,
// so the wait ends this long after the command itself exits.
const stderrGraceMs = 250;
const stdoutLimit = 16 * 1024 * 1024;

const exitFailure = (code, signal) => {
	if (signal) return `killed by ${signal}`;
	return code === 0 ? null : `exited with status ${code}`;
};

// Runs one argv without a shell in the payload's directory, writing the payload
// to stdin, and resolves to a failure and optional collected response. Failures
// include the tail of stderr. At its timeout the child is force-killed and the
// wait ends even if it has not exited, so a child that ignores SIGTERM cannot
// block later commands or the harness.
function runCommand(handler, payload, options = {}) {
	return new Promise((resolve) => {
		let child;
		try {
			// Pi changes ctx.cwd on a cross-project resume without changing the process directory.
			child = spawn(handler.command, handler.args, { cwd: payload.cwd, stdio: ["pipe", options.collect ? "pipe" : "ignore", "pipe"] });
		} catch (error) {
			resolve({ failure: `could not start: ${error.message}` });
			return;
		}
		let stderr = "";
		let stdout = [];
		let stdoutBytes = 0;
		let overflow = false;
		let done = false;
		let grace;
		const seconds = handler.timeout > 0 ? handler.timeout : defaultTimeoutSeconds;
		const timer = setTimeout(() => {
			child.kill("SIGKILL");
			finish(`timed out after ${seconds}s`);
		}, seconds * 1000);
		const abort = () => {
			child.kill("SIGKILL");
			finish(options.signal.reason?.message ?? "cancelled");
		};
		const finish = (failure) => {
			if (done) return;
			done = true;
			clearTimeout(timer);
			clearTimeout(grace);
			options.signal?.removeEventListener("abort", abort);
			// Keep draining pipes so a process still writing to them never blocks,
			// without holding the harness open.
			child.stderr.unref();
			child.stdout?.unref();
			const detail = stderr.trim();
			if (!failure && overflow) failure = `stdout exceeds ${stdoutLimit} bytes`;
			let response;
			if (!failure && options.collect && stdoutBytes > 0) {
				const text = Buffer.concat(stdout).toString("utf8").trim();
				if (text) {
					try {
						response = JSON.parse(text);
						if (!response || typeof response !== "object" || Array.isArray(response)) throw new Error("expected an object");
						if (Object.keys(response).length === 0) response = undefined;
					} catch (error) { response = undefined; failure = `invalid stdout JSON object: ${error.message}`; }
				}
			}
			resolve({ failure: failure && detail ? `${failure}: ${detail}` : failure, response });
		};
		if (child.stdout) child.stdout.on("data", (chunk) => {
			if (done || overflow) return;
			stdoutBytes += chunk.length;
			if (stdoutBytes > stdoutLimit) { overflow = true; stdout = []; }
			else stdout.push(chunk);
		});
		options.signal?.addEventListener("abort", abort, { once: true });
		if (options.signal?.aborted) abort();
		child.stderr.setEncoding("utf8");
		child.stderr.on("data", (chunk) => {
			if (!done) stderr = (stderr + chunk).slice(-stderrTailLength);
		});
		child.once("error", (error) => finish(`could not start: ${error.message}`));
		child.once("exit", (code, signal) => {
			// The command finished in time, so only the stderr wait remains.
			clearTimeout(timer);
			grace = setTimeout(() => finish(exitFailure(code, signal)), stderrGraceMs);
		});
		child.once("close", (code, signal) => finish(exitFailure(code, signal)));
		child.stdin.on("error", () => {});
		child.stdin.end(JSON.stringify(payload));
	});
}

// Runs commands in order. By default, throws an aggregate error; collect returns
// { responses, failures }. matcher selects exact native matches, budgetMs bounds
// the batch, signal cancels it, and stop returns true for a decisive response.
async function runHooks(event, payload, options = {}) {
	const failures = [];
	const responses = [];
	const controller = new AbortController();
	const cancel = () => controller.abort(new Error("cancelled"));
	options.signal?.addEventListener("abort", cancel, { once: true });
	if (options.signal?.aborted) cancel();
	const budget = options.budgetMs == null ? undefined : setTimeout(
		() => controller.abort(new Error("callback budget exceeded")), Math.max(0, options.budgetMs),
	);
	try {
		for (const entry of config.hooks?.[event] ?? []) {
			if (options.collect && entry.matcher && entry.matcher !== options.matcher) continue;
			for (const handler of entry.hooks) {
				if (controller.signal.aborted) break;
				const { failure, response } = await runCommand(handler, payload, { collect: options.collect, signal: controller.signal });
				if (failure) failures.push(`${[handler.command, ...handler.args].join(" ")}: ${failure}`);
				if (response !== undefined) {
					responses.push(response);
					if (options.stop?.(response)) return { responses, failures };
				}
			}
			if (controller.signal.aborted) break;
		}
	} finally {
		clearTimeout(budget);
		options.signal?.removeEventListener("abort", cancel);
	}
	if (options.collect) return { responses, failures };
	if (failures.length > 0) throw new Error(`agenthook ${event} commands failed: ${failures.join("; ")}`);
}
