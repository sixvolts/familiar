// maintenance.spec.ts — maintenance mode: take the primary model out
// of rotation and route chat to an operator-selected fallback, with a
// warning banner for every user. Covers the HTTP contract (toggle,
// admin-only, /auth/status reflection, validation) and the desktop
// admin control + banner. Auto-on-primary-offline is unit-tested in
// the Go maintenance package (needs a controllable health signal).

import { test as base, expect, APIRequestContext } from "@playwright/test";
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

// A registered model id to use as the fallback. The fixture always
// registers "test/dummy"; prefer whatever /status reports first so the
// test survives config tweaks.
async function aModelID(request: APIRequestContext, stack: GatewayStack, admin: TestUser): Promise<string> {
    const status = await (
        await request.get(`${stack.workspaceURL}/console/api/status`, { headers: authed(admin) })
    ).json();
    const id = (status.models || [])[0]?.id;
    return id || "test/dummy";
}

test("admin toggles maintenance; it reflects in GET and /auth/status, and clears", async ({
    stack,
    request,
}) => {
    const admin = await createTestUser({ role: "admin" });
    const model = await aModelID(request, stack, admin);

    // Initially inactive.
    let m = await (await request.get(`${stack.workspaceURL}/console/api/maintenance`, { headers: authed(admin) })).json();
    expect(m.available).toBe(true);
    expect(m.active).toBe(false);

    // Enable with a fallback model.
    const on = await request.post(`${stack.workspaceURL}/console/api/maintenance`, {
        headers: authed(admin),
        data: { enabled: true, model_id: model },
    });
    expect(on.ok(), `enable: HTTP ${on.status()}`).toBeTruthy();
    m = await on.json();
    expect(m.active).toBe(true);
    expect(m.enabled).toBe(true);
    expect(m.reason).toBe("manual");
    expect(m.model_id).toBe(model);
    expect(m.message).toContain("Maintenance mode");

    // /auth/status carries the banner block while active.
    const status = await (await request.get(`${stack.workspaceURL}/console/api/auth/status`, { headers: authed(admin) })).json();
    expect(status.maintenance?.active).toBe(true);
    expect(status.maintenance?.message).toContain("Maintenance mode");

    // Disable → inactive, and /auth/status drops the block.
    const off = await request.post(`${stack.workspaceURL}/console/api/maintenance`, {
        headers: authed(admin),
        data: { enabled: false, model_id: model },
    });
    expect(off.ok()).toBeTruthy();
    expect((await off.json()).active).toBe(false);
    const status2 = await (await request.get(`${stack.workspaceURL}/console/api/auth/status`, { headers: authed(admin) })).json();
    expect(status2.maintenance).toBeUndefined();
});

test("enable is rejected without a model, and unknown models 400", async ({ stack, request }) => {
    const admin = await createTestUser({ role: "admin" });

    const noModel = await request.post(`${stack.workspaceURL}/console/api/maintenance`, {
        headers: authed(admin),
        data: { enabled: true, model_id: "" },
    });
    expect(noModel.status()).toBe(400);

    const ghost = await request.post(`${stack.workspaceURL}/console/api/maintenance`, {
        headers: authed(admin),
        data: { enabled: true, model_id: "ghost/model" },
    });
    expect(ghost.status()).toBe(400);
});

test("a non-admin can read state but cannot toggle", async ({ stack, request }) => {
    const user = await createTestUser(); // role: "user"
    const model = "test/dummy";

    // GET is any-authed.
    const get = await request.get(`${stack.workspaceURL}/console/api/maintenance`, { headers: authed(user) });
    expect(get.ok()).toBeTruthy();

    // POST is admin-only.
    const post = await request.post(`${stack.workspaceURL}/console/api/maintenance`, {
        headers: authed(user),
        data: { enabled: true, model_id: model },
    });
    expect(post.status()).toBe(403);
});

test("the banner shows for a user while maintenance is active", async ({ stack, browser, request }) => {
    const admin = await createTestUser({ role: "admin" });
    const model = await aModelID(request, stack, admin);

    // Turn it on out-of-band, then load the app as a regular user.
    await request.post(`${stack.workspaceURL}/console/api/maintenance`, {
        headers: authed(admin),
        data: { enabled: true, model_id: model },
    });

    const viewer = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, viewer);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        const banner = page.locator("#maintenance-banner-host .maintenance-banner");
        await expect(banner).toBeVisible({ timeout: 10_000 });
        await expect(banner).toContainText("Maintenance mode");

        // Clearing it server-side hides the banner on the next watchdog
        // probe (or a reload).
        await request.post(`${stack.workspaceURL}/console/api/maintenance`, {
            headers: authed(admin),
            data: { enabled: false, model_id: model },
        });
        await page.reload();
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await expect(page.locator("#maintenance-banner-host .maintenance-banner")).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});

test("an admin enables maintenance from the System Status card", async ({ stack, browser }) => {
    const admin = await createTestUser({ role: "admin" });
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, admin);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Open the System Status panel and find the maintenance card.
        await page.evaluate(() => (window as any).appSwitchPanel?.("system-status"));
        const sel = page.locator("#maint-model");
        await expect(sel).toBeVisible({ timeout: 10_000 });
        // The dropdown is populated from the live model catalog.
        await expect(sel.locator("option")).not.toHaveCount(0);

        // Enable through the UI → the banner appears.
        await page.locator("#maint-toggle").click();
        await expect(page.locator("#maintenance-banner-host .maintenance-banner")).toBeVisible({
            timeout: 10_000,
        });
        await expect(page.locator("#maint-toggle")).toHaveText("Disable");

        // Disable again → banner clears.
        await page.locator("#maint-toggle").click();
        await expect(page.locator("#maintenance-banner-host .maintenance-banner")).toHaveCount(0, {
            timeout: 10_000,
        });
    } finally {
        await ctx.close();
    }
});
