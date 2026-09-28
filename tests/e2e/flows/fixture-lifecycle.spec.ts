// fixture-lifecycle.spec.ts — the stack fixture's process handling
// (fixtures/gateway.ts), on a plain child process: no stack, no browser.
//
// A gateway the OS kills with a signal (jetsam under memory pressure)
// has exitCode null. Checking exitCode alone made stop() wait forever on
// it, and made start() miss a crash during boot until the 30s deadline.

import { test, expect } from "@playwright/test";
import { spawn } from "node:child_process";
import { stopChild, waitForUrl } from "../fixtures/gateway";

test.describe.configure({ timeout: 15_000 });

test("stop returns for a child a signal already killed", async () => {
    const child = spawn("sleep", ["30"]);
    const gone = new Promise((resolve) => child.once("exit", resolve));
    child.kill("SIGKILL");
    await gone;
    const t0 = Date.now();
    await stopChild(child);
    expect(Date.now() - t0).toBeLessThan(1_000);
});

test("stop ends a running child", async () => {
    const child = spawn("sleep", ["30"]);
    await stopChild(child);
    expect(child.signalCode).toBe("SIGTERM");
});

test("boot gives up as soon as the child dies by a signal", async () => {
    const child = spawn("sleep", ["30"]);
    setTimeout(() => child.kill("SIGKILL"), 200);
    const t0 = Date.now();
    // Nothing listens on port 1, so only the child's death can end this early.
    await expect(waitForUrl("http://127.0.0.1:1/", 10_000, child, "sleeper")).rejects.toThrow(/exited \(SIGKILL\)/);
    expect(Date.now() - t0).toBeLessThan(3_000);
});
