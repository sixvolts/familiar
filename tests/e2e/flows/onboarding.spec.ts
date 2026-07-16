// onboarding.spec.ts — the paths a NEW beta user actually walks to get
// in: an account awaiting approval, the "register a key" affordance on
// the login page, and the admin-issued enrollment-link flow (enroll.html
// + token-gated /enroll endpoints). These had no E2E coverage and are
// exactly where a first-time user gets confused or stuck, so they're
// worth pinning before opening the doors.

import { test as base, expect, Page } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { enableVirtualAuthenticator } from "../fixtures/authenticator";
import { createTestUser, setUserStatus, TestUser } from "../fixtures/user";
import { resetAuth } from "../helpers/seedDb";

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

// Serial + clean auth state per test — same contract as auth.spec, since
// these also mutate the shared credentials/users tables.
test.describe.configure({ mode: "serial" });
test.beforeEach(async () => {
    await resetAuth();
});

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

// registerFirstAdmin runs the first-run setup ceremony so a credential
// exists (the precondition for the login + register-key branches).
async function registerFirstAdmin(page: Page, stack: GatewayStack) {
    await enableVirtualAuthenticator(page);
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-loading")).toBeHidden({ timeout: 15_000 });
    await expect(page.locator("#view-setup")).toBeVisible();
    await page.locator("#setup-email").fill("admin@example.com");
    await page.locator("#setup-display-name").fill("E2E Admin");
    await page.locator("#btn-setup-register").click();
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
}

test("a pending account gets an 'awaiting approval' message, not a raw error", async ({ stack, page }) => {
    await registerFirstAdmin(page, stack);

    // Flip the bootstrap user to pending and log out — now their valid
    // passkey assertion succeeds but the gateway refuses to mint a
    // session (status != approved).
    await setUserStatus(stack.firstUserID, "pending");
    await page.evaluate(() => fetch("/console/api/auth/logout", { method: "POST", credentials: "include" }));
    await page.reload();
    await expect(page.locator("#view-login")).toBeVisible();

    await page.locator("#btn-login").click();
    // Friendly, status-specific copy — not "ERROR: account pending approval".
    await expect(page.locator("#login-error")).toContainText(/awaiting admin approval/i, { timeout: 15_000 });
    await expect(page.locator("#login-error")).not.toContainText(/^ERROR:/);
    await expect(page.locator("#view-dashboard")).toBeHidden();
});

test("a disabled account gets the disabled message", async ({ stack, page }) => {
    await registerFirstAdmin(page, stack);
    await setUserStatus(stack.firstUserID, "disabled");
    await page.evaluate(() => fetch("/console/api/auth/logout", { method: "POST", credentials: "include" }));
    await page.reload();
    await expect(page.locator("#view-login")).toBeVisible();

    await page.locator("#btn-login").click();
    await expect(page.locator("#login-error")).toContainText(/disabled/i, { timeout: 15_000 });
    await expect(page.locator("#view-dashboard")).toBeHidden();
});

// issueEnrollToken creates a brand-new approved user (no passkey yet)
// and self-issues an enrollment token for them — the admin/Slack-
// created-account starting point for the enrollment flow.
async function newUserWithEnrollToken(stack: GatewayStack, request: any) {
    const user = await createTestUser();
    const tokenResp = await request.post(`${stack.workspaceURL}/console/api/auth/enrollment-token`, {
        headers: authed(user),
        data: { target_rp_id: "localhost" },
    });
    expect(tokenResp.ok(), `enrollment-token: HTTP ${tokenResp.status()}`).toBeTruthy();
    const { token } = await tokenResp.json();
    expect(token, "token issued").toBeTruthy();
    return { user, token };
}

test("an expired or invalid enrollment link explains itself", async ({ stack, browser }) => {
    // The gateway collapses expired/used/wrong-domain into one generic
    // error; the enroll page should tell the user what to DO, not show a
    // raw status code. A bogus token fails at begin (before any passkey
    // ceremony), so no authenticator is needed.
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    try {
        await page.goto(`${stack.workspaceURL}/enroll?token=bogus-${Date.now().toString(36)}`);
        await expect(page.locator("#register-btn")).toBeVisible({ timeout: 10_000 });
        await page.locator("#register-btn").click();
        const status = page.locator("#status.is-err");
        await expect(status).toBeVisible({ timeout: 10_000 });
        await expect(status).toContainText(/invalid or has expired/i);
        await expect(status).toContainText(/ask your admin/i);
    } finally {
        await ctx.close();
    }
});

test("DESKTOP: create user → enroll a passkey → log in", async ({ stack, browser, request }) => {
    const { user, token } = await newUserWithEnrollToken(stack, request);

    // A fresh desktop context (no seeded session cookie — we want the
    // REAL passkey login, not a pre-authenticated boot).
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    try {
        await enableVirtualAuthenticator(page);

        // 1. Register the passkey via the enrollment link.
        await page.goto(`${stack.workspaceURL}/enroll?token=${encodeURIComponent(token)}`);
        await expect(page.locator("#register-btn")).toBeVisible({ timeout: 10_000 });
        await page.locator("#register-btn").click();
        await expect(page.locator("#status.is-ok")).toContainText(/passkey registered/i, { timeout: 15_000 });

        // The token is one-shot: a second begin with the same token fails.
        const reuse = await request.post(`${stack.workspaceURL}/console/api/auth/enroll/begin`, {
            data: { token },
        });
        expect(reuse.ok(), "consumed enrollment token must not be reusable").toBeFalsy();

        // 2. Now log in with that passkey (same page → the virtual
        //    authenticator + its resident credential persist across the
        //    navigation; login is a discoverable assertion, no email).
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-login")).toBeVisible({ timeout: 15_000 });
        await page.locator("#btn-login").click();
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // 3. And it's the right user.
        const status = await (await page.request.get(`${stack.workspaceURL}/console/api/auth/status`)).json();
        expect(status.authenticated).toBe(true);
        expect(status.user).toBe(user.id);
    } finally {
        await ctx.close();
    }
});
