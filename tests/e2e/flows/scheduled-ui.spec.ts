// scheduled-ui.spec.ts — the Actions panel UI without a model: the
// "Run as" envelope select (built-ins + the user's shards), the
// envelope round-trip through the form, and the slack_dm target
// option. Run mechanics live in scheduled.spec.ts (model-gated) and
// the Go runner tests.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

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

test("the Push notification target shows a hint and auto-creates a thread", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-scheduled").click();
        await expect(page.locator("#panel-scheduled")).toBeVisible({ timeout: 10_000 });
        await expect(page.locator("#actions-rows")).not.toContainText("Loading", { timeout: 10_000 });
        await page.locator("#actions-new").click();
        await expect(page.locator("#action-detail")).toBeVisible({ timeout: 10_000 });

        // "Push notification (PWA)" is an offered delivery target, and
        // choosing it reveals the explanatory hint.
        await expect(page.locator('#action-target-kind option[value="push"]')).toHaveCount(1);
        await page.locator("#action-target-kind").selectOption("push");
        await expect(page.locator("#action-target-push")).toBeVisible();

        // Create it — the push target auto-creates a "Scheduled: <name>"
        // thread (one per action) the notification will deep-link to.
        const name = `push ui ${Date.now().toString(36)}`;
        await page.locator("#action-name").fill(name);
        await page.locator("#action-prompt").fill("notify me");
        await page.locator("#action-cron").fill("0 7 * * *");
        await page.locator("#action-form button[type=submit]").click();
        await expect(page.locator("#actions-rows tr", { hasText: name })).toBeVisible({ timeout: 10_000 });

        const list = await (
            await request.get(`${stack.workspaceURL}/console/api/actions`, { headers: authed(user) })
        ).json();
        const act = (list.items ?? []).find((a: any) => a.name === name);
        expect(act, "created action").toBeTruthy();
        const t = (act.report_targets || [])[0];
        expect(t.kind).toBe("push");
        expect(t.conversation_id, "push target auto-creates a thread").toMatch(/[0-9a-f-]{36}/);
    } finally {
        await ctx.close();
    }
});

test("New action opens as its own page with a clock-titled header", async ({ stack, browser }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-scheduled").click();
        await expect(page.locator("#panel-scheduled")).toBeVisible({ timeout: 10_000 });

        // Panel title: capital-A "Scheduled Actions" with the clock glyph,
        // and no "Automation" eyebrow.
        const title = page.locator("#scheduled-list .section-title-scheduled");
        await expect(title).toContainText("Scheduled Actions");
        await expect(title.locator("svg")).toBeVisible();
        await expect(page.locator("#scheduled-list")).not.toContainText("Automation");

        // Wait for the panel init to finish wiring before clicking New.
        await expect(page.locator("#actions-rows")).not.toContainText("Loading", { timeout: 10_000 });

        // New action takes over the panel — the list view is hidden and
        // the detail shows with a back link on top + a clock-titled header.
        await page.locator("#actions-new").click();
        await expect(page.locator("#action-detail")).toBeVisible({ timeout: 10_000 });
        await expect(page.locator("#scheduled-list")).toBeHidden();
        await expect(page.locator("#action-detail-back")).toBeVisible();
        await expect(page.locator(".action-detail-header .section-title-scheduled svg")).toBeVisible();
        await expect(page.locator("#action-detail-title")).toHaveText("New Action");

        // The form is grouped into the same section cards as the shard form.
        await expect(page.locator("#action-form .shard-section-title")).toHaveText([
            "Definition",
            "Trigger",
            "Report",
            "Envelope & limits",
        ]);

        // Back returns to the list.
        await page.locator("#action-detail-back").click();
        await expect(page.locator("#scheduled-list")).toBeVisible();
        await expect(page.locator("#action-detail")).toBeHidden();
    } finally {
        await ctx.close();
    }
});

test("the Run-as select offers built-ins + shards, and ephemeral round-trips", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const shardID = `e2e-envui-${Date.now().toString(36)}`;
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/shards`, {
                headers,
                data: {
                    id: shardID, name: "Env Shard", persistence: "persistent",
                    visibility: "isolated", scope_tag: `shard:${shardID}`,
                    system_prompt: "x", tool_allowlist: [],
                },
            })
        ).ok(),
    ).toBeTruthy();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-scheduled").click();
        await expect(page.locator("#panel-scheduled")).toBeVisible({ timeout: 10_000 });
        await page.locator("#actions-new").click();

        // The envelope select: two built-ins, then the shard group.
        const runAs = page.locator("#action-shard");
        await expect(runAs.locator('option[value="user"]')).toHaveText(/Run as you/);
        await expect(runAs.locator('option[value="ephemeral"]')).toHaveText(/Ephemeral/);
        await expect(runAs.locator(`optgroup[label="Your shards"] option[value="shard:${shardID}"]`))
            .toHaveCount(1);

        // The delivery select advertises Slack DM.
        await expect(page.locator('#action-target-kind option[value="slack_dm"]')).toHaveCount(1);

        // Create an ephemeral action through the form.
        await page.locator("#action-name").fill("env ui action");
        await page.locator("#action-prompt").fill("say hi");
        await page.locator("#action-cron").fill("0 7 * * *");
        await page.locator("#action-target-kind").selectOption("log");
        await runAs.selectOption("ephemeral");
        await page.locator("#action-form button[type=submit]").click();

        // List shows the envelope; reopening shows it selected.
        const row = page.locator("#actions-rows tr", { hasText: "env ui action" });
        await expect(row).toBeVisible({ timeout: 10_000 });
        await expect(row).toContainText("ephemeral");
        await row.click();
        await expect(runAs).toHaveValue("ephemeral", { timeout: 10_000 });

        // And the API agrees.
        const list = await (
            await request.get(`${stack.workspaceURL}/console/api/actions`, { headers })
        ).json();
        const act = (list.items ?? []).find((a: any) => a.name === "env ui action");
        expect(act.envelope).toBe("ephemeral");
        expect(act.shard_id ?? "").toBe("");

        // Switch it to the shard envelope and back through the form.
        await runAs.selectOption(`shard:${shardID}`);
        await page.locator("#action-form button[type=submit]").click();
        await expect
            .poll(async () => {
                const l = await (
                    await request.get(`${stack.workspaceURL}/console/api/actions`, { headers })
                ).json();
                const a = (l.items ?? []).find((x: any) => x.name === "env ui action");
                return a && a.envelope + ":" + (a.shard_id || "");
            }, { timeout: 10_000 })
            .toBe(`shard:${shardID}`);
    } finally {
        await ctx.close();
    }
});
