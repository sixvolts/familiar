// mobile.spec.ts — the mobile SPA (mobile.html / mobile.js), which had
// ZERO E2E coverage despite being what most beta testers will use. Runs
// ONLY under the "mobile" Playwright project (Pixel 7 descriptor: phone
// viewport + touch + mobile UA), so the workspace's UA sniffing serves
// mobile.html at "/". Focus: the boot/auth gate and — the P0 fix — that
// a session lapsing mid-use lands the user on login instead of a wall
// of failing requests (mobile had no 401 handling and no watchdog).

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, setUserStatus } from "../fixtures/user";
import { enableVirtualAuthenticator } from "../fixtures/authenticator";

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

test("the phone UA gets the mobile SPA and an unauthenticated boot lands on an auth screen", async ({
    stack,
    page,
}) => {
    await page.goto(stack.workspaceURL);
    // mobile.html-specific chrome proves the UA routing served the
    // mobile SPA, not the desktop index.
    await expect(page.locator("#mob-auth-loading")).toHaveCount(1);
    // Boot settles off the spinner…
    await expect(page.locator("#mob-auth-loading")).toBeHidden({ timeout: 15_000 });
    // …onto an auth overlay (login or first-run setup), never the app.
    await expect(page.locator("#mob-app")).toBeHidden();
    const authVisible = page.locator("#mob-auth-login:visible, #mob-auth-setup:visible");
    await expect(authVisible.first()).toBeVisible();
});

test("an authenticated boot shows the app shell", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);

    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    await expect(page.locator("#mob-auth-login")).toBeHidden();
    // The bottom tab bar is the mobile shell's signature chrome.
    await expect(page.locator("#mob-tabbar")).toBeVisible();
});

test("a session that lapses mid-use routes back to login (watchdog)", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });

    // The session goes away (expired / revoked) — every auth/status
    // probe now 401s. Before the fix, mobile had no watchdog and would
    // sit on a dead app dumping "HTTP 401" strings.
    await page.route("**/console/api/auth/status", (route) =>
        route.fulfill({ status: 401, contentType: "application/json", body: '{"error":"unauthorized"}' }),
    );
    // Returning to the app fires the watchdog probe.
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));

    await expect(page.locator("#mob-auth-login")).toBeVisible({ timeout: 15_000 });
});

test("MOBILE: create user → enroll a passkey → log in", async ({ stack, page, request }) => {
    // Full onboarding chain on the phone: a new approved user with no
    // passkey self-issues an enrollment token, registers via the
    // enrollment link, then logs into the mobile SPA with that passkey.
    const user = await createTestUser();
    const tokenResp = await request.post(`${stack.workspaceURL}/console/api/auth/enrollment-token`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { target_rp_id: "localhost" },
    });
    expect(tokenResp.ok(), `enrollment-token: HTTP ${tokenResp.status()}`).toBeTruthy();
    const { token } = await tokenResp.json();
    expect(token, "token issued").toBeTruthy();

    await enableVirtualAuthenticator(page);

    // 1. Register the passkey via the enrollment link (enroll.html is
    //    served at /enroll regardless of UA).
    await page.goto(`${stack.workspaceURL}/enroll?token=${encodeURIComponent(token)}`);
    await expect(page.locator("#register-btn")).toBeVisible({ timeout: 10_000 });
    await page.locator("#register-btn").click();
    await expect(page.locator("#status.is-ok")).toContainText(/passkey registered/i, { timeout: 15_000 });

    // 2. Log into the mobile SPA with that passkey (the phone UA gets
    //    mobile.html at "/"; the authenticator + resident credential
    //    persist across the navigation).
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-auth-login")).toBeVisible({ timeout: 15_000 });
    await page.locator("#mob-login-btn").click();
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    await expect(page.locator("#mob-auth-login")).toBeHidden();

    // 3. The right user is signed in.
    const status = await (await page.request.get(`${stack.workspaceURL}/console/api/auth/status`)).json();
    expect(status.authenticated).toBe(true);
    expect(status.user).toBe(user.id);
});

test("MOBILE: the Account screen shows a gated Notifications toggle", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });

    await page.evaluate(() => { location.hash = "account"; });
    await expect(page.locator('.mob-screen[data-screen="account"]')).toHaveClass(/is-active/, { timeout: 10_000 });

    // The Notifications section renders and its state resolves (not stuck
    // on "Checking…"). Headless isn't an installed standalone PWA and the
    // stack has no VAPID configured, so the toggle is gated off — what
    // matters is Push.render ran without error and surfaced a reason.
    await expect(page.locator(".mob-screen.is-active", { hasText: "Notifications" })).toBeVisible();
    const meta = page.locator("#mob-push-meta");
    await expect(meta).not.toHaveText("Checking…", { timeout: 10_000 });
    await expect(meta).not.toHaveText("");
    await expect(page.locator("#mob-push-toggle")).toBeHidden();
});

test("MOBILE: a pending account gets friendly copy, not a raw error", async ({ stack, page, request }) => {
    // Parity with desktop: a pending mobile user must see a sentence, not
    // the raw "account pending approval" string. Enroll a passkey, flip
    // the account to pending, then attempt login.
    const user = await createTestUser();
    const tokenResp = await request.post(`${stack.workspaceURL}/console/api/auth/enrollment-token`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { target_rp_id: "localhost" },
    });
    expect(tokenResp.ok(), `enrollment-token: HTTP ${tokenResp.status()}`).toBeTruthy();
    const { token } = await tokenResp.json();

    await enableVirtualAuthenticator(page);
    await page.goto(`${stack.workspaceURL}/enroll?token=${encodeURIComponent(token)}`);
    await page.locator("#register-btn").click();
    await expect(page.locator("#status.is-ok")).toContainText(/passkey registered/i, { timeout: 15_000 });

    // Gate the account, then try to sign in on the mobile SPA.
    await setUserStatus(user.id, "pending");
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-auth-login")).toBeVisible({ timeout: 15_000 });
    await page.locator("#mob-login-btn").click();

    const err = page.locator("#mob-login-error");
    await expect(err).toBeVisible({ timeout: 15_000 });
    await expect(err).toContainText(/awaiting admin approval/i);
    await expect(err).not.toContainText(/account pending approval/i); // the raw string
    await expect(page.locator("#mob-app")).toBeHidden();
});
