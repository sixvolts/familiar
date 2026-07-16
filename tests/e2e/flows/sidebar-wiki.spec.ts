// sidebar-wiki.spec.ts — the workspace rail's wiki section: expanding
// the category lists books; expanding a book lazily fetches and shows
// its pages. Regression spec for the "book expands to 'Empty' despite
// having pages" report (2026-06-12).

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

test("expanding a book in the sidebar shows its pages", async ({ stack, browser, request }) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };

    // Seed: one book, two pages (one nested under the other so the
    // tree path is exercised too).
    const bookName = `Probe ${Date.now().toString(36)}`;
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: bookName },
        })
    ).json();
    expect(book.slug, JSON.stringify(book)).toBeTruthy();
    const page1 = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Alpha Page", content: "alpha body" },
        })
    ).json();
    expect(page1.id, JSON.stringify(page1)).toBeTruthy();
    const page2 = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Beta Page", content: "beta body" },
        })
    ).json();
    expect(page2.id, JSON.stringify(page2)).toBeTruthy();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Expand the wiki category in the rail.
        await page.locator(".sidebar-cat-wiki .sidebar-row-chevron").click();
        const children = page.locator('.sidebar-children[data-category="wiki"]');
        const bookRow = children.locator("a.sidebar-child", { hasText: bookName });
        await expect(bookRow).toBeVisible({ timeout: 10_000 });

        // Expand the book row — pages must appear, not "Empty".
        await bookRow.locator(".sidebar-tree-caret").click();
        await expect(children.locator("a.sidebar-child", { hasText: "Alpha Page" })).toBeVisible({
            timeout: 10_000,
        });
        await expect(children.locator("a.sidebar-child", { hasText: "Beta Page" })).toBeVisible();
        await expect(children.locator(".sidebar-folder-empty")).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});

test("creating and deleting a page in the surface updates the rail immediately", async ({
    stack,
    browser,
    request,
}) => {
    // Regression for "notes pages update the rail on add/delete but wiki
    // pages don't" (2026-06-15). The rail caches each book's pages in
    // sidebarWikiPagesCache; only familiar:notesChanged clears it +
    // re-renders. notes.js fired that event on mutation; wiki.js didn't,
    // so a created/deleted page sat stale in the rail until a re-expand.
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const bookName = `Live ${Date.now().toString(36)}`;
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, { headers, data: { name: bookName } })
    ).json();
    // Seed one page so the book + its caret render and the rail caches a
    // page tree (the cache is what used to go stale).
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
                headers,
                data: { title: "Seed Page", content: "seed" },
            })
        ).ok(),
    ).toBeTruthy();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Expand the wiki category + the book so the rail fetches and
        // caches the book's pages.
        await page.locator(".sidebar-cat-wiki .sidebar-row-chevron").click();
        const children = page.locator('.sidebar-children[data-category="wiki"]');
        const bookRow = children.locator("a.sidebar-child", { hasText: bookName });
        await expect(bookRow).toBeVisible({ timeout: 10_000 });
        await bookRow.locator(".sidebar-tree-caret").click();
        await expect(children.locator("a.sidebar-child", { hasText: "Seed Page" })).toBeVisible({
            timeout: 10_000,
        });

        // Open the book in the wiki surface, then create a page through
        // its book-splash "New page" tile.
        await bookRow.click();
        const newPageTile = page.locator(".wiki-empty-tile", { hasText: "New page" });
        await expect(newPageTile).toBeVisible({ timeout: 10_000 });
        await newPageTile.click();

        // The new "Untitled" page must appear in the rail with no manual
        // re-expand — this is the bug.
        await expect(children.locator("a.sidebar-child", { hasText: "Untitled" })).toBeVisible({
            timeout: 10_000,
        });

        // Renaming the page and leaving the title field must update the
        // rail row too (blur flushes the save, then notesChanged clears
        // the per-book page cache so the new title re-fetches).
        const titleInput = page.locator("input.notes-title:visible");
        await titleInput.fill("Renamed Page");
        await titleInput.blur();
        await expect(children.locator("a.sidebar-child", { hasText: "Renamed Page" })).toBeVisible({
            timeout: 10_000,
        });
        await expect(children.locator("a.sidebar-child", { hasText: "Untitled" })).toHaveCount(0);

        // Delete it through the surface overflow — it must vanish from the
        // rail too, while the seed page is left untouched.
        page.once("dialog", (d) => d.accept());
        await page.locator(".notes-overflow-btn:visible").click();
        await page.locator(".notes-overflow-item.danger", { hasText: "Delete page" }).click();
        await expect(children.locator("a.sidebar-child", { hasText: "Renamed Page" })).toHaveCount(0, {
            timeout: 10_000,
        });
        await expect(children.locator("a.sidebar-child", { hasText: "Seed Page" })).toBeVisible();
    } finally {
        await ctx.close();
    }
});

test("a long page tree scrolls inside the rail and never covers the user bubble", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };

    const bookName = `Tall ${Date.now().toString(36)}`;
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: bookName },
        })
    ).json();
    for (let i = 0; i < 40; i++) {
        expect(
            (
                await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
                    headers,
                    data: { title: `Filler Page ${String(i).padStart(2, "0")}`, content: "x" },
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

        // Natural geometry before anything overflows — the long tree
        // must not scrunch the category rows (flex-shrink quirk) and
        // the rail chrome outside the scroll region (Home row) must
        // not move or resize at all.
        const notesRow = page.locator(".sidebar-cat-notes");
        const heightBefore = (await notesRow.boundingBox())!.height;
        const homeRow = page.locator("#sidebar-home-row");
        const homeBefore = (await homeRow.boundingBox())!;

        await page.locator(".sidebar-cat-wiki .sidebar-row-chevron").click();
        const children = page.locator('.sidebar-children[data-category="wiki"]');
        const bookRow = children.locator("a.sidebar-child", { hasText: bookName });
        await expect(bookRow).toBeVisible({ timeout: 10_000 });
        await bookRow.locator(".sidebar-tree-caret").click();
        await expect(children.locator("a.sidebar-child", { hasText: "Filler Page 00" })).toBeVisible({
            timeout: 10_000,
        });

        // Category rows keep their natural height when the region
        // becomes scrollable — the container gives, not the rows.
        const heightAfter = (await notesRow.boundingBox())!.height;
        expect(heightAfter, "category rows must not compress when the rail scrolls").toBeCloseTo(
            heightBefore,
            1,
        );

        // Fixed rail chrome is bolted down: the Home row sits above
        // the scroll region and must not move or shrink.
        const homeAfter = (await homeRow.boundingBox())!;
        expect(homeAfter.y, "Home row must not move when the rail scrolls").toBeCloseTo(homeBefore.y, 1);
        expect(homeAfter.height, "Home row must not shrink when the rail scrolls").toBeCloseTo(
            homeBefore.height,
            1,
        );

        // The categories region must be the thing that scrolls…
        const scrolls = await page
            .locator(".sidebar-categories")
            .evaluate((el) => el.scrollHeight > el.clientHeight);
        expect(scrolls, "expanded tree should overflow into a scrollable rail region").toBe(true);

        // …and the user bubble stays visible and unobstructed: the
        // point at its center must hit the bubble itself, not an
        // overflowing tree row painted on top of it.
        const bubble = page.locator(".sidebar-user");
        await expect(bubble).toBeVisible();
        const box = (await bubble.boundingBox())!;
        const viewport = page.viewportSize()!;
        expect(box.y + box.height).toBeLessThanOrEqual(viewport.height + 1);
        const hitsBubble = await page.evaluate(({ x, y }) => {
            const el = document.elementFromPoint(x, y);
            return !!el && !!el.closest(".sidebar-user");
        }, { x: box.x + box.width / 2, y: box.y + box.height / 2 });
        expect(hitsBubble, "user bubble must not be covered by the page tree").toBe(true);
    } finally {
        await ctx.close();
    }
});

test("wiki pages reparent by drag and un-nest via the context menu", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const bookName = `Nest ${Date.now().toString(36)}`;
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: bookName },
        })
    ).json();
    const parent = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Parent Page", content: "p" },
        })
    ).json();
    const child = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Child Page", content: "c" },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-wiki .sidebar-row-chevron").click();
        const children = page.locator('.sidebar-children[data-category="wiki"]');
        const bookRow = children.locator("a.sidebar-child", { hasText: bookName });
        await expect(bookRow).toBeVisible({ timeout: 10_000 });
        await bookRow.locator(".sidebar-tree-caret").click();
        await expect(children.locator("a.sidebar-child", { hasText: "Child Page" })).toBeVisible({
            timeout: 10_000,
        });

        // Drag "Child Page" onto "Parent Page" with one shared
        // DataTransfer (Playwright's dragTo doesn't carry it).
        await page.evaluate(() => {
            const rows = [...document.querySelectorAll(".sidebar-child")];
            const src = rows.find((r) => r.textContent?.includes("Child Page"));
            const dst = rows.find((r) => r.textContent?.includes("Parent Page"));
            if (!src || !dst) throw new Error("drag rows not found");
            const dt = new DataTransfer();
            const fire = (el: Element, type: string) =>
                el.dispatchEvent(new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: dt }));
            fire(src, "dragstart");
            fire(dst, "dragover");
            fire(dst, "drop");
            fire(src, "dragend");
        });
        await expect
            .poll(async () => {
                const p = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/${book.slug}/page-by-id/${child.id}`,
                        { headers },
                    )
                ).json();
                return p.parent_id ?? null;
            }, { timeout: 10_000 })
            .toBe(parent.id);

        // The child now lives under a collapsed "Parent Page" —
        // expand it, then un-nest via the context menu.
        const parentRow = children.locator("a.sidebar-child", { hasText: "Parent Page" });
        await expect(parentRow.locator(".sidebar-tree-caret.has-children")).toBeVisible({
            timeout: 10_000,
        });
        await parentRow.locator(".sidebar-tree-caret").click();
        const childRow = children.locator("a.sidebar-child", { hasText: "Child Page" });
        await expect(childRow).toBeVisible({ timeout: 10_000 });
        await childRow.click({ button: "right" });
        await page
            .locator(".sidebar-ctxmenu .sidebar-ctxmenu-item", { hasText: "Move to top level" })
            .click();
        await expect
            .poll(async () => {
                const p = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/${book.slug}/page-by-id/${child.id}`,
                        { headers },
                    )
                ).json();
                return p.parent_id ?? null;
            }, { timeout: 10_000 })
            .toBe(null);
    } finally {
        await ctx.close();
    }
});

test("pages with children sort above leaf pages in the rail tree", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const bookName = `Order ${Date.now().toString(36)}`;
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, { headers, data: { name: bookName } })
    ).json();
    // Create the leaf FIRST and name it so both creation order and
    // alphabetical order put it ahead of the parent — so a passing
    // test can only mean the parents-first sort actually reordered.
    const leaf = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Aaa Leaf", content: "leaf" },
        })
    ).json();
    const parent = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Zzz Parent", content: "parent" },
        })
    ).json();
    const inner = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Inner Child", content: "child" },
        })
    ).json();
    expect(leaf.id && parent.id && inner.id, "seed pages").toBeTruthy();
    // Nest the child so "Zzz Parent" has children.
    const mv = await request.post(
        `${stack.workspaceURL}/console/api/books/${book.slug}/page-by-id/${inner.id}/move`,
        { headers, data: { parent_id: parent.id } },
    );
    expect(mv.ok(), `move: HTTP ${mv.status()}`).toBeTruthy();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-wiki .sidebar-row-chevron").click();
        const children = page.locator('.sidebar-children[data-category="wiki"]');
        const bookRow = children.locator("a.sidebar-child", { hasText: bookName });
        await expect(bookRow).toBeVisible({ timeout: 10_000 });
        await bookRow.locator(".sidebar-tree-caret").click();

        // Both top-level pages render (the nested child stays collapsed).
        const parentRow = children.locator("a.sidebar-child", { hasText: "Zzz Parent" });
        const leafRow = children.locator("a.sidebar-child", { hasText: "Aaa Leaf" });
        await expect(parentRow).toBeVisible({ timeout: 10_000 });
        await expect(leafRow).toBeVisible();
        // …and the parent (it has children) sorts ABOVE the leaf, even
        // though the leaf was created first and sorts first alphabetically.
        const order = await children.locator("a.sidebar-child").evaluateAll((rows) =>
            rows.map((r) => r.textContent || ""),
        );
        const parentIdx = order.findIndex((t) => t.includes("Zzz Parent"));
        const leafIdx = order.findIndex((t) => t.includes("Aaa Leaf"));
        expect(parentIdx, JSON.stringify(order)).toBeLessThan(leafIdx);
        // The parent carries a children caret; the leaf does not.
        await expect(parentRow.locator(".sidebar-tree-caret.has-children")).toBeVisible();
        await expect(leafRow.locator(".sidebar-tree-caret.has-children")).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});

test("the Scheduled row opens the panel and wears the teal accent", async ({
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

        const row = page.locator(".sidebar-cat-scheduled");
        await expect(row).toBeVisible();
        // Teal glyph (--teal-400 #2DD4BF), not Home's neutral white.
        const glyphColor = await row
            .locator(".sidebar-glyph")
            .evaluate((el) => getComputedStyle(el).color);
        expect(glyphColor).toBe("rgb(45, 212, 191)");

        await row.click();
        await expect(page.locator("#panel-scheduled")).toBeVisible({ timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});

test("a narrow window keeps the rail clamped — Home fixed, bubble visible", async ({
    stack,
    browser,
    request,
}) => {
    // Below 769px a legacy media query used to flip the shell to a
    // single column, un-clamping the rail's height — every scroll and
    // flex-shrink fix silently stopped applying. This pins the narrow
    // layout: same vertical rail, slimmer column, same guarantees.
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const bookName = `Narrow ${Date.now().toString(36)}`;
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: bookName },
        })
    ).json();
    for (let i = 0; i < 40; i++) {
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: `Filler Page ${String(i).padStart(2, "0")}`, content: "x" },
        });
    }

    const ctx = await browser.newContext({ viewport: { width: 470, height: 950 } });
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        const home = page.locator("#sidebar-home-row");
        const before = (await home.boundingBox())!;

        await page.locator(".sidebar-cat-wiki .sidebar-row-chevron").click();
        const children = page.locator('.sidebar-children[data-category="wiki"]');
        const bookRow = children.locator("a.sidebar-child", { hasText: bookName });
        await expect(bookRow).toBeVisible({ timeout: 10_000 });
        await bookRow.locator(".sidebar-tree-caret").click();
        await expect(
            children.locator("a.sidebar-child", { hasText: "Filler Page 00" }),
        ).toBeVisible({ timeout: 10_000 });

        // Home is bolted down, even after scrolling the region.
        const expanded = (await home.boundingBox())!;
        expect(expanded.y, "Home must not move in a narrow window").toBeCloseTo(before.y, 1);
        expect(expanded.height).toBeCloseTo(before.height, 1);
        await page.locator(".sidebar-categories").evaluate((el) => (el.scrollTop = el.scrollHeight));
        const scrolled = (await home.boundingBox())!;
        expect(scrolled.y).toBeCloseTo(before.y, 1);

        // The rail stays inside the viewport; the bubble is reachable.
        const bubble = (await page.locator(".sidebar-user").boundingBox())!;
        expect(bubble.y + bubble.height).toBeLessThanOrEqual(951);
    } finally {
        await ctx.close();
    }
});

test("the wiki ⋯ menu pins a page for the user and it lands on Home", async ({ stack, browser, request }) => {
    // Pinning a wiki page is a per-user preference (any member, not
    // write-gated). The ⋯ menu item posts to the dedicated /pin endpoint;
    // the pin then shows up in /console/api/home/pins as kind="wiki".
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: `Pin Wiki ${Date.now().toString(36)}` },
        })
    ).json();
    const page = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Pin me", content: "# Pin me\n\nbody" },
        })
    ).json();

    const homePinIds = async () => {
        const r = await request.get(`${stack.workspaceURL}/console/api/home/pins`, { headers });
        return ((await r.json()).items ?? []).map((p: any) => p.id);
    };
    expect(await homePinIds()).not.toContain(page.id);

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const p = await ctx.newPage();
    try {
        await p.goto(stack.workspaceURL);
        await expect(p.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await p.evaluate(({ slug, id }) => {
            (window as any).FamiliarWorkspace.openDoc("wiki", slug, "Pin me", { pageId: id });
        }, { slug: book.slug, id: page.id });

        // Pin via the menu (clicking the item closes the menu).
        await p.locator(".notes-overflow-btn:visible").click();
        await p.locator(".notes-overflow.is-open .notes-overflow-item", { hasText: "Pin page" }).click();
        await expect.poll(homePinIds, { timeout: 10_000 }).toContain(page.id);

        // Re-open: the menu now offers "Unpin page". Act on the same open
        // menu — clicking the item closes it (the ⋯ button toggles, so
        // reopening would just close an already-open menu).
        await p.locator(".notes-overflow-btn:visible").click();
        const unpin = p.locator(".notes-overflow.is-open .notes-overflow-item", { hasText: "Unpin page" });
        await expect(unpin).toBeVisible();
        await unpin.click();
        await expect.poll(homePinIds, { timeout: 10_000 }).not.toContain(page.id);
    } finally {
        await ctx.close();
    }
});
