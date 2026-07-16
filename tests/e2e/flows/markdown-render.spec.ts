// markdown-render.spec.ts — guards that chat markdown actually RENDERS
// (marked + DOMPurify) under the app's Content-Security-Policy. The libs
// were vendored locally because the CSP (`script-src 'self'`) blocks the
// CDN they used to load from; without the vendoring chat silently falls
// back to raw <pre> text. This catches a CDN/CSP regression that the
// model-gated chat specs don't (they only assert text content). No model
// needed — the assistant turn is seeded directly.

import { test as base, expect } from "@playwright/test";
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

function authed(u: TestUser) {
    return { Cookie: u.cookieHeader, "Content-Type": "application/json" };
}

test("seeded chat markdown renders to real HTML under the CSP", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const H = authed(user);
    const conv = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: H,
            data: { title: "Markdown check", model: "familiar" },
        })
    ).json();
    await request.post(`${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`, {
        headers: H,
        data: { role: "user", content: "show me formatting" },
    });
    await request.post(`${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`, {
        headers: H,
        data: { role: "assistant", content: "Here you go:\n\n**bold thing**\n\n- first\n- second\n\n`inline code`" },
    });

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.evaluate((id) => {
            const ws: any = (window as any).FamiliarWorkspace;
            ws.openDoc("chat", id, "Markdown check");
        }, conv.id);

        const assistant = page.locator(".chat-msg-assistant").last();
        await expect(assistant).toBeVisible({ timeout: 15_000 });
        // Rendered, not raw: marked produced real elements (so the
        // vendored libs loaded under the CSP) ...
        await expect(assistant.locator("strong")).toHaveText("bold thing");
        await expect(assistant.locator("li")).toHaveCount(2);
        await expect(assistant.locator("code")).toHaveText("inline code");
        // ... and the literal markdown syntax is gone.
        await expect(assistant).not.toContainText("**bold thing**");
    } finally {
        await ctx.close();
    }
});
