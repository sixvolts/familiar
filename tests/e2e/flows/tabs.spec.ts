// tabs.spec.ts — tab-targeting rules for sidebar opens (2026-06-12
// rework). The contract: clicking a doc in the rail NEVER overrides
// a tab that has a document loaded. Order of preference:
//   1. doc already open in some tab → focus that tab
//   2. an empty tab (splash) of the SAME surface → load it there
//   3. otherwise → NEW tab in the left-most panel
// Other surfaces' splashes are never repurposed by a doc click.

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

test("sidebar doc clicks never override doc-bearing tabs; new tabs land left-most", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    for (const title of ["First Note", "Second Note"]) {
        expect(
            (
                await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
                    headers,
                    data: { title, content: title + " body" },
                })
            ).ok(),
        ).toBeTruthy();
    }

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        // Land in the workspace: default layout is split-2x1 with a
        // chat splash in panel A and a notes splash in panel B.
        await page.locator(".sidebar-cat-chat").click();
        const panelA = page.locator('.ws-panel[data-slot="A"]');
        const panelB = page.locator('.ws-panel[data-slot="B"]');
        await expect(panelA).toBeVisible();
        await expect(panelB).toBeVisible();
        await expect(panelA.locator(".ws-tab")).toHaveCount(1);
        await expect(panelB.locator(".ws-tab")).toHaveCount(1);

        // Expand the Notes category in the rail.
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        const noteRow = (t: string) =>
            page.locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: t });

        // 1st click: the notes SPLASH (panel B) takes the doc — an
        // empty same-surface tab overrides nothing.
        await noteRow("First Note").click();
        await expect(panelB.locator(".ws-tab", { hasText: "First Note" })).toBeVisible({
            timeout: 10_000,
        });
        await expect(panelB.locator(".ws-tab")).toHaveCount(1);

        // 2nd click: no empty notes tab left. The open note must NOT
        // be replaced — a NEW tab opens, and it lands in the
        // LEFT-MOST panel (A), next to the untouched chat splash.
        await noteRow("Second Note").click();
        await expect(panelA.locator(".ws-tab", { hasText: "Second Note" })).toBeVisible({
            timeout: 10_000,
        });
        await expect(panelA.locator(".ws-tab")).toHaveCount(2);
        await expect(panelB.locator(".ws-tab")).toHaveCount(1);
        await expect(panelB.locator(".ws-tab", { hasText: "First Note" })).toBeVisible();

        // 3rd click on an already-open doc: dedup — focus, no new tab.
        await noteRow("First Note").click();
        await expect(panelB.locator(".ws-tab.is-active", { hasText: "First Note" })).toBeVisible({
            timeout: 10_000,
        });
        await expect(panelA.locator(".ws-tab")).toHaveCount(2);
        await expect(panelB.locator(".ws-tab")).toHaveCount(1);
    } finally {
        await ctx.close();
    }
});

test("open documents survive a reload, and layout switches migrate tabs without loss", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
        headers,
        data: { title: "Persistent Note", content: "still here\n" },
    });

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Persistent Note" })
            .click();
        await expect(page.locator(".ws-tab", { hasText: "Persistent Note" })).toBeVisible({
            timeout: 10_000,
        });

        // Reload: the workspace state restores from localStorage and
        // the note rehydrates — title on the tab, content on screen.
        await page.reload();
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        // Boot lands in Home mode — bring the workspace forward.
        await page.evaluate(() => (window as any).appSwitchPanel("workspace"));
        await expect(page.locator(".ws-tab", { hasText: "Persistent Note" })).toBeVisible({
            timeout: 10_000,
        });
        await expect(page.locator(".ProseMirror.toastui-editor-contents")).toContainText("still here", {
            timeout: 10_000,
        });

        // Layout switch: collapsing to single migrates every tab into
        // the surviving panel — nothing is lost.
        const before = await page.locator(".ws-tab").count();
        await page.locator('.ws-layout-btn[data-layout="single"]').click();
        await expect(page.locator('.ws-panel[data-slot="A"] .ws-tab')).toHaveCount(before);
        await expect(page.locator(".ws-tab", { hasText: "Persistent Note" })).toBeVisible();
        // And back out: tabs stay where they are, still none lost.
        await page.locator('.ws-layout-btn[data-layout="split-2x1"]').click();
        expect(await page.locator(".ws-tab").count()).toBe(before);
    } finally {
        await ctx.close();
    }
});

test("tab context menu (right-click + touch long-press) moves tabs between panels", async ({
    stack,
    browser,
}) => {
    const user = await createTestUser({ role: "admin" });
    const ctx = await browser.newContext({ hasTouch: true });
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-chat").click();
        const panelA = page.locator('.ws-panel[data-slot="A"]');
        const panelB = page.locator('.ws-panel[data-slot="B"]');
        await expect(panelA.locator(".ws-tab")).toHaveCount(1);
        await expect(panelB.locator(".ws-tab")).toHaveCount(1);

        // Right-click: move the chat tab from A to the right panel.
        await panelA.locator(".ws-tab").first().click({ button: "right" });
        const menu = page.locator(".sidebar-ctxmenu");
        await expect(menu).toBeVisible();
        await expect(menu.locator(".sidebar-ctxmenu-item", { hasText: "Move to right panel" })).toBeVisible();
        await menu.locator(".sidebar-ctxmenu-item", { hasText: "Move to right panel" }).click();
        await expect(panelA.locator(".ws-tab")).toHaveCount(0);
        await expect(panelB.locator(".ws-tab")).toHaveCount(2);

        // Touch long-press on a tab in B: synthesize the pointer
        // sequence the iPad produces (pointerdown touch, hold past
        // the 450ms threshold, no movement) and move it back left.
        const tab = panelB.locator(".ws-tab", { hasText: "Chat" }).first();
        await tab.evaluate((el) => {
            const r = el.getBoundingClientRect();
            el.dispatchEvent(new PointerEvent("pointerdown", {
                pointerType: "touch", isPrimary: true, bubbles: true,
                clientX: r.x + r.width / 2, clientY: r.y + r.height / 2,
            }));
        });
        await page.waitForTimeout(600);
        await tab.evaluate((el) => {
            el.dispatchEvent(new PointerEvent("pointerup", {
                pointerType: "touch", isPrimary: true, bubbles: true,
            }));
        });
        await expect(menu).toBeVisible();
        await menu.locator(".sidebar-ctxmenu-item", { hasText: "Move to left panel" }).click();
        await expect(panelA.locator(".ws-tab")).toHaveCount(1);
        await expect(panelB.locator(".ws-tab")).toHaveCount(1);

        // Close via the menu, for completeness.
        await panelA.locator(".ws-tab").first().click({ button: "right" });
        await menu.locator(".sidebar-ctxmenu-item", { hasText: "Close tab" }).click();
        await expect(panelA.locator(".ws-tab")).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});

test("a stale tab restore lands on the splash, and home pins obey the contract", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const doomed = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Doomed Note", content: "x" },
        })
    ).json();
    const pinned = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Pinned Survivor", content: "x" },
        })
    ).json();
    await request.post(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${pinned.id}/pin`,
        { headers, data: { pinned: true } },
    );

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        // Open the doomed note, then delete it server-side and
        // reload — the persisted tab points at a dead doc and must
        // come back as a notes splash, not an error pane.
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Doomed Note" })
            .click();
        const panelB = page.locator('.ws-panel[data-slot="B"]');
        await expect(panelB.locator(".ws-tab", { hasText: "Doomed Note" })).toBeVisible({
            timeout: 10_000,
        });

        await request.delete(
            `${stack.workspaceURL}/console/api/books/personal/page-by-id/${doomed.id}`,
            { headers },
        );
        await page.reload();
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes").click();
        // The restored tab self-heals to the splash (its New-note
        // compact button is the marker), with no error text.
        await expect(page.locator(".notes-splash-host .wiki-empty-tile.is-compact")).toBeVisible({
            timeout: 10_000,
        });
        await expect(page.locator("text=Couldn't load note")).toHaveCount(0);

        // Home pin click routes through the doc-open contract: the
        // pinned note opens and is deduped on a second click.
        await page.locator("#sidebar-home-row").click();
        const pinRow = page.locator("#home-pinned-grid", { hasText: "Pinned Survivor" });
        await expect(pinRow).toBeVisible({ timeout: 10_000 });
        await page.locator("#home-pinned-grid >> text=Pinned Survivor").click();
        await expect(page.locator(".ws-tab", { hasText: "Pinned Survivor" })).toBeVisible({
            timeout: 10_000,
        });
        const tabCount = await page.locator(".ws-tab").count();
        await page.locator("#sidebar-home-row").click();
        await page.locator("#home-pinned-grid >> text=Pinned Survivor").click();
        await expect(page.locator(".ws-tab", { hasText: "Pinned Survivor" })).toBeVisible({
            timeout: 10_000,
        });
        expect(await page.locator(".ws-tab").count()).toBe(tabCount);
    } finally {
        await ctx.close();
    }
});
