// memory-panel.spec.ts — the memory console panel: opening memories and
// entities from the keyboard, and the list showing the tab picked last
// and a real page after deletes. Memories are seeded straight into the
// test database (no model involved).

import { test as base, expect, Page, BrowserContext } from "@playwright/test";
import { Client } from "pg";
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

async function seed(user: TestUser, rows: { content: string; source_type?: string }[], rels: [string, string, string][] = []) {
    const client = new Client({ connectionString: process.env.FAMILIAR_TEST_DSN });
    await client.connect();
    try {
        for (const r of rows) {
            await client.query(
                `INSERT INTO memories (agent_id, scope, content, source_type, user_id)
                 VALUES ('familiar', $1, $2, $3, $4)`,
                [r.source_type === "conversation" ? "session" : "user", r.content, r.source_type || "explicit", user.id],
            );
        }
        for (const [s, p, o] of rels) {
            await client.query(
                `INSERT INTO relationships (subject, predicate, object, user_id) VALUES ($1, $2, $3, $4)`,
                [s, p, o, user.id],
            );
        }
    } finally {
        await client.end();
    }
}

async function openPanel(browser: any, stack: GatewayStack, user: TestUser): Promise<{ ctx: BrowserContext; page: Page }> {
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await page.evaluate(() => (window as any).appSwitchPanel("memory"));
    await expect(page.locator("#memory-section")).toBeVisible({ timeout: 10_000 });
    return { ctx, page };
}

// Memories and entities open from the keyboard. Their rows took mouse
// clicks only, and they are the only way to edit, delete, merge or
// collapse anything.
test("memories and entities open from the keyboard", async ({ stack, browser }) => {
    const user = await createTestUser();
    await seed(user, [{ content: "Drew keeps the spare key under the blue pot" }], [["acme", "located_in", "boston"]]);
    const { ctx, page } = await openPanel(browser, stack, user);
    try {
        const open = page.getByRole("button", { name: /Open memory: Drew keeps the spare key/ });
        await open.focus();
        await page.keyboard.press("Enter");
        await expect(page.locator("#memory-detail")).toBeVisible();
        await expect(page.locator("#memory-detail")).toContainText("blue pot");
        await page.keyboard.press("Escape");

        await page.locator('.memory-kind-tab[data-kind="entities"]').click();
        const ent = page.getByRole("button", { name: "Open entity: acme" });
        await ent.focus();
        await page.keyboard.press("Enter");
        await expect(page.locator("#entity-detail")).toBeVisible();
    } finally {
        await ctx.close();
    }
});

// A load overtaken by a later one doesn't paint: switching from the slow
// Conversation log tab to Knowledge showed the transcripts under
// Knowledge when they arrived last.
test("the list shows the tab picked last, not the slowest response", async ({ stack, browser }) => {
    const user = await createTestUser();
    await seed(user, [
        { content: "knowledge: Drew prefers tea" },
        { content: "user: hi assistant: transcript chunk", source_type: "conversation" },
    ]);
    const { ctx, page } = await openPanel(browser, stack, user);
    try {
        await expect(page.locator("#memory-rows")).toContainText("Drew prefers tea", { timeout: 10_000 });
        await page.route("**/console/api/memories?*", async (route) => {
            if (route.request().url().includes("kind=chunks")) await new Promise((r) => setTimeout(r, 800));
            await route.continue();
        });
        await page.locator('.memory-kind-tab[data-kind="chunks"]').click();
        await page.locator('.memory-kind-tab[data-kind="knowledge"]').click();
        await page.waitForTimeout(1500);
        await expect(page.locator("#memory-rows")).toContainText("Drew prefers tea");
        await expect(page.locator("#memory-rows")).not.toContainText("transcript chunk");
    } finally {
        await ctx.close();
    }
});

// Deleting the only row on the last page shows the page before it, not
// "51–50 OF 50" beside NO RESULTS.
test("deleting the last page's only row goes back a page", async ({ stack, browser }) => {
    const user = await createTestUser();
    const rows = Array.from({ length: 51 }, (_, i) => ({ content: `fact number ${String(i).padStart(2, "0")}` }));
    await seed(user, rows);
    const { ctx, page } = await openPanel(browser, stack, user);
    try {
        await expect(page.locator("#m-page-info")).toHaveText("1–50 OF 51", { timeout: 10_000 });
        await page.locator("#m-next").click();
        await expect(page.locator("#m-page-info")).toHaveText("51–51 OF 51");
        page.on("dialog", (d) => d.accept());
        await page.locator("#memory-rows .row-open").first().click();
        await page.locator("#detail-delete").click();
        await expect(page.locator("#m-page-info")).toHaveText("1–50 OF 50", { timeout: 10_000 });
        await expect(page.locator("#memory-rows .row-open")).toHaveCount(50);
    } finally {
        await ctx.close();
    }
});
