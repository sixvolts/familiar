// chat-ui.spec.ts — the desktop chat surface's behaviour around a turn,
// without a model: refusals, reloads mid-turn, links, focus, rendering,
// renames, accessibility and cleanup. The chat stream is fulfilled by
// request interception where a turn is needed.

import { test as base, expect, Page, BrowserContext, Route } from "@playwright/test";
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

async function seedConversation(stack: GatewayStack, user: TestUser, title: string, messages: object[]) {
    const r = await fetch(`${stack.workspaceURL}/console/api/conversations`, {
        method: "POST",
        headers: authed(user),
        body: JSON.stringify({ title, model: "familiar" }),
    });
    const conv = await r.json();
    for (const m of messages) {
        const w = await fetch(`${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`, {
            method: "POST",
            headers: authed(user),
            body: JSON.stringify(m),
        });
        if (!w.ok) throw new Error(`seed message: HTTP ${w.status} ${await w.text()}`);
    }
    return conv as { id: string };
}

async function openPage(stack: GatewayStack, user: TestUser, browser: any): Promise<{ ctx: BrowserContext; page: Page }> {
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await page.locator(".sidebar-cat-chat").click();
    await expect(page.locator(".chat-shell").first()).toBeVisible({ timeout: 10_000 });
    return { ctx, page };
}

async function openConversation(page: Page, id?: string) {
    await page.evaluate((cid) => {
        window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: cid ? { surface: "chat", id: cid } : { surface: "chat" } }));
    }, id);
    const shell = page.locator(".chat-shell:visible").first();
    await expect(shell.locator(".chat-input")).toBeVisible({ timeout: 10_000 });
    return shell;
}

function sse(events: [string, object][]) {
    return events.map(([kind, data]) => `event: ${kind}\ndata: ${JSON.stringify(data)}\n\n`).join("");
}

// fulfillChat answers /api/chat with the given SSE body, after delayMs.
async function fulfillChat(page: Page, body: string, delayMs = 0) {
    await page.route("**/api/chat", async (route: Route) => {
        if (delayMs) await new Promise((r) => setTimeout(r, delayMs));
        await route.fulfill({ status: 200, contentType: "text/event-stream", body });
    });
}

const reply = (text: string) => sse([
    ["session", { session_id: "s", persists_reply: true }],
    ["token", { chunk: text }],
    ["done", { content: text, finish: "stop" }],
]);

// A refused message (409 shard disabled, 429 busy, 400 too large) shows
// the server's reason. It was treated as a dropped connection: "Connection
// lost…", a recovery poll, and the reason never shown.
test("a refused message shows the server's reason", async ({ stack, browser }) => {
    const user = await createTestUser();
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const shell = await openConversation(page);
        await page.route("**/api/chat", (route) => route.fulfill({
            status: 409, contentType: "application/json",
            body: JSON.stringify({ error: { message: "shard is disabled", code: 409 } }),
        }));
        await shell.locator(".chat-input").fill("hello");
        await shell.locator(".chat-send-btn").click();
        await expect(shell.locator(".chat-error")).toContainText("shard is disabled", { timeout: 10_000 });
        await expect(shell.locator(".chat-messages")).not.toContainText("Connection lost");
        await expect(shell.locator(".chat-send-btn")).toBeEnabled();
    } finally {
        await ctx.close();
    }
});

// A tool-calling assistant row isn't shown on reload: its prose is also in
// the turn's final reply, so the digest appeared twice.
test("prose written alongside a tool call appears once on reload", async ({ stack, browser }) => {
    const user = await createTestUser();
    const conv = await seedConversation(stack, user, "Digest", [
        { role: "user", content: "summarize the papers" },
        { role: "assistant", content: "DIGEST: three papers.", tool_calls: [{ id: "c1", name: "save_fact", arguments: {} }] },
        { role: "tool", content: "saved", tool_call_id: "c1" },
        { role: "assistant", content: "DIGEST: three papers.\n\nSaved to memory." },
    ]);
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const shell = await openConversation(page, conv.id);
        await expect(shell.locator(".chat-messages")).toContainText("Saved to memory.", { timeout: 10_000 });
        await expect(shell.locator(".chat-msg-assistant", { hasText: "DIGEST" })).toHaveCount(1);
    } finally {
        await ctx.close();
    }
});

// A conversation ending on a user message whose reply is still being
// written (a reload mid-turn) waits for it with Send disabled; it said
// "interrupted, send a message to continue", inviting a second turn.
test("a reply still running after a reload is waited for, not re-asked", async ({ stack, browser }) => {
    const user = await createTestUser();
    const conv = await seedConversation(stack, user, "Mid-turn", [{ role: "user", content: "write the long page" }]);
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        let running = true;
        await page.route("**/api/chat/status**", (route) => route.fulfill({
            status: 200, contentType: "application/json", body: JSON.stringify({ running }),
        }));
        const shell = await openConversation(page, conv.id);
        await expect(shell.locator(".chat-interrupted-note")).toContainText("still being written", { timeout: 10_000 });
        await expect(shell.locator(".chat-send-btn")).toBeDisabled();

        // The turn finishes: its reply lands in the conversation.
        await fetch(`${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`, {
            method: "POST", headers: authed(user),
            body: JSON.stringify({ role: "assistant", content: "Here is the long page." }),
        });
        running = false;
        await expect(shell.locator(".chat-messages")).toContainText("Here is the long page.", { timeout: 20_000 });
        await expect(shell.locator(".chat-send-btn")).toBeEnabled();
    } finally {
        await ctx.close();
    }
});

// A web link in a reply opens in a new tab; in this one it replaced the
// whole workspace.
test("web links in a reply open in a new tab", async ({ stack, browser }) => {
    const user = await createTestUser();
    const conv = await seedConversation(stack, user, "Links", [
        { role: "user", content: "source?" },
        { role: "assistant", content: "See [the source](https://example.test/paper)." },
    ]);
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        await ctx.route("https://example.test/**", (route) => route.fulfill({ status: 200, contentType: "text/html", body: "<p>paper</p>" }));
        const shell = await openConversation(page, conv.id);
        const link = shell.locator(".chat-messages a", { hasText: "the source" });
        await expect(link).toBeVisible({ timeout: 10_000 });
        const [popup] = await Promise.all([ctx.waitForEvent("page"), link.click()]);
        await popup.waitForLoadState();
        expect(popup.url()).toBe("https://example.test/paper");
        expect(page.url().startsWith(stack.workspaceURL)).toBe(true);
    } finally {
        await ctx.close();
    }
});

// A finished turn doesn't take focus from where the user went meanwhile
// (a note in the other pane): the rest of their sentence landed in the
// composer and the next Enter sent it.
test("a finished reply leaves focus where the user put it", async ({ stack, browser }) => {
    const user = await createTestUser();
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const shell = await openConversation(page);
        await fulfillChat(page, reply("done."), 1500);
        await shell.locator(".chat-input").fill("hi");
        await shell.locator(".chat-send-btn").click();
        await page.evaluate(() => {
            const other = document.createElement("input");
            other.id = "elsewhere";
            document.body.appendChild(other);
            other.focus();
        });
        await expect(shell.locator(".chat-messages")).toContainText("done.", { timeout: 10_000 });
        await expect(shell.locator(".chat-send-btn")).toHaveText("Send");
        expect(await page.evaluate(() => document.activeElement && document.activeElement.id)).toBe("elsewhere");

        // Still in the chat: focus returns to the composer.
        await fulfillChat(page, reply("again."));
        await shell.locator(".chat-input").fill("once more");
        await shell.locator(".chat-send-btn").click();
        await expect(shell.locator(".chat-messages")).toContainText("again.", { timeout: 10_000 });
        await expect(shell.locator(".chat-input")).toBeFocused();
    } finally {
        await ctx.close();
    }
});

// Tokens render once per frame. Every token re-parsed and rebuilt the
// whole message: 300 tokens, 300 full renders.
test("a streamed reply renders once per frame, not once per token", async ({ stack, browser }) => {
    const user = await createTestUser();
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const shell = await openConversation(page);
        await fulfillChat(page, reply("warm-up"));
        await shell.locator(".chat-input").fill("warm up");
        await shell.locator(".chat-send-btn").click();
        await expect(shell.locator(".chat-messages")).toContainText("warm-up", { timeout: 10_000 });

        const tokens: [string, object][] = [["session", { session_id: "s", persists_reply: true }]];
        let text = "";
        for (let i = 0; i < 300; i++) {
            tokens.push(["token", { chunk: `w${i} ` }]);
            text += `w${i} `;
        }
        tokens.push(["done", { content: text, finish: "stop" }]);
        await page.unroute("**/api/chat");
        await fulfillChat(page, sse(tokens));
        await page.evaluate(() => {
            const m = (window as any).marked;
            const orig = m.parse.bind(m);
            (window as any).__parses = 0;
            m.parse = (...a: any[]) => { (window as any).__parses++; return orig(...a); };
        });
        await shell.locator(".chat-input").fill("go");
        await shell.locator(".chat-send-btn").click();
        await expect(shell.locator(".chat-messages")).toContainText("w299", { timeout: 10_000 });
        const parses = await page.evaluate(() => (window as any).__parses);
        expect(parses, `${parses} renders for 300 tokens`).toBeLessThan(30);
    } finally {
        await ctx.close();
    }
});

// A rename goes to the conversation that was renamed, even if another is
// opened before the debounce fires.
test("a rename lands on the conversation being renamed", async ({ stack, browser }) => {
    const user = await createTestUser();
    const a = await seedConversation(stack, user, "Alpha", [{ role: "user", content: "a" }, { role: "assistant", content: "A." }]);
    const b = await seedConversation(stack, user, "Beta", [{ role: "user", content: "b" }, { role: "assistant", content: "B." }]);
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const shell = await openConversation(page, a.id);
        await expect(shell.locator(".chat-messages")).toContainText("A.", { timeout: 10_000 });
        await shell.locator(".chat-conv-title").fill("Trip plan");
        await openConversation(page, b.id); // within the 500ms debounce
        await expect(page.locator(".chat-shell:visible .chat-messages")).toContainText("B.", { timeout: 10_000 });
        const title = async (id: string) => (await (await fetch(`${stack.workspaceURL}/console/api/conversations/${id}`, { headers: authed(user) })).json()).conversation.title;
        await expect.poll(() => title(a.id), { timeout: 5_000 }).toBe("Trip plan");
        expect(await title(b.id)).toBe("Beta");
    } finally {
        await ctx.close();
    }
});

// Images in a reply fit the column; a wide one pushed the panel sideways.
test("a wide image in a reply doesn't widen the chat", async ({ stack, browser }) => {
    const user = await createTestUser();
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="2400" height="200"><rect width="2400" height="200" fill="teal"/></svg>`;
    const src = "data:image/svg+xml;base64," + Buffer.from(svg).toString("base64");
    const conv = await seedConversation(stack, user, "Image", [
        { role: "user", content: "show it" },
        { role: "assistant", content: `![wide](${src})` },
    ]);
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const shell = await openConversation(page, conv.id);
        const img = shell.locator(".chat-msg-body img");
        await expect(img).toBeVisible({ timeout: 10_000 });
        await expect.poll(() => img.evaluate((el: HTMLImageElement) => el.complete)).toBe(true);
        const m = shell.locator(".chat-messages");
        expect(await m.evaluate((el) => el.scrollWidth - el.clientWidth), "the panel scrolls sideways").toBeLessThanOrEqual(1);
    } finally {
        await ctx.close();
    }
});

// Screen readers: the transcript is a log, a finished reply is announced,
// the title and menu are labelled, Escape closes the menu, and the research
// card's Stop is a reachable button (the card was role=button, which hid it).
test("the chat is usable with a screen reader and the keyboard", async ({ stack, browser }) => {
    const user = await createTestUser();
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        await page.route("**/console/api/research/runs/active**", (route) => route.fulfill({
            status: 200, contentType: "application/json",
            body: JSON.stringify({ run: { id: "run-1", status: "researching", topic: "optane", workers: [] } }),
        }));
        const shell = await openConversation(page);
        await expect(shell.locator(".chat-messages")).toHaveAttribute("role", "log");
        await expect(shell.locator(".chat-conv-title")).toHaveAttribute("aria-label", "Conversation title");

        const more = shell.locator(".notes-overflow-btn");
        await expect(more).toHaveAttribute("aria-expanded", "false");
        await more.click();
        await expect(more).toHaveAttribute("aria-expanded", "true");
        await page.keyboard.press("Escape");
        await expect(more).toHaveAttribute("aria-expanded", "false");

        await fulfillChat(page, reply("The answer is 42."));
        await shell.locator(".chat-input").fill("question");
        await shell.locator(".chat-send-btn").click();
        await expect(shell.locator('[role="status"]')).toContainText("The answer is 42.", { timeout: 10_000 });

        const card = shell.locator(".chat-research-card");
        await expect(card).toBeVisible({ timeout: 15_000 });
        expect(await card.getAttribute("role")).toBeNull();
        await expect(card.getByRole("button", { name: "Stop research" })).toBeVisible();
        await expect(card.getByRole("button", { name: "Open" })).toBeVisible();
    } finally {
        await ctx.close();
    }
});

// Safari commits an IME candidate with an Enter that has keyCode 229 and
// isComposing false; it sent the half-typed message.
test("Enter that commits an IME candidate doesn't send", async ({ stack, browser }) => {
    const user = await createTestUser();
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const shell = await openConversation(page);
        let sent = 0;
        await page.route("**/api/chat", (route) => { sent++; return route.fulfill({ status: 200, contentType: "text/event-stream", body: reply("x") }); });
        await shell.locator(".chat-input").fill("にほん");
        await shell.locator(".chat-input").evaluate((el) => {
            el.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", keyCode: 229, bubbles: true, cancelable: true }));
        });
        await page.waitForTimeout(500);
        expect(sent).toBe(0);
        await expect(shell.locator(".chat-input")).toHaveValue("にほん");
    } finally {
        await ctx.close();
    }
});

// No in-panel conversation rail is built (it was hidden, and its "+ New"
// passed the click event as a shard id).
test("the hidden conversation rail is gone", async ({ stack, browser }) => {
    const user = await createTestUser();
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const shell = await openConversation(page);
        await expect(shell.locator(".chat-left, .chat-new-btn, .chat-conv-list")).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});

// Closing a chat tab stops its polling: a recovery poll kept running (and
// kept the tab's DOM alive) after the tab was gone.
test("closing a chat tab stops its recovery poll", async ({ stack, browser }) => {
    const user = await createTestUser();
    const conv = await seedConversation(stack, user, "Closing", [{ role: "user", content: "long job" }]);
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        let polls = 0;
        await page.route("**/api/chat/status**", (route) => { polls++; return route.fulfill({ status: 200, contentType: "application/json", body: '{"running":true}' }); });
        const shell = await openConversation(page, conv.id);
        await expect(shell.locator(".chat-interrupted-note")).toContainText("still being written", { timeout: 10_000 });
        await expect.poll(() => polls, { timeout: 10_000 }).toBeGreaterThan(1);
        await page.locator(".ws-tab", { hasText: "Closing" }).locator(".ws-tab-close").click();
        await page.waitForTimeout(1_000);
        const after = polls;
        await page.waitForTimeout(8_000);
        expect(polls - after, "the closed tab kept polling").toBe(0);
    } finally {
        await ctx.close();
    }
});

// The vendored highlight.js core was a CommonJS build that threw
// "module is not defined" on every load; it isn't loaded any more.
test("opening a chat with code raises no script errors", async ({ stack, browser }) => {
    const user = await createTestUser();
    const conv = await seedConversation(stack, user, "Code", [
        { role: "user", content: "code?" },
        { role: "assistant", content: "```go\nfunc main() {}\n```" },
    ]);
    const { ctx, page } = await openPage(stack, user, browser);
    try {
        const errors: string[] = [];
        page.on("pageerror", (e) => errors.push(e.message));
        const shell = await openConversation(page, conv.id);
        await expect(shell.locator(".chat-msg-body pre code.hljs")).toContainText("func main", { timeout: 10_000 });
        expect(errors.filter((e) => /module is not defined/.test(e))).toEqual([]);
    } finally {
        await ctx.close();
    }
});
