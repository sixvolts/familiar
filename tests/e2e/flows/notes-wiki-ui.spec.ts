// notes-wiki-ui.spec.ts — the notes and wiki surfaces' fixes: share
// links after edits, saves on hide/unload, images landing in the page
// they were added to, closed tabs letting go, menus and labels, the
// sidebar tree, and the wiki's page events.

import { test as base, expect, Page, BrowserContext, APIRequestContext } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

const PUBLIC_HOST = "share.notes-ui.test";

const test = base.extend<{}, { stack: GatewayStack }>({
    stack: [
        async ({}, use) => {
            const stack = await start({ admin: true, publicHosts: [PUBLIC_HOST] });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

function hdrs(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

async function createNote(api: APIRequestContext, stack: GatewayStack, user: TestUser, title: string, content: string) {
    const r = await api.post(`${stack.workspaceURL}/console/api/books/personal/pages`, { headers: hdrs(user), data: { title, content } });
    expect(r.ok(), `create note: HTTP ${r.status()}`).toBeTruthy();
    return r.json();
}

async function getNote(api: APIRequestContext, stack: GatewayStack, user: TestUser, id: string) {
    return (await api.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${id}`, { headers: hdrs(user) })).json();
}

async function newPage(browser: any, stack: GatewayStack, user: TestUser, init?: () => void): Promise<{ ctx: BrowserContext; page: Page }> {
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    if (init) await page.addInitScript(init);
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    return { ctx, page };
}

async function openNote(page: Page, id: string) {
    await page.locator(".sidebar-cat-notes").click();
    const shell = page.locator(".notes-shell:visible").first();
    await expect(shell).toBeVisible({ timeout: 10_000 });
    await page.evaluate((nid) => {
        window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id: nid } }));
    }, id);
    const editor = shell.locator(".toastui-editor-ww-container .ProseMirror").first();
    await expect(editor).toBeVisible({ timeout: 10_000 });
    return { shell, editor };
}

async function typeAtEnd(page: Page, editor: any, text: string) {
    await editor.click();
    await page.keyboard.press("ControlOrMeta+End");
    await page.keyboard.type(text);
}

// Records clipboard writes, and every fetch with its keepalive flag.
function instrument() {
    (window as any).__copied = [];
    try {
        Object.defineProperty(navigator, "clipboard", {
            configurable: true,
            value: { writeText: async (t: string) => { (window as any).__copied.push(t); } },
        });
    } catch (_) { /* leave the real one */ }
    (window as any).__fetches = [];
    const orig = window.fetch.bind(window);
    window.fetch = (input: any, init?: any) => {
        const url = typeof input === "string" ? input : input.url;
        (window as any).__fetches.push({ url, method: (init && init.method) || "GET", keepalive: !!(init && init.keepalive) });
        return orig(input, init);
    };
}

// "Copy public link" copies the link after the note was edited. The
// save response's share had no public_url and replaced the known one,
// so the menu item did nothing.
test("Copy public link works after an edit", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Shared recipe", "flour");
    const share = await (await request.post(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/share`, {
        headers: hdrs(user), data: { enabled: true },
    })).json();
    const { ctx, page } = await newPage(browser, stack, user, instrument);
    try {
        const { shell, editor } = await openNote(page, note.id);
        await typeAtEnd(page, editor, " and eggs");
        await expect.poll(async () => (await getNote(request, stack, user, note.id)).content, { timeout: 10_000 }).toContain("and eggs");
        await expect(shell.locator(".notes-saved")).not.toHaveText("•");
        await shell.locator(".notes-overflow-btn").click();
        await shell.locator(".notes-overflow.is-open .notes-overflow-item", { hasText: "Copy public link" }).click();
        await expect.poll(() => page.evaluate(() => (window as any).__copied)).toEqual([share.public_url]);
    } finally {
        await ctx.close();
    }
});

// A share turned off elsewhere shows as off once the note saves: the
// old share was carried forward whenever a save response had none.
test("a share turned off elsewhere shows as off after the next save", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Was public", "text");
    const shareURL = `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/share`;
    await request.post(shareURL, { headers: hdrs(user), data: { enabled: true } });
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        const { shell, editor } = await openNote(page, note.id);
        await expect(shell.locator(".notes-share-indicator")).toBeVisible();
        await request.post(shareURL, { headers: hdrs(user), data: { enabled: false } });
        await typeAtEnd(page, editor, " more");
        await expect.poll(async () => (await getNote(request, stack, user, note.id)).content, { timeout: 10_000 }).toContain("more");
        await expect(shell.locator(".notes-share-indicator")).toBeHidden({ timeout: 5_000 });
    } finally {
        await ctx.close();
    }
});

// A pending edit goes out when the page is hidden, and on unload with
// keepalive. Nothing was sent on hide, and the unload save was a plain
// fetch the browser cancelled.
test("an edit is sent, with keepalive, when the page is hidden or unloads", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Hide me", "start");
    const { ctx, page } = await newPage(browser, stack, user, instrument);
    try {
        const { editor } = await openNote(page, note.id);
        const patches = () => page.evaluate((id) =>
            (window as any).__fetches.filter((f: any) => f.method === "PATCH" && f.url.includes(id)), note.id);

        await typeAtEnd(page, editor, " hidden");
        await page.evaluate(() => {
            Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "hidden" });
            document.dispatchEvent(new Event("visibilitychange"));
        });
        // Well inside the 500ms debounce.
        const onHide = await patches();
        expect(onHide.length, "a save on hide").toBe(1);
        expect(onHide[0].keepalive).toBe(true);
        await expect.poll(async () => (await getNote(request, stack, user, note.id)).content).toContain("hidden");

        await page.evaluate(() => {
            Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
        });
        await typeAtEnd(page, editor, " unload");
        await page.evaluate(() => window.dispatchEvent(new Event("beforeunload")));
        const all = await patches();
        expect(all.length, "a save on unload").toBe(2);
        expect(all[1].keepalive).toBe(true);
    } finally {
        await ctx.close();
    }
});

// No links footer appears under a note. It was disabled on load but
// still drawn when the open note changed elsewhere, and then stayed
// under the next note opened.
test("no links footer appears under a note after it changes elsewhere", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Linker", "see [[Somewhere]]");
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        const { shell, editor } = await openNote(page, note.id);
        await expect(editor).toContainText("Somewhere");
        const cur = await getNote(request, stack, user, note.id);
        const r = await request.patch(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers: { ...hdrs(user), "If-Match": cur.updated_at }, data: { content: "see [[Somewhere]] and [[Elsewhere]]" },
        });
        expect(r.ok()).toBeTruthy();
        await page.evaluate(() => window.dispatchEvent(new CustomEvent("familiar:notesChanged")));
        await expect(editor).toContainText("Elsewhere", { timeout: 10_000 });
        await page.waitForTimeout(500);
        await expect(shell.locator(".notes-page-links:visible")).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});

// An image added to a note goes into that note, even if the upload
// finishes after the user moved to another one.
test("an image finishing after a note switch isn't inserted into the other note", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const a = await createNote(request, stack, user, "Photo note", "A body");
    const b = await createNote(request, stack, user, "Other note", "B body");
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        let release: () => void = () => {};
        const gate = new Promise<void>((r) => { release = r; });
        await page.route("**/page-by-id/*/media", async (route) => {
            if (route.request().method() !== "POST") return route.continue();
            await gate;
            await route.continue();
        });
        const { shell, editor } = await openNote(page, a.id);
        await shell.locator(".notes-overflow-btn").click();
        await shell.locator(".notes-overflow.is-open input[type=file]").setInputFiles({
            name: "photo.png", mimeType: "image/png",
            buffer: Buffer.from("89504e470d0a1a0a0000000d4948445200000001000000010806000000" +
                "1f15c4890000000d49444154789c6300010000000500010d0a2db40000000049454e44ae426082", "hex"),
        });
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id } }));
        }, b.id);
        await expect(editor).toContainText("B body", { timeout: 10_000 });
        release();
        await page.waitForTimeout(1500);
        await expect(editor.locator("img")).toHaveCount(0);
        expect((await getNote(request, stack, user, b.id)).content).not.toContain("/console/api/media/");
    } finally {
        await ctx.close();
    }
});

// The same for a pasted image (the editor's upload hook).
test("a pasted image finishing after a note switch isn't inserted into the other note", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const a = await createNote(request, stack, user, "Paste into", "A text");
    const b = await createNote(request, stack, user, "Paste away", "B text");
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        let release: () => void = () => {};
        const gate = new Promise<void>((r) => { release = r; });
        await page.route("**/page-by-id/*/media", async (route) => {
            if (route.request().method() !== "POST") return route.continue();
            await gate;
            await route.continue();
        });
        const { editor } = await openNote(page, a.id);
        await editor.click();
        await page.evaluate(async () => {
            const c = document.createElement("canvas");
            c.width = 20; c.height = 20;
            const blob: Blob = await new Promise((r) => c.toBlob((x) => r(x!), "image/png"));
            const dt = new DataTransfer();
            dt.items.add(new File([blob], "pasted.png", { type: "image/png" }));
            document.querySelector(".notes-shell .ProseMirror.toastui-editor-contents")!
                .dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
        });
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id } }));
        }, b.id);
        await expect(editor).toContainText("B text", { timeout: 10_000 });
        release();
        await page.waitForTimeout(1500);
        await expect(editor.locator("img")).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});

// A note deleted elsewhere while open: the editor says so, offers the
// text back, and stops saving (each save 404'd behind a "check your
// connection" toast).
test("a note deleted elsewhere says so and stops saving", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Deleted note", "keep this text");
    const { ctx, page } = await newPage(browser, stack, user, instrument);
    try {
        const { shell, editor } = await openNote(page, note.id);
        const del = await request.delete(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, { headers: hdrs(user) });
        expect(del.ok()).toBeTruthy();
        const banner = shell.getByRole("alert");
        await expect(banner).toContainText("deleted elsewhere", { timeout: 10_000 });
        const patches = () => page.evaluate((id) =>
            (window as any).__fetches.filter((f: any) => f.method === "PATCH" && f.url.includes(id)).length, note.id);
        const before = await patches();
        await typeAtEnd(page, editor, " more");
        await page.waitForTimeout(1200);
        expect(await patches(), "saves after the delete").toBe(before);
        await banner.getByRole("button", { name: "Save as a new note" }).click();
        await expect(shell.getByRole("alert")).toHaveCount(0, { timeout: 10_000 });
        await expect(editor).toContainText("keep this text more");
    } finally {
        await ctx.close();
    }
});

// A closed notes tab lets go: it kept refetching its note on every
// note event, and kept its editor alive.
test("a closed notes tab stops reacting to note events", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Closing note", "bye");
    const { ctx, page } = await newPage(browser, stack, user, instrument);
    try {
        await openNote(page, note.id);
        await page.locator(".ws-tab", { hasText: "Closing note" }).locator(".ws-tab-close").click();
        await expect(page.locator(".ws-tab", { hasText: "Closing note" })).toHaveCount(0);
        const before = await page.evaluate((id) => (window as any).__fetches.filter((f: any) => f.url.includes(id)).length, note.id);
        await page.evaluate(() => {
            window.dispatchEvent(new CustomEvent("familiar:notesChanged"));
            window.dispatchEvent(new CustomEvent("familiar:pinsChanged"));
        });
        await page.waitForTimeout(800);
        const after = await page.evaluate((id) => (window as any).__fetches.filter((f: any) => f.url.includes(id)).length, note.id);
        expect(after - before, "fetches of the closed note").toBe(0);
    } finally {
        await ctx.close();
    }
});

// Closing a tab right after typing keeps the text: releasing the tab
// must send the edit still in the debounce, not drop it.
test("closing a notes tab right after typing keeps the text", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Quick close", "start");
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        const { editor } = await openNote(page, note.id);
        await typeAtEnd(page, editor, " last words");
        await page.locator(".ws-tab", { hasText: "Quick close" }).locator(".ws-tab-close").click();
        await expect.poll(async () => (await getNote(request, stack, user, note.id)).content, { timeout: 10_000 })
            .toContain("last words");
    } finally {
        await ctx.close();
    }
});

// Closing a tab while a save is in flight, with more typed meanwhile:
// the follow-up save must carry the text, not an empty note read from
// a released editor.
test("closing a notes tab during a save keeps the text typed meanwhile", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Slow close", "start");
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        let release: () => void = () => {};
        const gate = new Promise<void>((r) => { release = r; });
        let patches = 0;
        await page.route(`**/page-by-id/${note.id}`, async (route) => {
            if (route.request().method() !== "PATCH") return route.continue();
            patches++;
            if (patches === 1) await gate;
            await route.continue();
        });
        const { editor } = await openNote(page, note.id);
        await typeAtEnd(page, editor, " first");
        await expect.poll(() => patches, { timeout: 5_000 }).toBe(1);
        await typeAtEnd(page, editor, " second");
        await page.locator(".ws-tab", { hasText: "Slow close" }).locator(".ws-tab-close").click();
        release();
        await expect.poll(async () => (await getNote(request, stack, user, note.id)).content, { timeout: 10_000 })
            .toContain("second");
        await page.waitForTimeout(1000);
        const final = (await getNote(request, stack, user, note.id)).content;
        expect(final).toContain("start first second");
    } finally {
        await ctx.close();
    }
});

// A double-clicked "Share publicly" sends one request (the second
// failed on the server and toasted an error for a shared note).
test("a double-clicked Share publicly sends one request", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Share twice", "x");
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        let posts = 0;
        await page.route("**/page-by-id/*/share", async (route) => {
            posts++;
            await new Promise((r) => setTimeout(r, 300));
            await route.continue();
        });
        const { shell } = await openNote(page, note.id);
        await shell.locator(".notes-overflow-btn").click();
        await page.evaluate(() => {
            const item = [...document.querySelectorAll(".notes-shell .notes-overflow-item")]
                .find((el) => el.textContent === "Share publicly") as HTMLElement;
            item.click();
            item.click();
        });
        await expect(shell.locator(".notes-share-indicator")).toBeVisible({ timeout: 10_000 });
        await page.waitForTimeout(500);
        expect(posts).toBe(1);
    } finally {
        await ctx.close();
    }
});

// The notes controls have names, and the actions menu works from the
// keyboard.
test("the notes menu and fields are labelled and keyboard-operable", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Accessible", "x");
    await request.post(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/share`, { headers: hdrs(user), data: { enabled: true } });
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        const { shell } = await openNote(page, note.id);
        await expect(shell.getByRole("textbox", { name: "Note title" })).toBeVisible();
        // The list pane is hidden in this layout; the field still needs a name.
        await expect(shell.locator('input[type="search"][aria-label="Search notes"]')).toHaveCount(1);
        await expect(shell.getByRole("img", { name: "Page shared publicly" })).toBeVisible();
        const btn = shell.getByRole("button", { name: "Note actions" });
        await expect(btn).toHaveAttribute("aria-haspopup", "menu");
        await expect(btn).toHaveAttribute("aria-expanded", "false");
        await btn.click();
        await expect(btn).toHaveAttribute("aria-expanded", "true");
        const menu = shell.getByRole("menu");
        await expect(menu.getByRole("menuitem", { name: "Delete note" })).toBeVisible();
        await page.keyboard.press("ArrowDown");
        await page.keyboard.press("Escape");
        await expect(btn).toHaveAttribute("aria-expanded", "false");
        await expect(btn).toBeFocused();
    } finally {
        await ctx.close();
    }
});

// A note whose parent is gone is listed at the top of the sidebar tree.
// Pages deleted before deletes moved their children up left children
// pointing at them, and the tree never reached those.
test("a note whose parent is gone shows at the top of the sidebar tree", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const orphan = await createNote(request, stack, user, "Orphaned child", "x");
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        await page.route("**/console/api/books/personal/pages?limit=200", async (route) => {
            const resp = await route.fetch();
            const body = await resp.json();
            for (const it of body.items || []) {
                if (it.id === orphan.id) it.parent_id = "00000000-0000-4000-8000-000000000000";
            }
            await route.fulfill({ response: resp, json: body });
        });
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await expect(page.locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Orphaned child" }))
            .toBeVisible({ timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});

// "Add diagram" opens the fence it just added, counted as the diagram
// tab counts: ```Mermaid and ~~~ fences are diagrams too (the editor
// draws them), and a regex for "```mermaid" missed them.
test("Add diagram opens the new fence on a page with other diagrams", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const content = "```Mermaid\ngraph TD; REAL-->ONE;\n```\n\n~~~mermaid\ngraph TD; T-->W;\n~~~\n";
    const note = await createNote(request, stack, user, "Diagrams", content);
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        const { shell } = await openNote(page, note.id);
        await page.evaluate(() => {
            const ws = (window as any).FamiliarWorkspace;
            const orig = ws.openDoc.bind(ws);
            (window as any).__opened = [];
            ws.openDoc = (...a: any[]) => { (window as any).__opened.push(a); return orig(...a); };
        });
        await shell.locator(".notes-overflow-btn").click();
        await shell.locator(".notes-overflow.is-open .notes-overflow-item", { hasText: "Add diagram" }).click();
        const opened = await page.evaluate(() => (window as any).__opened);
        const diagram = opened.find((a: any[]) => a[0] === "diagram");
        expect(diagram && diagram[3].fence_index).toBe(2);
    } finally {
        await ctx.close();
    }
});

async function createBook(api: APIRequestContext, stack: GatewayStack, user: TestUser, name: string) {
    const r = await api.post(`${stack.workspaceURL}/console/api/books`, { headers: hdrs(user), data: { name } });
    expect(r.ok(), `create book: HTTP ${r.status()}`).toBeTruthy();
    return r.json();
}

async function openWikiPage(page: Page, slug: string, id: string, text: string) {
    await page.evaluate(({ s, i }) => {
        (window as any).FamiliarWorkspace.openDoc("wiki", s, "Wiki", { pageId: i });
    }, { s: slug, i: id });
    const shell = page.locator(".wiki-shell:visible").first();
    const editor = shell.locator(".toastui-editor-ww-container .ProseMirror").first();
    await expect(editor).toContainText(text, { timeout: 10_000 });
    return { shell, editor };
}

// Page events from other books leave the page list alone: every save
// anywhere (a note's autosave, a research run) refetched and rebuilt
// every open wiki tab's list, twice.
test("wiki: an event from another book doesn't refetch the page list", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const book = await createBook(request, stack, user, `Events ${Date.now().toString(36)}`);
    const p = await (await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
        headers: hdrs(user), data: { title: "Home", content: "home page" },
    })).json();
    const { ctx, page } = await newPage(browser, stack, user, instrument);
    try {
        await openWikiPage(page, book.slug, p.id, "home page");
        const lists = () => page.evaluate((slug) =>
            (window as any).__fetches.filter((f: any) => f.url.endsWith(`/books/${slug}/pages`)).length, book.slug);
        const before = await lists();
        await page.evaluate(() => window.dispatchEvent(new CustomEvent("familiar:pageEvent", {
            detail: { kind: "page-saved", book_id: "00000000-0000-4000-8000-000000000001", page_id: "00000000-0000-4000-8000-000000000002" },
        })));
        await page.waitForTimeout(800);
        expect(await lists(), "list fetches after another book's event").toBe(before);
        // This book's events still refresh it (once, debounced).
        await page.evaluate((bid) => {
            for (let i = 0; i < 3; i++) {
                window.dispatchEvent(new CustomEvent("familiar:pageEvent", {
                    detail: { kind: "page-saved", book_id: bid, page_id: "00000000-0000-4000-8000-00000000000" + i },
                }));
            }
        }, book.id);
        await expect.poll(lists, { timeout: 3_000 }).toBe(before + 1);
    } finally {
        await ctx.close();
    }
});

// The open page deleted elsewhere: the editor says so and stops saving
// (every save failed silently while the user kept typing).
test("wiki: the open page deleted elsewhere says so and stops saving", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const book = await createBook(request, stack, user, `Deleted ${Date.now().toString(36)}`);
    const p = await (await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
        headers: hdrs(user), data: { title: "Doomed", content: "doomed page" },
    })).json();
    const { ctx, page } = await newPage(browser, stack, user, instrument);
    try {
        const { shell, editor } = await openWikiPage(page, book.slug, p.id, "doomed page");
        const del = await request.delete(`${stack.workspaceURL}/console/api/books/${book.slug}/page-by-id/${p.id}`, { headers: hdrs(user) });
        expect(del.ok()).toBeTruthy();
        const banner = shell.getByRole("alert");
        await expect(banner).toContainText("deleted elsewhere", { timeout: 10_000 });
        await expect(banner.getByRole("button", { name: "Copy the text" })).toBeVisible();
        const patches = () => page.evaluate((id) =>
            (window as any).__fetches.filter((f: any) => f.method === "PATCH" && f.url.includes(id)).length, p.id);
        const before = await patches();
        await typeAtEnd(page, editor, " still typing");
        await page.waitForTimeout(1200);
        expect(await patches(), "saves after the delete").toBe(before);
        await expect(shell.locator(".notes-saved")).toHaveText("Deleted");
    } finally {
        await ctx.close();
    }
});

// The book splash's menu listener doesn't pile up: each render of the
// splash added a document click listener that was never removed.
test("wiki: re-rendering the book splash doesn't pile up document listeners", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const name = `Splash ${Date.now().toString(36)}`;
    const book = await createBook(request, stack, user, name);
    const { ctx, page } = await newPage(browser, stack, user, () => {
        const orig = EventTarget.prototype.addEventListener;
        (window as any).__docClicks = [];
        EventTarget.prototype.addEventListener = function (type: string, fn: any, opts?: any) {
            if (this === document && type === "click") (window as any).__docClicks.push((opts && opts.signal) || null);
            return orig.call(this, type, fn, opts);
        };
    });
    try {
        const live = () => page.evaluate(() => (window as any).__docClicks.filter((s: any) => !s || !s.aborted).length);
        await page.evaluate((s) => (window as any).FamiliarWorkspace.openDoc("wiki", s, "Wiki"), book.slug);
        const shell = page.locator(".wiki-shell:visible").first();
        await expect(shell.locator(".wiki-splash-title", { hasText: name })).toBeVisible({ timeout: 10_000 });
        const first = await live();
        for (let i = 0; i < 3; i++) {
            await page.evaluate(() => window.dispatchEvent(new CustomEvent("familiar:surfaceNavRoot", { detail: { surface: "wiki" } })));
            await expect(shell.locator(".wiki-splash-title", { hasText: "Your books" })).toBeVisible({ timeout: 10_000 });
            await shell.locator(".wiki-splash-row-title", { hasText: name }).click();
            await expect(shell.locator(".wiki-splash-title", { hasText: name })).toBeVisible({ timeout: 10_000 });
        }
        expect(await live(), "live document click listeners after 3 more splash renders").toBe(first);
    } finally {
        await ctx.close();
    }
});

// Wiki controls have names; icon buttons read "plus", "⋯" and "✕".
test("wiki: icon controls and fields are labelled", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const book = await createBook(request, stack, user, `Labels ${Date.now().toString(36)}`);
    const p = await (await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
        headers: hdrs(user), data: { title: "Labelled", content: "label page" },
    })).json();
    const { ctx, page } = await newPage(browser, stack, user);
    try {
        const { shell } = await openWikiPage(page, book.slug, p.id, "label page");
        // The left rail is hidden in this layout; its controls still need names.
        await expect(shell.locator('button.wiki-new-page-btn[aria-label="New page"]')).toHaveCount(1);
        await expect(shell.locator('select.wiki-book-select[aria-label="Book"]')).toHaveCount(1);
        await expect(shell.getByRole("textbox", { name: "Page title" })).toBeVisible();
        const btn = shell.getByRole("button", { name: "Page actions" });
        await btn.click();
        await expect(btn).toHaveAttribute("aria-expanded", "true");
        await page.keyboard.press("Escape");
        await expect(btn).toHaveAttribute("aria-expanded", "false");
        await expect(shell.locator(".notes-saved")).toHaveAttribute("aria-live", "polite");
    } finally {
        await ctx.close();
    }
});
