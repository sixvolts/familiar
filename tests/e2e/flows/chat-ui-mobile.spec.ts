// chat-ui-mobile.spec.ts — the phone chat's twins of the desktop chat
// fixes (chat-ui.spec.ts): refusals, tool-call rows on reload, web links
// and per-frame rendering. No model; the stream is fulfilled.

import { test as base, expect, Page, Route } from "@playwright/test";
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
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

async function seed(stack: GatewayStack, user: TestUser, messages: object[]) {
    const conv = await (await fetch(`${stack.workspaceURL}/console/api/conversations`, {
        method: "POST", headers: authed(user), body: JSON.stringify({ title: "Phone", model: "familiar" }),
    })).json();
    for (const m of messages) {
        const r = await fetch(`${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`, {
            method: "POST", headers: authed(user), body: JSON.stringify(m),
        });
        if (!r.ok) throw new Error(`seed: HTTP ${r.status}`);
    }
    return conv.id as string;
}

async function send(page: Page, text: string) {
    await page.locator("#mob-thread-input").fill(text);
    await page.locator("#mob-thread-form").evaluate((f: HTMLFormElement) => f.requestSubmit());
}

const sse = (events: [string, object][]) => events.map(([k, d]) => `event: ${k}\ndata: ${JSON.stringify(d)}\n\n`).join("");

// A refused message shows the reason. The gateway's error is an object,
// and reading it as a string threw: the phone said "Connection lost".
test("MOBILE: a refused message shows the server's reason", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.route("**/api/chat", (route: Route) => route.fulfill({
        status: 409, contentType: "application/json",
        body: JSON.stringify({ error: { message: "shard is disabled", code: 409 } }),
    }));
    await page.goto(`${stack.workspaceURL}/#chat/new`);
    await expect(page.locator("#mob-thread-input")).toBeVisible({ timeout: 15_000 });
    await send(page, "hello");
    await expect(page.locator(".mob-msg.is-assistant").last()).toContainText("shard is disabled", { timeout: 10_000 });
    await expect(page.locator("#mob-thread-scroll")).not.toContainText("Connection lost");
});

// A tool-calling row isn't shown: its prose is in the final reply too.
test("MOBILE: prose written alongside a tool call appears once", async ({ stack, page, context }) => {
    const user = await createTestUser();
    const id = await seed(stack, user, [
        { role: "user", content: "summarize" },
        { role: "assistant", content: "DIGEST: three papers.", tool_calls: [{ id: "c1", name: "save_fact", arguments: {} }] },
        { role: "tool", content: "saved", tool_call_id: "c1" },
        { role: "assistant", content: "DIGEST: three papers.\n\nSaved to memory." },
    ]);
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(`${stack.workspaceURL}/#chat/${id}`);
    await expect(page.locator("#mob-thread-scroll")).toContainText("Saved to memory.", { timeout: 15_000 });
    await expect(page.locator(".mob-msg.is-assistant", { hasText: "DIGEST" })).toHaveCount(1);
});

// A web link in a reply opens outside the app instead of replacing it.
test("MOBILE: web links in a reply open in a new tab", async ({ stack, page, context }) => {
    const user = await createTestUser();
    const id = await seed(stack, user, [
        { role: "user", content: "source?" },
        { role: "assistant", content: "See [the source](https://example.test/paper)." },
    ]);
    await attachSession(context, stack.workspaceURL, user);
    await context.route("https://example.test/**", (route) => route.fulfill({ status: 200, contentType: "text/html", body: "<p>paper</p>" }));
    await page.goto(`${stack.workspaceURL}/#chat/${id}`);
    const link = page.locator("#mob-thread-scroll a", { hasText: "the source" });
    await expect(link).toBeVisible({ timeout: 15_000 });
    const [popup] = await Promise.all([context.waitForEvent("page"), link.click()]);
    await popup.waitForLoadState();
    expect(popup.url()).toBe("https://example.test/paper");
    expect(page.url().startsWith(stack.workspaceURL)).toBe(true);
});

// Tokens render once per frame, not once per token.
test("MOBILE: a streamed reply renders once per frame", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    const events: [string, object][] = [["session", { session_id: "s", persists_reply: true }]];
    let text = "";
    for (let i = 0; i < 300; i++) {
        events.push(["token", { chunk: `w${i} ` }]);
        text += `w${i} `;
    }
    events.push(["done", { content: text, finish: "stop" }]);
    let first = true;
    await page.route("**/api/chat", (route: Route) => {
        const body = first
            ? sse([["session", { session_id: "s", persists_reply: true }], ["token", { chunk: "warm" }], ["done", { content: "warm", finish: "stop" }]])
            : sse(events);
        first = false;
        return route.fulfill({ status: 200, contentType: "text/event-stream", body });
    });
    await page.goto(`${stack.workspaceURL}/#chat/new`);
    await expect(page.locator("#mob-thread-input")).toBeVisible({ timeout: 15_000 });
    await send(page, "warm up");
    await expect(page.locator("#mob-thread-scroll")).toContainText("warm", { timeout: 10_000 });
    await page.evaluate(() => {
        const m = (window as any).marked;
        const orig = m.parse.bind(m);
        (window as any).__parses = 0;
        m.parse = (...a: any[]) => { (window as any).__parses++; return orig(...a); };
    });
    await send(page, "go");
    await expect(page.locator("#mob-thread-scroll")).toContainText("w299", { timeout: 10_000 });
    const parses = await page.evaluate(() => (window as any).__parses);
    expect(parses, `${parses} renders for 300 tokens`).toBeLessThan(30);
});
