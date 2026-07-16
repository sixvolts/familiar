// auth.spec.ts — the WebAuthn ceremony, end to end (TESTING-PLAN.md
// §"Phase 1" step 4). Drives the real first-run setup → register →
// auto-login → dashboard flow through the browser against an admin-
// enabled stack, using the virtual authenticator (no hardware key).
// Then logout → log back in with the same resident credential.
//
// Re-runnable: resetAuth() clears the auth tables before each test so
// the bootstrap "setup" view fires (it only shows when the
// credentials table is empty). CI provisions a clean Postgres per the
// plan, so first-run is automatic there; this keeps local re-runs sane.

import { test as base, expect, Page } from "@playwright/test";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import { start, GatewayStack } from "../fixtures/gateway";
import { enableVirtualAuthenticator } from "../fixtures/authenticator";
import { resetAuth } from "../helpers/seedDb";

// Worker-scoped admin-enabled stack — booting the gateway + workspace
// is expensive, so it's shared across the worker's specs. Auth state
// is reset per-test below.
const test = base.extend<{}, { stack: GatewayStack }>({
    stack: [
        async ({}, use) => {
            const stack = await start({ admin: true });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

// These specs mutate the shared credentials table, so run them in
// order, each from a clean auth state.
test.describe.configure({ mode: "serial" });

test.beforeEach(async () => {
    await resetAuth();
});

// On any failure, surface the gateway log tail — WebAuthn / admin
// errors land there, and a UI-level "#view-setup is hidden" is almost
// never the real story. Cheap insurance against debugging blind.
test.afterEach(async ({ stack }, testInfo) => {
    if (testInfo.status === testInfo.expectedStatus) return;
    const tail = await readLogTail(path.join(stack.instanceDir, "gateway.log"));
    await testInfo.attach("gateway.log (tail)", { body: tail, contentType: "text/plain" });
    console.error(`\n--- gateway.log (tail) for "${testInfo.title}" ---\n${tail}\n`);
});

test("register a passkey on first run, then logout and log back in", async ({ stack, page }) => {
    // The virtual authenticator must exist before the ceremony's
    // navigator.credentials.create() fires.
    await enableVirtualAuthenticator(page);

    await gotoFreshSetup(page, stack);

    // Design: the first-run page carries the brand (f-mark + "Familiar"
    // wordmark) and the system's sans heading — no leftover big-italic
    // serif "display" treatment.
    await expect(page.locator("#view-setup .lp-wordmark svg")).toBeVisible();
    await expect(page.locator("#view-setup .lp-wordmark")).toContainText("Familiar");
    await expect(page.locator("#view-setup .lp-title")).toHaveText("Register a passkey");
    await expect(page.locator("#view-setup .display")).toHaveCount(0);

    await page.locator("#setup-email").fill("e2e@example.com");
    await page.locator("#setup-display-name").fill("E2E Admin");
    await page.locator("#btn-setup-register").click();

    // doRegister → doLogin → boot: a successful ceremony auto-logs-in
    // and renders the dashboard. Generous timeout — the ceremony is a
    // few round-trips through the proxy.
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await expect(page.locator("#view-setup")).toBeHidden();

    // The session cookie is real and resolves to the bootstrap admin.
    const status = await page.evaluate(async () => {
        const r = await fetch("/console/api/auth/status", { credentials: "include" });
        return r.json();
    });
    expect(status.authenticated).toBe(true);
    expect(status.user).toBe(stack.firstUserID);
    expect(status.role).toBe("admin");

    // Logout invalidates the session; a reload drops back to login.
    await page.evaluate(() =>
        fetch("/console/api/auth/logout", { method: "POST", credentials: "include" }),
    );
    await page.reload();
    await expect(page.locator("#view-login")).toBeVisible();

    // Log back in with the same resident credential (discoverable
    // assertion — no allowlist, the authenticator surfaces the key).
    await page.locator("#btn-login").click();
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
});

test("a disabled user cannot mint a session", async ({ stack, page }) => {
    // Register the bootstrap admin first (becomes "approved").
    await enableVirtualAuthenticator(page);
    await gotoFreshSetup(page, stack);
    await page.locator("#setup-email").fill("e2e@example.com");
    await page.locator("#setup-display-name").fill("E2E Admin");
    await page.locator("#btn-setup-register").click();
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

    // Flip the admin to disabled directly in the DB, then log out.
    await disableUser(stack.firstUserID);
    await page.evaluate(() =>
        fetch("/console/api/auth/logout", { method: "POST", credentials: "include" }),
    );
    await page.reload();
    await expect(page.locator("#view-login")).toBeVisible();

    // A login attempt with a valid credential must NOT yield a session —
    // the disabled-status gate in loginFinish rejects it. The dashboard
    // must never appear; an error surfaces instead.
    await page.locator("#btn-login").click();
    await expect(page.locator("#login-error")).toBeVisible({ timeout: 15_000 });
    await expect(page.locator("#view-dashboard")).toBeHidden();
});

// gotoFreshSetup navigates to the workspace and waits for the SPA's
// boot() to settle on the first-run setup view. It diagnoses the two
// ways "setup didn't show" actually happens — a non-empty credentials
// table or an unreachable auth route — instead of failing opaquely on
// a hidden #view-setup.
async function gotoFreshSetup(page: Page, stack: GatewayStack): Promise<void> {
    await page.goto(stack.workspaceURL);

    // boot() starts on #view-loading (the only view visible by
    // default) and swaps to setup/login/dashboard once its two probes
    // (auth/status, register/status) resolve. Wait for that swap so we
    // assert on a settled UI, not a mid-boot snapshot.
    await expect(page.locator("#view-loading")).toBeHidden({ timeout: 15_000 });

    // Confirm the auth route is reachable AND the credentials table is
    // empty — the exact precondition for the setup view. A clear error
    // here beats a downstream "#view-setup is hidden".
    const resp = await page.request.get(`${stack.workspaceURL}/console/api/auth/register/status`);
    expect(
        resp.ok(),
        `register/status returned HTTP ${resp.status()} — admin auth route not reachable ` +
            `(is admin enabled + the /console/api proxy wired?)`,
    ).toBeTruthy();
    const body = await resp.json();
    expect(
        body.credentials_registered,
        `expected an empty credentials table for first-run setup, got ` +
            `credentials_registered=${body.credentials_registered}. resetAuth() may have hit a ` +
            `different database than the gateway.`,
    ).toBe(0);

    await expect(page.locator("#view-setup")).toBeVisible();
}

// disableUser flips the bootstrap admin's status to "disabled" so the
// login-status gate can be exercised. Inline pg use keeps the helper
// next to the assertion it supports.
async function disableUser(userID: string): Promise<void> {
    const { Client } = await import("pg");
    const client = new Client({ connectionString: process.env.FAMILIAR_TEST_DSN });
    await client.connect();
    try {
        await client.query(`UPDATE users SET status = 'disabled' WHERE id = $1`, [userID]);
    } finally {
        await client.end();
    }
}

async function readLogTail(file: string, bytes = 4000): Promise<string> {
    try {
        const buf = await fs.readFile(file);
        return buf.subarray(Math.max(0, buf.length - bytes)).toString("utf8");
    } catch {
        return "(no gateway.log)";
    }
}
