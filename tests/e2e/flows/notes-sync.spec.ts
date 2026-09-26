// notes-sync.spec.ts — two-device note sync over SSE (TESTING-PLAN.md
// §"Phase 2" item 1 — "the one that would have caught the clobber").
//
// The sync stack under test, end to end:
//   device A autosave (500ms debounce)
//     → PATCH /console/api/books/personal/page-by-id/{id} with If-Match
//     → pageevents bus → GET /console/api/events/pages (SSE, through
//       the workspace proxy)
//     → device B's familiar:pageEvent listener → refreshCurrentNote()
//       (only when the local editor is clean)
//
// Three invariants:
//   1. A clean device picks up a remote save without a reload.
//   2. A dirty device REFUSES the remote refresh; its own stale save
//      then 409s and surfaces the conflict instead of clobbering.
//   3. The If-Match precondition itself (API-level 409 + current row).
//
// The note is created via API BEFORE any browser opens: creating it
// also creates the personal book + membership row, and the SSE
// handler snapshots memberships once at connect — a stream opened
// before the personal book existed would filter the events away.

import { test as base, expect, Page, APIRequestContext } from "@playwright/test";
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

test.describe.configure({ mode: "serial" });

interface NotePage {
    id: string;
    title: string;
    content: string;
    updated_at: string;
}

async function createNote(
    api: APIRequestContext,
    stack: GatewayStack,
    user: TestUser,
    title: string,
    content: string,
): Promise<NotePage> {
    const resp = await api.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { title, content },
    });
    expect(resp.ok(), `create note: HTTP ${resp.status()}`).toBeTruthy();
    return resp.json();
}

// openNote drives the UI: dashboard → Notes surface → load the note
// → wait for the Toast UI (ProseMirror) editor to mount. The note is
// selected via the familiar:openDoc event — the same contract the
// sidebar's child rows dispatch — because the fresh-tab landing view
// is the splash (no tree rows to click yet).
async function openNote(page: Page, stack: GatewayStack, noteID: string) {
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

    await page.locator(".sidebar-cat-notes").click();
    const shell = page.locator(".notes-shell").first();
    await expect(shell).toBeVisible({ timeout: 10_000 });

    await page.evaluate((id) => {
        window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id } }));
    }, noteID);

    // Toast UI mounts ProseMirror twice (markdown + wysiwyg panes);
    // only the active wysiwyg pane is visible — target it explicitly.
    const editor = shell.locator(".toastui-editor-ww-container .ProseMirror").first();
    await expect(editor).toBeVisible({ timeout: 10_000 });
    return { shell, editor };
}

test("a save on device A reaches a clean device B without a reload", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "sync-target", "original content");

    const ctxA = await browser.newContext();
    const ctxB = await browser.newContext();
    await attachSession(ctxA, stack.workspaceURL, user);
    await attachSession(ctxB, stack.workspaceURL, user);
    const pageA = await ctxA.newPage();
    const pageB = await ctxB.newPage();

    try {
        const a = await openNote(pageA, stack, note.id);
        const b = await openNote(pageB, stack, note.id);
        await expect(b.editor).toContainText("original content");

        // Type on A and wait for the autosave PATCH to land (500ms
        // debounce + round trip). Waiting on the response — not the
        // "Saved" dot, which clears itself after 1.5s — keeps this
        // deterministic.
        const saved = pageA.waitForResponse(
            (r) => r.url().includes(`/page-by-id/${note.id}`) && r.request().method() === "PATCH" && r.ok(),
        );
        await a.editor.click();
        await pageA.keyboard.press("ControlOrMeta+a");
        await pageA.keyboard.type("hello from device A");
        await saved;

        // B picks the edit up via SSE → refreshCurrentNote, and says
        // who made it. No reload, no polling from the test.
        await expect(b.editor).toContainText("hello from device A", { timeout: 10_000 });
        await expect(b.shell.locator(".notes-saved")).toContainText(`Synced — ${user.displayName}`, {
            timeout: 10_000,
        });
    } finally {
        await ctxA.close();
        await ctxB.close();
    }
});

test("a dirty editor refuses the remote refresh and surfaces the conflict", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "clobber-target", "shared baseline");

    const ctxB = await browser.newContext();
    await attachSession(ctxB, stack.workspaceURL, user);
    const pageB = await ctxB.newPage();

    try {
        const b = await openNote(pageB, stack, note.id);
        await expect(b.editor).toContainText("shared baseline");

        // "Device A" is a plain API caller here — it grabs the current
        // updated_at BEFORE B dirties the editor so its write lands in
        // one fast round trip inside B's 500ms debounce window.
        const current: NotePage = await (
            await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
                headers: { Cookie: user.cookieHeader },
            })
        ).json();

        await b.editor.click();
        await pageB.keyboard.press("ControlOrMeta+a");
        await pageB.keyboard.type("local divergence on B");

        const remote = await request.patch(
            `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
            {
                headers: {
                    Cookie: user.cookieHeader,
                    "Content-Type": "application/json",
                    "If-Match": current.updated_at,
                },
                data: { title: "clobber-target", content: "remote edit wins the race" },
            },
        );
        expect(remote.ok(), `remote PATCH: HTTP ${remote.status()}`).toBeTruthy();

        // B's pending autosave fires with a now-stale If-Match. Both edits
        // replaced the same line, so the server can't merge them: 409,
        // saving pauses, and a banner offers the choice. B's local text
        // must survive — this exact path is the old clobber bug.
        const banner = b.shell.locator(".wiki-sync-banner");
        await expect(banner).toBeVisible({ timeout: 10_000 });
        await expect(b.shell.locator(".notes-saved")).toHaveText("Conflict");
        await expect(b.editor).toContainText("local divergence on B");
        await expect(b.editor).not.toContainText("remote edit wins the race");

        // "Use theirs" reloads the server's row and unblocks saving. (The
        // old message sent the user to click the note in the sidebar,
        // which did nothing for the note already open.)
        await banner.getByRole("button", { name: /use theirs/i }).click();
        await expect(b.editor).toContainText("remote edit wins the race", { timeout: 10_000 });
        await expect(banner).toBeHidden();
    } finally {
        await ctxB.close();
    }
});

test("a stale If-Match is refused with 409 and the current row", async ({ stack, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "api-conflict", "v1");

    // First writer moves the row.
    const first = await request.patch(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
        {
            headers: { Cookie: user.cookieHeader, "Content-Type": "application/json", "If-Match": note.updated_at },
            data: { title: "api-conflict", content: "v2" },
        },
    );
    expect(first.ok()).toBeTruthy();

    // Second writer still holds the v1 timestamp → 409 + the winning
    // row in the body, which is what the frontend reconciles from.
    const stale = await request.patch(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
        {
            headers: { Cookie: user.cookieHeader, "Content-Type": "application/json", "If-Match": note.updated_at },
            data: { title: "api-conflict", content: "v3 from a stale copy" },
        },
    );
    expect(stale.status()).toBe(409);
    const body = await stale.json();
    expect(body.error).toBe("stale");
    expect(body.current?.content).toBe("v2");

    // The losing write must not have landed.
    const after: NotePage = await (
        await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers: { Cookie: user.cookieHeader },
        })
    ).json();
    expect(after.content).toBe("v2");
});

async function serverContent(request: APIRequestContext, stack: GatewayStack, user: TestUser, id: string) {
    const r = await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${id}`, {
        headers: { Cookie: user.cookieHeader },
    });
    return ((await r.json()) as NotePage).content;
}

// Two writers editing different paragraphs of the same note both keep
// their edit. The server's three-way merge existed but never ran: every
// client sent the title with each save, which disqualified it.
test("disjoint concurrent edits merge instead of conflicting", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "merge-target", "line one\n\nline two\n\nline three");
    const ctxB = await browser.newContext();
    await attachSession(ctxB, stack.workspaceURL, user);
    const pageB = await ctxB.newPage();
    try {
        const b = await openNote(pageB, stack, note.id);
        await expect(b.editor).toContainText("line three");
        const current: NotePage = await (
            await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
                headers: { Cookie: user.cookieHeader },
            })
        ).json();

        // B appends to the last paragraph...
        await b.editor.click();
        await pageB.keyboard.press("ControlOrMeta+End");
        await pageB.keyboard.type(" edited by B");
        // ...while "device A" changes the first one, inside B's debounce.
        const remote = await request.patch(
            `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
            {
                headers: { Cookie: user.cookieHeader, "Content-Type": "application/json", "If-Match": current.updated_at },
                data: { title: "merge-target", content: current.content.replace("line one", "line one edited by A") },
            },
        );
        expect(remote.ok(), `remote PATCH: HTTP ${remote.status()}`).toBeTruthy();

        await expect
            .poll(() => serverContent(request, stack, user, note.id), { timeout: 10_000 })
            .toContain("line three edited by B");
        const merged = await serverContent(request, stack, user, note.id);
        expect(merged, "A's edit was lost in the merge").toContain("line one edited by A");
        await expect(b.editor).toContainText("line one edited by A", { timeout: 10_000 });
        await expect(b.shell.locator(".wiki-sync-banner")).toHaveCount(0);
    } finally {
        await ctxB.close();
    }
});

// A refresh triggered elsewhere (an AI tool writing some other note fires
// familiar:notesChanged) while the user is typing must not replace the
// editor with the last-saved body. It used to, and the pending autosave
// then saved the reverted text, so the loss was permanent.
test("a refresh while typing doesn't wipe the unsaved text", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "typing-target", "before");
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const n = await openNote(page, stack, note.id);
        await expect(n.editor).toContainText("before");
        await n.editor.click();
        await page.keyboard.press("ControlOrMeta+End");
        await page.keyboard.type(" and after");
        await page.evaluate(() => window.dispatchEvent(new CustomEvent("familiar:notesChanged")));
        await expect(n.editor).toContainText("before and after");
        await expect
            .poll(() => serverContent(request, stack, user, note.id), { timeout: 10_000 })
            .toContain("before and after");
    } finally {
        await ctx.close();
    }
});

// A rename by someone else must not break an open editor. Pages were
// loaded, polled and saved by slug, and the slug follows the title, so
// after a rename every save from the other editor 404'd.
test("the wiki keeps saving after someone else renames the page", async ({ stack, browser, request }) => {
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: `Rename Wiki ${Date.now().toString(36)}` },
        })
    ).json();
    const page = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Groceries", content: "milk" },
        })
    ).json();
    const byId = `${stack.workspaceURL}/console/api/books/${book.slug}/page-by-id/${page.id}`;

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const p = await ctx.newPage();
    try {
        await p.goto(stack.workspaceURL);
        await expect(p.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await p.evaluate(({ slug, id }) => {
            (window as any).FamiliarWorkspace.openDoc("wiki", slug, "Groceries", { pageId: id });
        }, { slug: book.slug, id: page.id });
        const editor = p.locator(".wiki-shell .toastui-editor-ww-container .ProseMirror").first();
        await expect(editor).toContainText("milk", { timeout: 10_000 });

        // Someone else renames it (new slug), then this user keeps typing.
        const renamed = await request.patch(byId, { headers, data: { title: "Shopping" } });
        expect(renamed.ok(), `rename: HTTP ${renamed.status()}`).toBeTruthy();
        expect((await renamed.json()).slug).not.toBe(page.slug);

        await editor.click();
        await p.keyboard.press("ControlOrMeta+End");
        await p.keyboard.type(" and eggs");
        await expect
            .poll(async () => (await (await request.get(byId, { headers })).json()).content, { timeout: 10_000 })
            .toContain("milk and eggs");
    } finally {
        await ctx.close();
    }
});
