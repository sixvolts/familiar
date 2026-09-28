// chat-mobile.spec.ts — mobile chat against a real model (the "mobile"
// project runs it; the desktop project ignores *mobile.spec.ts). A
// fulfilled route can't pace tokens, so streaming behaviour needs a live
// model.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession } from "../fixtures/user";
import { MODEL_URL, gateOnModel } from "../helpers/model";

const REPLY_TIMEOUT = 120_000;

const test = base.extend<{}, { stack: GatewayStack }>({
    stack: [
        async ({}, use) => {
            const stack = await start({ admin: true, chatModelURL: MODEL_URL });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

gateOnModel(test, "mobile chat specs", REPLY_TIMEOUT);

// Scrolling up while a reply streams stays put: every token used to snap
// the thread back to the bottom.
test("MOBILE: reading back while a reply streams isn't yanked to the bottom", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(`${stack.workspaceURL}/#chat/new`);
    const input = page.locator("#mob-thread-input");
    await expect(input).toBeVisible({ timeout: 15_000 });
    await input.fill("Write a story of at least 600 words about a lighthouse keeper, in many short paragraphs. No preamble.");
    await page.locator("#mob-thread-form").evaluate((f: HTMLFormElement) => f.requestSubmit());

    const bubble = page.locator(".mob-msg.is-assistant.is-streaming .bubble").last();
    const scroll = page.locator("#mob-thread-scroll");
    // Wait until the thread overflows by a good margin, so there is
    // somewhere to scroll back to.
    await expect.poll(() => scroll.evaluate((el) => el.scrollHeight - el.clientHeight), { timeout: REPLY_TIMEOUT })
        .toBeGreaterThan(300);
    await scroll.evaluate((el) => { el.scrollTop = 0; });
    const lenAtScroll = ((await bubble.textContent()) || "").length;
    await page.waitForTimeout(2_000);
    const lenAfter = ((await bubble.textContent().catch(() => null)) || "").length;
    expect(lenAfter, "the reply stopped streaming before it could be tested").toBeGreaterThan(lenAtScroll);
    expect(await scroll.evaluate((el) => el.scrollTop), "streaming pulled the view back down").toBeLessThan(50);
});
