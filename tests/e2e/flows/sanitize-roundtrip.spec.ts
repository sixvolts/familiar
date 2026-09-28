// sanitize-roundtrip.spec.ts — the editor now sanitizes with the
// shared policy (sanitize.js). Editing a page in WYSIWYG must still
// save back the structure the editor renders: table alignment and
// task lists. (An inline HTML <span style> is dropped on a WYSIWYG
// save; that was already so with the bundled sanitizer, see the
// batch 8 notes.)
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

const SOURCE =
    "Intro with <span style=\"color:red\">red text</span> inline.\n\n" +
    "| Left | Center | Right |\n| :--- | :---: | ---: |\n| a | b | c |\n\n" +
    "- [x] milk\n- [ ] eggs\n\nEnd";

test("editing a note in WYSIWYG keeps its table alignment and task list", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const created = await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
        headers,
        data: { title: "styled", content: SOURCE },
    });
    expect(created.ok()).toBeTruthy();
    const note = await created.json();
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
        await expect(editor).toContainText("End", { timeout: 10_000 });
        await editor.getByText("End").click();
        await page.keyboard.press("End");
        await page.keyboard.type("!");
        await expect
            .poll(
                async () => {
                    const r = await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, { headers });
                    return ((await r.json()).content as string) || "";
                },
                { timeout: 15_000 },
            )
            .toContain("End!");
        const r = await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, { headers });
        const saved = (await r.json()).content as string;
        expect(saved).toMatch(/\|\s*:-+:\s*\|/); // centered column
        expect(saved).toMatch(/\|\s*-+:\s*\|/); // right-aligned column
        expect(saved).toMatch(/[-*] \[x\] milk/);
        expect(saved).toMatch(/[-*] \[ \] eggs/);
    } finally {
        await ctx.close();
    }
});
