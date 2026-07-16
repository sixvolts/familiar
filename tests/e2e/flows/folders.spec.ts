// folders.spec.ts — chat folders: drag-to-fold + context menu in the
// sidebar, and the folder lifecycle at the API (TESTING-PLAN.md
// §"Phase 3" folders.spec). The drag test drives real HTML5
// drag-and-drop events against the sidebar's folder-grouped tree —
// the exact hit-target surface a CSS refactor breaks silently.
//
// Folder creation has no UI affordance yet, so folders are seeded
// through /console/api/chat/folders; conversations are plain rows
// (no LLM involved — creating one is just a POST).

import { test as base, expect, Page } from "@playwright/test";
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

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

async function seed(request: any, stack: GatewayStack, user: TestUser, folderName: string, convTitle: string) {
    const folder = await (
        await request.post(`${stack.workspaceURL}/console/api/chat/folders`, {
            headers: authed(user),
            data: { name: folderName },
        })
    ).json();
    const conv = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: authed(user),
            data: { title: convTitle, model: "familiar" },
        })
    ).json();
    return { folder, conv };
}

// expandChatCategory opens the sidebar's chat tree (the chevron is
// the expand affordance; the row body opens the surface instead).
async function expandChatCategory(page: Page, stack: GatewayStack) {
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await page.locator(".sidebar-cat-chat .sidebar-row-chevron").click();
    await expect(page.locator(".sidebar-children.sidebar-cat-chat")).toBeVisible({ timeout: 10_000 });
}

// dragRowToFolder performs HTML5 drag-and-drop between sidebar nodes.
// Playwright's high-level dragTo doesn't carry a shared DataTransfer
// through the HTML5 DnD event family, so the events are dispatched
// manually with one DataTransfer object — exactly what the browser
// does, minus the mouse.
async function dragRowToFolder(page: Page, rowText: string, folderText: string) {
    await page.evaluate(
        ({ rowText, folderText }) => {
            const rows = [...document.querySelectorAll(".sidebar-child")];
            const src = rows.find((r) => r.textContent?.includes(rowText));
            const headers = [...document.querySelectorAll(".sidebar-folder-header")];
            const dst = headers.find((h) => h.textContent?.includes(folderText));
            if (!src || !dst) throw new Error(`drag: src=${!!src} dst=${!!dst}`);
            const dt = new DataTransfer();
            const fire = (el: Element, type: string) =>
                el.dispatchEvent(
                    new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: dt }),
                );
            fire(src, "dragstart");
            fire(dst, "dragover");
            fire(dst, "drop");
            fire(src, "dragend");
        },
        { rowText, folderText },
    );
}

test("folder lifecycle: create, rename, move in and out, delete falls back to unfiled", async ({
    stack,
    request,
}) => {
    const user = await createTestUser();
    const { folder, conv } = await seed(request, stack, user, "Projects", "lifecycle conv");
    const base = `${stack.workspaceURL}/console/api`;

    // Rename.
    const renamed = await request.patch(`${base}/chat/folders/${folder.id}`, {
        headers: authed(user),
        data: { name: "Projects 2026" },
    });
    expect(renamed.ok(), `rename: HTTP ${renamed.status()}`).toBeTruthy();
    expect((await renamed.json()).name).toBe("Projects 2026");

    // Move the conversation in…
    const moveIn = await request.post(`${base}/conversations/${conv.id}/move`, {
        headers: authed(user),
        data: { folder_id: folder.id },
    });
    expect(moveIn.ok(), `move in: HTTP ${moveIn.status()}`).toBeTruthy();
    expect((await moveIn.json()).folder_id).toBe(folder.id);

    // …and back out with the empty-string sentinel the UI uses.
    const moveOut = await request.post(`${base}/conversations/${conv.id}/move`, {
        headers: authed(user),
        data: { folder_id: "" },
    });
    expect(moveOut.ok(), `move out: HTTP ${moveOut.status()}`).toBeTruthy();
    expect((await moveOut.json()).folder_id ?? null).toBeNull();

    // Deleting a folder must orphan its conversations gracefully
    // (FK is ON DELETE SET NULL), never delete them.
    await request.post(`${base}/conversations/${conv.id}/move`, {
        headers: authed(user),
        data: { folder_id: folder.id },
    });
    const del = await request.delete(`${base}/chat/folders/${folder.id}`, { headers: authed(user) });
    expect(del.ok(), `delete folder: HTTP ${del.status()}`).toBeTruthy();
    const after = await (
        await request.get(`${base}/conversations/${conv.id}`, { headers: authed(user) })
    ).json();
    const convAfter = after.conversation ?? after;
    expect(convAfter.folder_id ?? null, "conversation must survive folder deletion").toBeNull();
});

test("drag a conversation onto a folder in the sidebar", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const { folder, conv } = await seed(request, stack, user, "Drag Target", "drag me");

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await expandChatCategory(page, stack);

        // With folders present, unfiled conversations live under a
        // collapsed "Uncategorized" group — open it to reach the row.
        await page.locator(".sidebar-folder-uncategorized").click();
        await expect(page.locator(".sidebar-child", { hasText: "drag me" })).toBeVisible({
            timeout: 10_000,
        });

        const moved = page.waitForResponse(
            (r) => r.url().includes(`/conversations/${conv.id}/move`) && r.ok(),
        );
        await dragRowToFolder(page, "drag me", "Drag Target");
        await moved;

        // Server state moved…
        const got = await (
            await request.get(`${stack.workspaceURL}/console/api/conversations/${conv.id}`, {
                headers: authed(user),
            })
        ).json();
        expect((got.conversation ?? got).folder_id).toBe(folder.id);

        // …and the sidebar re-rendered the row under the folder
        // (folder count badge goes to 1).
        await expect(
            page.locator(".sidebar-folder-header", { hasText: "Drag Target" }).locator(".sidebar-folder-count"),
        ).toHaveText("1", { timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});

test("the row context menu moves a conversation out of its folder", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const { folder, conv } = await seed(request, stack, user, "Ctx Folder", "ctx conv");
    // Start INSIDE the folder so the menu offers "Move out of folder".
    await request.post(`${stack.workspaceURL}/console/api/conversations/${conv.id}/move`, {
        headers: authed(user),
        data: { folder_id: folder.id },
    });

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await expandChatCategory(page, stack);

        // The row lives under a collapsed folder group — expand it.
        await page.locator(".sidebar-folder-header", { hasText: "Ctx Folder" }).click();
        const row = page.locator(".sidebar-child", { hasText: "ctx conv" });
        await expect(row).toBeVisible({ timeout: 10_000 });

        await row.click({ button: "right" });
        const menu = page.locator(".sidebar-ctxmenu");
        await expect(menu).toBeVisible();

        const moved = page.waitForResponse(
            (r) => r.url().includes(`/conversations/${conv.id}/move`) && r.ok(),
        );
        await menu.locator(".sidebar-ctxmenu-item", { hasText: "Move out of folder" }).click();
        await moved;

        const got = await (
            await request.get(`${stack.workspaceURL}/console/api/conversations/${conv.id}`, {
                headers: authed(user),
            })
        ).json();
        expect((got.conversation ?? got).folder_id ?? null).toBeNull();
        await expect(menu).toBeHidden();
    } finally {
        await ctx.close();
    }
});

test("folders and conversations are owner-scoped", async ({ stack, request }) => {
    const owner = await createTestUser();
    const intruder = await createTestUser();
    const { folder, conv } = await seed(request, stack, owner, "Private Folder", "private conv");
    const base = `${stack.workspaceURL}/console/api`;

    // Not listed for the intruder…
    const folders = await (
        await request.get(`${base}/chat/folders`, { headers: authed(intruder) })
    ).json();
    expect((folders.items ?? []).map((f: any) => f.id)).not.toContain(folder.id);

    // …and not reachable or movable directly.
    expect([403, 404]).toContain(
        (await request.get(`${base}/conversations/${conv.id}`, { headers: authed(intruder) })).status(),
    );
    expect([400, 403, 404]).toContain(
        (
            await request.post(`${base}/conversations/${conv.id}/move`, {
                headers: authed(intruder),
                data: { folder_id: "" },
            })
        ).status(),
    );
});
