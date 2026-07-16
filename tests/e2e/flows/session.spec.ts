// session.spec.ts — session-expiry UX (2026-06-12 rework). Expiry
// used to be a silent mystery: API calls failing behind whatever was
// open until a refresh dumped the user at login. Now (a) the session
// watchdog lands the user on the login view the moment expiry is
// noticed (device wake / 90s probe), and (b) sessions slide — active
// use renews them, so the TTL is an idle window, not a deadline.

import { test as base, expect } from "@playwright/test";
import { Client } from "pg";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession } from "../fixtures/user";

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
