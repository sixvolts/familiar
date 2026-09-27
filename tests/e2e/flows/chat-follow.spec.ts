// chat-follow.spec.ts — the desktop chat stops following a streaming
// reply when the reader scrolls up without a wheel or key (a scrollbar
// drag, a touch scroll), and follows again at the bottom. Those gestures
// fire no event the chat could detach on synchronously, so it relied on
// the scroll event, a frame late: a token handled in between snapped the
// view back. The fast stand-in model (helpers/paced-model.ts) lands a
// token in that frame every time.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession } from "../fixtures/user";
import { startPacedModel, PacedModel } from "../helpers/paced-model";

const test = base.extend<{}, { model: PacedModel; stack: GatewayStack }>({
    model: [
        async ({}, use) => {
            const model = await startPacedModel();
            await use(model);
            await model.close();
        },
        { scope: "worker" },
    ],
    stack: [
        async ({ model }, use) => {
            const stack = await start({ admin: true, chatModelURL: model.url });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

test("a scroll with no wheel or key (scrollbar, touch) lets go, and the bottom follows again", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const conv = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
            data: { title: "Follow test", model: "familiar" },
        })
    ).json();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-chat").click();
        await page.evaluate(
            (id) => window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat", id } })),
            conv.id,
        );
        const shell = page.locator(".chat-shell", { hasText: "Follow test" }).first();
        await expect(shell.locator(".chat-input")).toBeVisible({ timeout: 10_000 });
        await shell.locator(".chat-input").fill("Write a long story.");
        await shell.locator(".chat-send-btn").click();

        const messages = shell.locator(".chat-messages");
        const streaming = shell.locator(".chat-msg-streaming");
        await expect.poll(() => messages.evaluate((el) => el.scrollHeight - el.clientHeight), { timeout: 30_000 })
            .toBeGreaterThan(300);

        await messages.evaluate((el) => { el.scrollTop = 0; });
        await page.waitForTimeout(1_000);
        await expect(streaming, "the reply finished before the check").toHaveCount(1);
        expect(await messages.evaluate((el) => el.scrollTop), "a token snapped the view back down").toBeLessThan(50);

        await messages.evaluate((el) => { el.scrollTop = el.scrollHeight; });
        await page.waitForTimeout(500);
        await expect(streaming).toHaveCount(1);
        expect(await messages.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight),
            "following didn't resume at the bottom").toBeLessThan(64);
    } finally {
        await ctx.close();
    }
});
