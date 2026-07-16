// sidebar-panels.spec.ts — the Shards and Actions rail categories
// list their content as children (2026-06-13 fix: Actions had no
// child-fetch branch at all, and Shards filtered to chat-enabled
// only). Clicking a child reaches the right place.

import { test as base, expect } from "@playwright/test";
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

test("Shards and Actions rail categories list their content", async ({ stack, browser, request }) => {
    // Fresh non-admin user so the lists are scoped to exactly what
    // this test creates (admins see every shard/action on the box).
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const tag = Date.now().toString(36);
    const shardID = `nav-${tag}`;
    await request.post(`${stack.workspaceURL}/console/api/shards`, {
        headers,
        data: {
            id: shardID, name: `Shard ${tag}`, persistence: "persistent",
            visibility: "isolated", scope_tag: `shard:${shardID}`,
            system_prompt: "x", tool_allowlist: [],
        },
    });
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers, data: { title: "Tgt", content: "x" },
        })
    ).json();
    await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers,
        data: {
            name: `Action ${tag}`, prompt: "hi", cron: "0 7 * * *",
            report_targets: [{ kind: "page", book_slug: "personal", page_id: note.id }],
        },
    });

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Shards: the shard appears as a child.
        await page.locator(".sidebar-cat-shards .sidebar-row-chevron").click();
        const shardKids = page.locator('.sidebar-children[data-category="shards"] a.sidebar-child');
        await expect(shardKids.filter({ hasText: `Shard ${tag}` })).toHaveCount(1, { timeout: 10_000 });

        // Actions: the action appears as a child (was always empty).
        await page.locator(".sidebar-cat-scheduled .sidebar-row-chevron").click();
        const actionKids = page.locator('.sidebar-children[data-category="scheduled"] a.sidebar-child');
        await expect(actionKids.filter({ hasText: `Action ${tag}` })).toHaveCount(1, { timeout: 10_000 });

        // Clicking the action child opens the Actions panel.
        await actionKids.filter({ hasText: `Action ${tag}` }).click();
        await expect(page.locator("#panel-scheduled")).toBeVisible({ timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});
