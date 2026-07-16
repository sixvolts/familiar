// splash.spec.ts — the reworked splash screens (2026-06-12): each
// surface's landing page shows a compact "+ New" button with that
// section's pinned items below it, and a dense recents list on the
// right. Driven through the real DOM; no model needed.

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

test("chat, notes, and wiki splashes show compact New + pinned left, dense recents right", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const post = (path: string, data: any) =>
        request.post(`${stack.workspaceURL}${path}`, { headers, data });

    // Seed: two conversations (one pinned), two notes (one pinned),
    // a book with two pages (one pinned).
    const convPinned = await (await post("/console/api/conversations", { title: "Pinned Conv" })).json();
    await (await post("/console/api/conversations", { title: "Recent Conv" })).json();
    await request.patch(`${stack.workspaceURL}/console/api/conversations/${convPinned.id}`, {
        headers,
        data: { pinned: true },
    });

    const notePinned = await (
        await post("/console/api/books/personal/pages", { title: "Pinned Note", content: "x" })
    ).json();
    await post("/console/api/books/personal/pages", { title: "Recent Note", content: "x" });
    await post(`/console/api/books/personal/page-by-id/${notePinned.id}/pin`, { pinned: true });

    const book = await (await post("/console/api/books", { name: `Splash ${Date.now().toString(36)}` })).json();
    const pagePinned = await (
        await post(`/console/api/books/${book.slug}/pages`, { title: "Pinned Page", content: "x" })
    ).json();
    await post(`/console/api/books/${book.slug}/pages`, { title: "Recent Page", content: "x" });
    await post(`/console/api/books/${book.slug}/page-by-id/${pagePinned.id}/pin`, { pinned: true });

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // ── Chat splash ────────────────────────────────────────
        await page.locator(".sidebar-cat-chat").click();
        const chatSplash = page.locator(".chat-splash-host");
        await expect(chatSplash.locator(".wiki-empty-tile.is-compact")).toBeVisible({ timeout: 10_000 });
        // Square-ish chip: far narrower than the 220px+ column the
        // old tile filled, but taller than an ordinary pill button
        // (icon stacked over label).
        const btn = (await chatSplash.locator(".wiki-empty-tile.is-compact").boundingBox())!;
        expect(btn.width, "New button should hug its content").toBeLessThan(180);
        expect(btn.height, "New button should be a chip, not a pill").toBeGreaterThan(56);
        expect(btn.width / btn.height, "New button should be square-ish").toBeLessThan(2.2);
        await expect(
            chatSplash.locator(".wiki-splash-tile-col .wiki-splash-row", { hasText: "Pinned Conv" }),
        ).toBeVisible();
        const chatRecents = chatSplash.locator(".wiki-splash-list.is-dense");
        await expect(chatRecents.locator(".wiki-splash-row", { hasText: "Recent Conv" })).toBeVisible();

        // ── Notes splash ───────────────────────────────────────
        await page.locator(".sidebar-cat-notes").click();
        const notesSplash = page.locator(".notes-splash-host");
        await expect(notesSplash.locator(".wiki-empty-tile.is-compact")).toBeVisible({ timeout: 10_000 });
        await expect(
            notesSplash.locator(".wiki-splash-tile-col .wiki-splash-row", { hasText: "Pinned Note" }),
        ).toBeVisible();
        await expect(
            notesSplash.locator(".wiki-splash-list.is-dense .wiki-splash-row", { hasText: "Recent Note" }),
        ).toBeVisible();

        // ── Wiki splash (books level) ──────────────────────────
        await page.locator(".sidebar-cat-wiki").click();
        const wikiHost = page.locator(".notes-empty.wiki-splash-host");
        await expect(wikiHost.locator(".wiki-empty-tile.is-compact")).toBeVisible({ timeout: 10_000 });
        await expect(
            wikiHost.locator(".wiki-splash-tile-col .wiki-splash-row", { hasText: "Pinned Page" }),
        ).toBeVisible();
        const bookRow = wikiHost.locator(".wiki-splash-list.is-dense .wiki-splash-row", {
            hasText: "Splash",
        });
        await expect(bookRow.first()).toBeVisible();

        // ── Book splash ────────────────────────────────────────
        await bookRow.first().click();
        await expect(
            wikiHost.locator(".wiki-splash-tile-col .wiki-splash-row", { hasText: "Pinned Page" }),
        ).toBeVisible({ timeout: 10_000 });
        await expect(
            wikiHost.locator(".wiki-splash-list.is-dense .wiki-splash-row", { hasText: "Recent Page" }),
        ).toBeVisible();
    } finally {
        await ctx.close();
    }
});

test("Archive book lives on the book splash overflow, not a page's menu", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const post = (path: string, data: any) => request.post(`${stack.workspaceURL}${path}`, { headers, data });

    const book = await (await post("/console/api/books", { name: `Arch ${Date.now().toString(36)}` })).json();
    await (await post(`/console/api/books/${book.slug}/pages`, { title: "Solo Page", content: "x" })).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Open the book splash.
        await page.locator(".sidebar-cat-wiki").click();
        const wikiHost = page.locator(".notes-empty.wiki-splash-host");
        const bookRow = wikiHost.locator(".wiki-splash-list.is-dense .wiki-splash-row", { hasText: "Arch" });
        await expect(bookRow.first()).toBeVisible({ timeout: 10_000 });
        await bookRow.first().click();

        // The book-splash overflow (owner) now carries Archive book.
        const splashOverflow = wikiHost.locator(".wiki-splash-overflow");
        await expect(splashOverflow.locator(".notes-overflow-btn")).toBeVisible({ timeout: 10_000 });
        await splashOverflow.locator(".notes-overflow-btn").click();
        await expect(splashOverflow.locator(".notes-overflow-item", { hasText: "Archive book" })).toBeVisible();
        await expect(splashOverflow.locator(".notes-overflow-item", { hasText: "Manage users" })).toBeVisible();

        // Open the page; its header overflow must NOT carry Archive book
        // anymore (but still has Delete page).
        await wikiHost.locator(".wiki-splash-row", { hasText: "Solo Page" }).first().click();
        const pageOverflow = page.locator(".notes-header.is-wiki .notes-overflow").first();
        await expect(pageOverflow.locator(".notes-overflow-btn")).toBeVisible({ timeout: 10_000 });
        await pageOverflow.locator(".notes-overflow-btn").click();
        await expect(pageOverflow.locator(".notes-overflow-item", { hasText: "Delete page" })).toBeVisible();
        await expect(pageOverflow.locator(".notes-overflow-item", { hasText: "Archive book" })).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});
