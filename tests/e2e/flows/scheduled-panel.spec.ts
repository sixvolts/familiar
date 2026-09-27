// scheduled-panel.spec.ts — the Scheduled panel keeps what it can't
// show and doesn't carry one action's state into another: a shard
// envelope it can't list, another action's webhook URL, a 0-second
// interval. Rows and run rows open from the keyboard. No model needed.

import { test as base, expect, Page, BrowserContext, APIRequestContext } from "@playwright/test";
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

async function createAction(request: APIRequestContext, stack: GatewayStack, user: TestUser, extra: Record<string, unknown>) {
    const resp = await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers: authed(user),
        data: { name: `act ${Date.now().toString(36)}`, prompt: "summarize", trigger_kind: "cron", cron: "0 7 * * *", report_targets: [{ kind: "log" }], ...extra },
    });
    expect(resp.ok(), `create action: ${resp.status()} ${await resp.text()}`).toBeTruthy();
    return resp.json();
}

async function getAction(request: APIRequestContext, stack: GatewayStack, user: TestUser, id: string) {
    return (await request.get(`${stack.workspaceURL}/console/api/actions/${id}`, { headers: authed(user) })).json();
}

async function openPanel(browser: any, stack: GatewayStack, user: TestUser): Promise<{ ctx: BrowserContext; page: Page }> {
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    return { ctx, page };
}

async function showPanel(page: Page, stack: GatewayStack) {
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await page.locator(".sidebar-cat-scheduled").click();
    await expect(page.locator("#panel-scheduled")).toBeVisible({ timeout: 10_000 });
    await expect(page.locator("#actions-rows")).not.toContainText("Loading", { timeout: 10_000 });
}

async function openFromKeyboard(page: Page, name: string) {
    const open = page.getByRole("button", { name: `Open action: ${name}` });
    await open.focus();
    await page.keyboard.press("Enter");
    await expect(page.locator("#action-name")).toHaveValue(name, { timeout: 10_000 });
}

// With the shard list unavailable, the stored shard stays selected and
// an unrelated edit doesn't send the envelope. It fell back to "Run as
// you", and the save re-enveloped the action with the full toolbox.
test("an edit keeps a shard envelope the panel can't list", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const shardID = `e2e-sched-${Date.now().toString(36)}`;
    const sh = await request.post(`${stack.workspaceURL}/console/api/shards`, {
        headers: authed(user),
        data: { id: shardID, name: "Poster", persistence: "persistent", visibility: "isolated", scope_tag: `shard:${shardID}`, system_prompt: "x", tool_allowlist: [] },
    });
    expect(sh.ok()).toBeTruthy();
    const act = await createAction(request, stack, user, { envelope: "shard", shard_id: shardID });

    const { ctx, page } = await openPanel(browser, stack, user);
    try {
        await page.route("**/console/api/shards", (route) => route.fulfill({ status: 503, body: "{}" }));
        await showPanel(page, stack);
        await openFromKeyboard(page, act.name);
        await expect(page.locator("#action-shard")).toHaveValue(`shard:${shardID}`);
        await expect(page.locator("#action-shard option:checked")).toContainText("unavailable");

        await page.locator("#action-prompt").fill("summarize, tersely");
        const patch = page.waitForRequest((r) => r.method() === "PATCH" && r.url().includes(`/console/api/actions/${act.id}`));
        await page.locator("#action-form button[type=submit]").click();
        const body = (await patch).postDataJSON();
        expect(Object.keys(body)).not.toContain("envelope");
        await expect.poll(async () => (await getAction(request, stack, user, act.id)).prompt).toBe("summarize, tersely");
        const got = await getAction(request, stack, user, act.id);
        expect(got.envelope + ":" + got.shard_id).toBe(`shard:${shardID}`);
    } finally {
        await ctx.close();
    }
});

// Opening one action after another shows the second one's state only:
// a webhook action's secret URL showed under the next action.
test("one action's webhook URL doesn't show under another", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const hook = await createAction(request, stack, user, { trigger_kind: "webhook", cron: "" });
    const cron = await createAction(request, stack, user, {});
    expect(hook.webhook_token).toBeTruthy();

    const { ctx, page } = await openPanel(browser, stack, user);
    try {
        await showPanel(page, stack);
        await openFromKeyboard(page, hook.name);
        await expect(page.locator("#action-webhook-hint")).toContainText(hook.webhook_token);
        await page.locator("#action-detail-back").click();
        await openFromKeyboard(page, cron.name);
        await page.locator("#action-trigger").selectOption("webhook");
        await expect(page.locator("#action-webhook-hint")).toBeVisible();
        await expect(page.locator("#action-webhook-hint")).not.toContainText(hook.webhook_token);
    } finally {
        await ctx.close();
    }
});

// "Min seconds between fires" 0 means every event fires; it was stored
// (and shown) as 60.
test("a 0-second interval round-trips", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const hook = await createAction(request, stack, user, { trigger_kind: "webhook", cron: "", min_interval_seconds: 0 });
    expect(hook.min_interval_seconds).toBe(0);
    const { ctx, page } = await openPanel(browser, stack, user);
    try {
        await showPanel(page, stack);
        await openFromKeyboard(page, hook.name);
        await expect(page.locator("#action-interval")).toHaveValue("0");
        await page.locator("#action-prompt").fill("summarize again");
        await page.locator("#action-form button[type=submit]").click();
        await expect.poll(async () => (await getAction(request, stack, user, hook.id)).prompt).toBe("summarize again");
        expect((await getAction(request, stack, user, hook.id)).min_interval_seconds).toBe(0);
    } finally {
        await ctx.close();
    }
});

// A run's detail (full output, per-target delivery results — the only
// place a failed delivery shows) opens from the keyboard.
test("a run's detail opens from the keyboard", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const act = await createAction(request, stack, user, {});
    const run = await request.post(`${stack.workspaceURL}/console/api/actions/${act.id}/run`, { headers: authed(user) });
    expect(run.ok(), await run.text()).toBeTruthy();

    const { ctx, page } = await openPanel(browser, stack, user);
    try {
        await showPanel(page, stack);
        await openFromKeyboard(page, act.name);
        const row = page.locator("#action-runs-rows tr[data-expandable]").first();
        await expect(row).toBeVisible({ timeout: 10_000 });
        await expect(row).toHaveAttribute("aria-expanded", "false");
        await row.focus();
        await page.keyboard.press("Enter");
        await expect(page.locator("#action-runs-rows .action-run-detail")).toBeVisible();
        await expect(row).toHaveAttribute("aria-expanded", "true");
    } finally {
        await ctx.close();
    }
});
