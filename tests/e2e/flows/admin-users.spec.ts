// admin-users.spec.ts — the admin Users panel's onboarding affordances:
// an invited-but-not-yet-enrolled user is flagged ("no passkey") in the
// table and detail, and the admin can re-issue an enrollment link in one
// click. This is the manual-relay onboarding model — make it painless.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

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

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

test("an invited user is flagged 'no passkey' and gets a one-click enrollment link", async ({
    stack,
    browser,
    request,
}) => {
    const admin = await createTestUser({ role: "admin" });

    // Invite a teammate by email — created approved, but with no passkey.
    // Unique display name too: the canonical id derives from it, and with
    // the shared test DB + 48h token TTL a constant name would accumulate
    // active enrollment tokens past the per-user cap → 429 on re-issue.
    const suffix = Date.now().toString(36);
    const inviteResp = await request.post(`${stack.workspaceURL}/console/api/users`, {
        headers: authed(admin),
        data: { email: `invitee-${suffix}@example.com`, display_name: `Invitee ${suffix}`, role: "user" },
    });
    expect(inviteResp.ok(), `invite: HTTP ${inviteResp.status()}`).toBeTruthy();
    const invitee = (await inviteResp.json()).user;
    expect(invitee.id, "invited user id").toBeTruthy();
    expect(invitee.passkey_count, "fresh invite has no passkey").toBe(0);

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, admin);
    // Let the one-click copy succeed so we exercise the full path.
    await ctx.grantPermissions(["clipboard-read", "clipboard-write"], { origin: stack.workspaceURL });
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.evaluate(() => {
            const sp = (window as any).appSwitchPanel;
            if (sp) sp("users");
        });
        await expect(page.locator("#panel-users")).toBeVisible({ timeout: 10_000 });

        // The invitee's row carries the "no passkey" flag.
        const row = page.locator(`#users-rows tr[data-id="${invitee.id}"]`);
        await expect(row).toBeVisible({ timeout: 10_000 });
        await expect(row.locator(".status-pill.no-passkey")).toBeVisible();

        // Open the detail → PASSKEY reads "Not enrolled".
        await row.click();
        await expect(page.locator("#user-detail")).toBeVisible();
        await expect(page.locator("#user-detail-meta")).toContainText("Not enrolled");

        // Wait for the RP dropdown to finish its async populate (the sole
        // configured domain auto-selects) before the one-click action, so
        // the test doesn't race the load.
        await expect(page.locator("#user-enroll-rp")).not.toHaveValue("", { timeout: 10_000 });

        // One-click re-issue: link generated + a 48h-valid enroll URL.
        await page.locator("#user-enroll-go").click();
        await expect(page.locator("#user-enroll-url")).toHaveValue(/\/enroll\?token=/, { timeout: 10_000 });
        await expect(page.locator("#user-enroll-msg")).toContainText(/valid/i);

        // A link sent to the wrong place can be taken back: revoking ends
        // every unused one (it used to stay good for 48h).
        const token = new URL(await page.locator("#user-enroll-url").inputValue()).searchParams.get("token");
        page.once("dialog", (d) => d.accept());
        await page.locator("#user-enroll-revoke").click();
        await expect(page.locator("#user-enroll-msg")).toContainText(/Revoked \d+ unused link/, { timeout: 10_000 });
        const begin = await request.post(`${stack.workspaceURL}/console/api/auth/enroll/begin`, {
            headers: { "Content-Type": "application/json" },
            data: { token },
        });
        expect(begin.status(), "a revoked link still starts an enrollment").toBe(400);
    } finally {
        await ctx.close();
    }
});
