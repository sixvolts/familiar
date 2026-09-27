// chat-follow-mobile.spec.ts — the mobile thread stops following a
// streaming reply the moment the reader scrolls up, and follows again once
// they're back at the bottom. The reply comes from a fast stand-in model
// (helpers/paced-model.ts): the scroll event that reports the reader's
// move arrives a frame late, and the follow logic read its flag, so a
// token handled in between snapped the view back. Only a fast stream hits
// that every time.

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

test("MOBILE: a fast stream lets go when the reader scrolls up, and follows again at the bottom", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(`${stack.workspaceURL}/#chat/new`);
    const input = page.locator("#mob-thread-input");
    await expect(input).toBeVisible({ timeout: 15_000 });
    await input.fill("Write a long story.");
    await page.locator("#mob-thread-form").evaluate((f: HTMLFormElement) => f.requestSubmit());

    const scroll = page.locator("#mob-thread-scroll");
    const streaming = page.locator(".mob-msg.is-assistant.is-streaming");
    await expect.poll(() => scroll.evaluate((el) => el.scrollHeight - el.clientHeight), { timeout: 30_000 })
        .toBeGreaterThan(300);

    await scroll.evaluate((el) => { el.scrollTop = 0; });
    await page.waitForTimeout(1_000);
    await expect(streaming, "the reply finished before the check").toHaveCount(1);
    expect(await scroll.evaluate((el) => el.scrollTop), "a token snapped the view back down").toBeLessThan(50);

    // Back at the bottom, it follows again.
    await scroll.evaluate((el) => { el.scrollTop = el.scrollHeight; });
    await page.waitForTimeout(500);
    await expect(streaming).toHaveCount(1);
    expect(await scroll.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight),
        "following didn't resume at the bottom").toBeLessThan(40);
});
