// session.spec.ts — session-expiry UX (2026-06-12 rework). Expiry
// used to be a silent mystery: API calls failing behind whatever was
// open until a refresh dumped the user at login. Now (a) the session
// watchdog lands the user on the login view the moment expiry is
// noticed (device wake / 90s probe), and (b) sessions slide — active
// use renews them, so the TTL is an idle window, not a deadline.

import { test as base, expect } from "@playwright/test";
import { Client } from "pg";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, seedCredential } from "../fixtures/user";

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

async function sql(query: string, params: unknown[]): Promise<any[]> {
    const client = new Client({ connectionString: process.env.FAMILIAR_TEST_DSN });
    await client.connect();
    try {
        const { rows } = await client.query(query, params);
        return rows;
    } finally {
        await client.end();
    }
}

test("an expired session lands on the login view, not a half-dead UI", async ({
    stack,
    browser,
}) => {
    const user = await createTestUser({ role: "admin" });
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Kill the session server-side — the device-asleep case.
        await sql(`UPDATE admin_sessions SET expires_at = NOW() - interval '1 minute' WHERE token = $1`, [
            user.sessionToken,
        ]);

        // Waking the device fires the watchdog probe.
        await page.evaluate(() => window.dispatchEvent(new Event("focus")));
        await expect(page.locator("#view-login")).toBeVisible({ timeout: 10_000 });
        await expect(page.locator("#view-dashboard")).toBeHidden();
    } finally {
        await ctx.close();
    }
});

test("active sessions slide: use within the window renews the expiry", async ({
    stack,
    request,
}) => {
    const user = await createTestUser();

    // Age the session into the renewal half of a 2h idle window
    // (legacy shape: no ttl_seconds — derives from the mint bounds).
    await sql(
        `UPDATE admin_sessions
            SET ttl_seconds = NULL,
                created_at = NOW() - interval '110 minutes',
                expires_at = NOW() + interval '10 minutes'
          WHERE token = $1`,
        [user.sessionToken],
    );

    // Any authenticated request renews; auth/status also re-sets the
    // cookie so the browser copy rolls with it.
    const resp = await request.get(`${stack.workspaceURL}/console/api/auth/status`, {
        headers: { Cookie: user.cookieHeader },
    });
    expect(resp.status()).toBe(200);
    const setCookie = resp.headers()["set-cookie"] || "";
    expect(setCookie).toContain("familiar_admin_session=");

    const rows = await sql(
        `SELECT EXTRACT(EPOCH FROM (expires_at - NOW()))::int AS remaining, ttl_seconds
           FROM admin_sessions WHERE token = $1`,
        [user.sessionToken],
    );
    expect(rows.length).toBe(1);
    // Renewed back out to ~the full 2h window, and the legacy row's
    // window was backfilled.
    expect(rows[0].remaining).toBeGreaterThan(100 * 60);
    expect(rows[0].ttl_seconds).toBeGreaterThan(6000);
});

// Someone else signing in, in a tab that showed a dashboard, gets a
// fresh page. Nothing reset the previous principal's open chat, notes,
// Home pins or panel permissions, so on a shared device or kiosk the
// next person saw and used them.
test("signing in as someone else in the same tab starts from a clean page", async ({ stack, browser }) => {
    const alice = await createTestUser();
    const bob = await createTestUser();
    await seedCredential(alice.id); // the expired session lands on login, not first-run setup
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, alice);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.evaluate(() => { (window as any).__aliceDocument = true; });

        // Alice's session ends; the watchdog lands the tab on login.
        await sql(`UPDATE admin_sessions SET expires_at = NOW() - interval '1 minute' WHERE token = $1`, [alice.sessionToken]);
        await page.evaluate(() => window.dispatchEvent(new Event("focus")));
        await expect(page.locator("#view-login")).toBeVisible({ timeout: 10_000 });

        // Bob signs in here. The passkey ceremony is stubbed; his session
        // cookie stands in for the one login/finish would set.
        await attachSession(ctx, stack.workspaceURL, bob);
        await page.route("**/console/api/auth/login/begin", (r) => r.fulfill({ json: { publicKey: { challenge: "AAAA" } } }));
        await page.route("**/console/api/auth/login/finish", (r) => r.fulfill({ json: { status: "authenticated" } }));
        await page.evaluate(() => {
            const buf = () => new Uint8Array([1]).buffer;
            (navigator.credentials as any).get = async () => ({
                id: "x", rawId: buf(), type: "public-key",
                response: { authenticatorData: buf(), clientDataJSON: buf(), signature: buf(), userHandle: null },
                getClientExtensionResults: () => ({}),
            });
        });
        await page.locator("#btn-login").click();

        await expect.poll(() => page.evaluate(() => (window as any).FAMILIAR_SESSION?.user), { timeout: 15_000 }).toBe(bob.id);
        await expect(page.locator("#view-dashboard")).toBeVisible();
        expect(await page.evaluate(() => (window as any).__aliceDocument), "Bob got Alice's page, not a fresh one").toBeUndefined();
    } finally {
        await ctx.close();
    }
});

// Tab titles (note and conversation names) and document ids are kept
// per principal and dropped on sign-out. They sat under one browser-wide
// key: after Alice signed out, Bob's tab bar showed her tab titles.
test("workspace tabs don't outlive sign-out or reach the next person", async ({ stack, browser, request }) => {
    const alice = await createTestUser();
    const bob = await createTestUser();
    await seedCredential(alice.id); // signing out lands on login, not first-run setup
    const secret = `Layoffs plan ${Date.now().toString(36)}`;
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers: { Cookie: alice.cookieHeader, "Content-Type": "application/json" },
            data: { title: secret, content: "private" },
        })
    ).json();
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    const stored = () => page.evaluate(() => Object.keys(localStorage).map((k) => k + "=" + localStorage.getItem(k)).join("\n"));
    try {
        await attachSession(ctx, stack.workspaceURL, alice);
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes").click();
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id } }));
        }, note.id);
        await expect(page.locator(".ws-tab-label", { hasText: secret })).toBeVisible({ timeout: 10_000 });
        await expect.poll(stored).toContain(`familiar.workspace.v1:u:${alice.id}=`);

        await page.evaluate(() => (window as any).appSwitchPanel("user"));
        await page.locator("#user-signout").click();
        await expect(page.locator("#view-login")).toBeVisible({ timeout: 15_000 });
        expect(await stored(), "Alice's tab titles are still in the browser").not.toContain(secret);

        await attachSession(ctx, stack.workspaceURL, bob);
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await expect(page.locator(".ws-tab-label", { hasText: secret })).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});
