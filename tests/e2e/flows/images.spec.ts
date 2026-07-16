// images.spec.ts — MEDIA-DIAGRAMS Phase 1: page-attached images.
// Upload is write-gated on the book, bytes are sniffed (declared
// content type ignored), serving is membership-gated and proxied
// through the gateway, big images grow a 400px thumbnail, and the
// editor paste hook uploads instead of inlining base64.

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

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader };
}

// bigPNG renders an 800x600 PNG in the browser (no Node image deps).
async function bigPNG(page: Page): Promise<Buffer> {
    const dataURL = await page.evaluate(() => {
        const c = document.createElement("canvas");
        c.width = 800;
        c.height = 600;
        const g = c.getContext("2d")!;
        g.fillStyle = "#246";
        g.fillRect(0, 0, 800, 600);
        g.fillStyle = "#fa3";
        g.fillRect(100, 100, 400, 250);
        return c.toDataURL("image/png");
    });
    return Buffer.from(dataURL.split(",")[1], "base64");
}

test("upload, serve, thumbnail, and authz", async ({ stack, browser, request }) => {
    const owner = await createTestUser();
    const intruder = await createTestUser();
    const headers = { ...authed(owner), "Content-Type": "application/json" };
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Image Note", content: "# pics\n" },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, owner);
    const page = await ctx.newPage();
    let png: Buffer;
    try {
        await page.goto(stack.workspaceURL);
        png = await bigPNG(page);
    } finally {
        await ctx.close();
    }

    // Upload.
    const uploadURL = `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/media`;
    const up = await request.post(uploadURL, {
        headers: authed(owner),
        multipart: { file: { name: "photo.png", mimeType: "image/png", buffer: png } },
    });
    expect(up.status(), await up.text()).toBe(201);
    const meta = await up.json();
    expect(meta.width).toBe(800);
    expect(meta.height).toBe(600);
    expect(meta.url).toContain("/console/api/media/");

    // Serve: original is the PNG, thumb is a 400px JPEG.
    const orig = await request.get(`${stack.workspaceURL}${meta.url}`, { headers: authed(owner) });
    expect(orig.status()).toBe(200);
    expect(orig.headers()["content-type"]).toBe("image/png");
    expect((await orig.body()).length).toBe(png.length);
    const thumb = await request.get(`${stack.workspaceURL}${meta.thumb_url}`, { headers: authed(owner) });
    expect(thumb.status()).toBe(200);
    expect(thumb.headers()["content-type"]).toBe("image/jpeg");
    expect((await thumb.body()).length).toBeLessThan(png.length);

    // Authz: a non-member reads 404 (no id probing), and can't
    // upload into someone else's page either.
    expect(
        (await request.get(`${stack.workspaceURL}${meta.url}`, { headers: authed(intruder) })).status(),
    ).toBe(404);
    expect(
        (
            await request.post(uploadURL, {
                headers: authed(intruder),
                multipart: { file: { name: "x.png", mimeType: "image/png", buffer: png } },
            })
        ).status(),
    ).toBe(404);

    // Content sniffing: a text file dressed as a PNG is refused.
    const fake = await request.post(uploadURL, {
        headers: authed(owner),
        multipart: {
            file: { name: "evil.png", mimeType: "image/png", buffer: Buffer.from("<svg onload=alert(1)>") },
        },
    });
    expect(fake.status()).toBe(415);

    // Deleting the page cascades the media rows — the URL dies with
    // it (bytes are reaped later by the orphan sweep).
    expect(
        (
            await request.delete(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
                headers: authed(owner),
            })
        ).ok(),
    ).toBeTruthy();
    expect(
        (await request.get(`${stack.workspaceURL}${meta.url}`, { headers: authed(owner) })).status(),
    ).toBe(404);
});

test("the notes ⋯ menu inserts images and diagrams", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const headers = { ...authed(user), "Content-Type": "application/json" };
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Menu Note", content: "menu start\n" },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        const png = await bigPNG(page);
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Menu Note" })
            .click();
        await expect(page.locator(".ProseMirror.toastui-editor-contents")).toContainText("menu start", {
            timeout: 10_000,
        });

        // The ⋯ menu advertises both insertion actions. (The chat
        // surface reuses the same classes for its own hidden menu, so
        // scope to the visible button + the open menu.)
        await page.locator(".notes-overflow-btn:visible").click();
        const openMenu = page.locator(".notes-overflow.is-open");
        await expect(openMenu.locator(".notes-overflow-item", { hasText: "Add image…" })).toBeVisible();
        await expect(openMenu.locator(".notes-overflow-item", { hasText: "Add diagram" })).toBeVisible();

        // Deliver the file straight to the picker input — the menu
        // item just forwards a click to it, and native chooser
        // interception is unreliable under automation. This drives
        // the same change-handler → upload → addImage path.
        await openMenu
            .locator('input[type="file"]')
            .setInputFiles({ name: "menu.png", mimeType: "image/png", buffer: png });
        const img = page.locator('.ProseMirror.toastui-editor-contents img[src*="/console/api/media/"]');
        await expect(img).toBeVisible({ timeout: 15_000 });

        // The menu stayed open (setInputFiles bypassed the closing
        // item-click) — dismiss via the click-away on the TEXT
        // paragraph (the inserted image's resize wrapper swallows
        // clicks), then reopen.
        await page
            .locator(".ProseMirror.toastui-editor-contents p", { hasText: "menu start" })
            .click();
        await expect(page.locator(".notes-overflow.is-open")).toHaveCount(0);

        // Add diagram: starter fence renders inline AND its orange
        // diagram tab opens.
        await page.locator(".notes-overflow-btn:visible").click();
        await page
            .locator(".notes-overflow.is-open .notes-overflow-item", { hasText: "Add diagram" })
            .click();
        await expect(page.locator('.ws-tab[data-category="diagram"]')).toBeVisible({ timeout: 10_000 });
        await expect(page.locator(".diagram-source")).toHaveValue(/graph TD;/);
        // The note itself gained the fence (autosave flushes it).
        await expect
            .poll(async () => {
                const p = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
                        { headers },
                    )
                ).json();
                return p.content || "";
            }, { timeout: 10_000 })
            .toContain("```mermaid");
    } finally {
        await ctx.close();
    }
});

test("images resize via preset chips and the drag handle; width persists as #w=", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const headers = { ...authed(user), "Content-Type": "application/json" };
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Resize Note", content: "resize me\n" },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        const png = await bigPNG(page);
        const up = await request.post(
            `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/media`,
            { headers: authed(user), multipart: { file: { name: "r.png", mimeType: "image/png", buffer: png } } },
        );
        const meta = await up.json();
        await request.patch(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers,
            data: { content: `resize me\n\n![r](${meta.url})\n` },
        });

        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Resize Note" })
            .click();
        const wrap = page.locator(".familiar-img-wrap");
        await expect(wrap).toBeVisible({ timeout: 10_000 });

        // Select → chips + handle appear; M chip sets 50%.
        await wrap.click();
        await expect(wrap).toHaveClass(/is-selected/);
        await wrap.locator(".familiar-img-chip", { hasText: "M" }).click();
        await expect
            .poll(async () => {
                const p = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
                        { headers },
                    )
                ).json();
                return p.content || "";
            }, { timeout: 10_000 })
            .toContain("#w=50)");
        // The wrapper tracks the committed width.
        await expect
            .poll(async () => wrap.evaluate((el: HTMLElement) => el.style.width), { timeout: 5_000 })
            .toBe("50%");

        // Drag the corner handle wider with the real mouse and
        // confirm a new (larger) fragment lands in the markdown.
        await wrap.click();
        const handle = wrap.locator(".familiar-img-handle");
        await expect(handle).toBeVisible();
        const hb = (await handle.boundingBox())!;
        await page.mouse.move(hb.x + hb.width / 2, hb.y + hb.height / 2);
        await page.mouse.down();
        await page.mouse.move(hb.x + 200, hb.y + hb.height / 2, { steps: 8 });
        await page.mouse.up();
        await expect
            .poll(async () => {
                const p = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
                        { headers },
                    )
                ).json();
                const m = /#w=(\d+)\)/.exec(p.content || "");
                return m ? parseInt(m[1], 10) : null;
            }, { timeout: 10_000 })
            .toBeGreaterThan(50);

        // Round trip: markdown mode shows the fragment as plain text.
        await page.locator('button[data-mode="markdown"]').click();
        const md = await page.evaluate(() => {
            const el = document.querySelector(".toastui-editor .ProseMirror:not(.toastui-editor-contents)");
            return el ? (el as HTMLElement).innerText : "";
        });
        expect(md).toMatch(/#w=\d+\)/);
    } finally {
        await ctx.close();
    }
});

test("pasting an image into a note uploads it and the editor renders it back", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const headers = { ...authed(user), "Content-Type": "application/json" };
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Paste Note", content: "before paste\n" },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Paste Note" })
            .click();
        const editor = page.locator(".ProseMirror.toastui-editor-contents");
        await expect(editor).toContainText("before paste", { timeout: 10_000 });

        // Synthesize an image paste: a real File on a ClipboardEvent,
        // exactly what the browser hands ProseMirror.
        await editor.click();
        await page.evaluate(async () => {
            const c = document.createElement("canvas");
            c.width = 600;
            c.height = 400;
            c.getContext("2d")!.fillRect(0, 0, 600, 400);
            const blob: Blob = await new Promise((r) => c.toBlob((b) => r(b!), "image/png"));
            const dt = new DataTransfer();
            dt.items.add(new File([blob], "pasted.png", { type: "image/png" }));
            const target = document.querySelector(".ProseMirror.toastui-editor-contents")!;
            target.dispatchEvent(new ClipboardEvent("paste", {
                clipboardData: dt,
                bubbles: true,
                cancelable: true,
            }));
        });

        // The hook uploads and inserts ![alt](/console/api/media/{id});
        // the editor then loads the image through the authed proxy.
        const img = page.locator('.ProseMirror.toastui-editor-contents img[src*="/console/api/media/"]');
        await expect(img).toBeVisible({ timeout: 15_000 });
        await expect
            .poll(async () => img.evaluate((el: HTMLImageElement) => el.naturalWidth), { timeout: 10_000 })
            .toBe(600);

        // And the stored markdown carries the URL, not base64.
        await expect
            .poll(async () => {
                const p = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
                        { headers },
                    )
                ).json();
                return p.content || "";
            }, { timeout: 10_000 })
            .toContain("/console/api/media/");
        const saved = await (
            await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
                headers,
            })
        ).json();
        expect(saved.content).not.toContain("data:image");
    } finally {
        await ctx.close();
    }
});
