// resilience.spec.ts — how the workspace behaves when the network or
// model misbehaves. Deterministic (no live model): faults are injected
// with Playwright request interception (helpers/faults.ts) so we can
// simulate a 5xx model, a dropped connection, and an in-band SSE error
// frame, plus a save that won't land. Beta testers WILL hit flaky wifi
// and a busy/down local model — these pin that the UI degrades visibly
// instead of hanging or silently eating work.

import { test as base, expect, Page } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";
import { failPath, status500Path, sseErrorPath } from "../helpers/faults";

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

async function openChat(page: Page, stack: GatewayStack) {
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await page.locator(".sidebar-cat-chat").click();
    const shell = page.locator(".chat-shell").first();
    await expect(shell).toBeVisible({ timeout: 10_000 });
    await page.evaluate(() => {
        window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat" } }));
    });
    const input = shell.locator(".chat-input");
    await expect(input).toBeVisible({ timeout: 10_000 });
    return shell;
}

test("a 5xx from the model surfaces a chat error and frees the composer", async ({ stack, browser }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const shell = await openChat(page, stack);
        // Only /api/chat fails — conversation-create + message-persist
        // (which the send path does first) still go through.
        await status500Path(page, "/api/chat");

        await shell.locator(".chat-input").fill("hello there");
        await shell.locator(".chat-send-btn").click();

        await expect(shell.locator(".chat-error")).toBeVisible({ timeout: 10_000 });
        // The composer must recover — a stuck "…" button is the worst
        // outcome (user thinks the app is wedged).
        const sendBtn = shell.locator(".chat-send-btn");
        await expect(sendBtn).toBeEnabled();
        await expect(sendBtn).toHaveText("Send");
    } finally {
        await ctx.close();
    }
});

test("an in-band SSE error frame is rendered, not swallowed", async ({ stack, browser }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const shell = await openChat(page, stack);
        await sseErrorPath(page, "/api/chat", "model exploded");

        await shell.locator(".chat-input").fill("trigger an error");
        await shell.locator(".chat-send-btn").click();

        await expect(shell.locator(".chat-error")).toContainText("model exploded", { timeout: 10_000 });
        await expect(shell.locator(".chat-send-btn")).toBeEnabled();
    } finally {
        await ctx.close();
    }
});

test("a dropped connection while sending doesn't wedge the composer", async ({ stack, browser }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const shell = await openChat(page, stack);
        await failPath(page, "/api/chat"); // connection refused / aborted

        await shell.locator(".chat-input").fill("are you there");
        await shell.locator(".chat-send-btn").click();

        await expect(shell.locator(".chat-error")).toBeVisible({ timeout: 10_000 });
        await expect(shell.locator(".chat-send-btn")).toBeEnabled();
    } finally {
        await ctx.close();
    }
});

test("a conversation whose reply was interrupted shows the recovery notice", async ({
    stack,
    browser,
    request,
}) => {
    // Simulate the mid-stream-reload data path: the send flow persists
    // the user prompt BEFORE streaming and the answer only AFTER, so a
    // reload mid-answer leaves a conversation whose last turn is an
    // unanswered prompt. Seed exactly that shape and assert the UI
    // explains it instead of looking silently broken.
    const user = await createTestUser();
    const conv = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: authed(user),
            data: { title: "Interrupted", model: "familiar" },
        })
    ).json();
    const msg = await request.post(
        `${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`,
        { headers: authed(user), data: { role: "user", content: "what is the meaning of life" } },
    );
    expect(msg.ok(), `seed message: HTTP ${msg.status()}`).toBeTruthy();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-chat").click();
        await expect(page.locator(".chat-shell").first()).toBeVisible({ timeout: 10_000 });
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat", id } }));
        }, conv.id);

        await expect(page.locator(".chat-shell .chat-messages")).toContainText(
            "what is the meaning of life",
            { timeout: 10_000 },
        );
        await expect(page.locator(".chat-interrupted-note")).toBeVisible();
    } finally {
        await ctx.close();
    }
});

test("a note save that won't land toasts instead of failing silently", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers: authed(user),
            data: { title: "Flaky", content: "original body" },
        })
    ).json();
    expect(note.id, JSON.stringify(note)).toBeTruthy();

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
        await expect(editor).toBeVisible({ timeout: 10_000 });

        // Now break saves (the initial GET already loaded the note).
        await failPath(page, `/console/api/books/personal/page-by-id/${note.id}`);

        // Dirty the editor → debounced autosave → PATCH aborts.
        await editor.click();
        await page.keyboard.type(" more text that cannot be saved");

        // The user gets a visible, non-silent signal (the toast — the
        // status dot alone was too easy to miss on flaky wifi).
        await expect(page.locator(".toast", { hasText: /couldn't save/i })).toBeVisible({ timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});

// A reply belongs to the conversation it was asked in. The client used
// to persist the finished reply into whichever conversation was open
// when the stream ended, so switching threads mid-answer filed the
// answer under the wrong conversation.
test("a reply lands in the conversation it was asked in, even after switching away", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const create = async (title: string) =>
        (
            await request.post(`${stack.workspaceURL}/console/api/conversations`, {
                headers: authed(user),
                data: { title, model: "familiar" },
            })
        ).json();
    const a = await create("Thread A");
    const b = await create("Thread B");
    const contents = async (id: string): Promise<string[]> => {
        const r = await request.get(`${stack.workspaceURL}/console/api/conversations/${id}`, { headers: authed(user) });
        return ((await r.json()).messages || []).map((m: { content: string }) => m.content);
    };

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const shell = await openChat(page, stack);
        // Hold the model's answer until the user has moved to thread B.
        let release!: () => void;
        const held = new Promise<void>((r) => (release = r));
        let asked = false;
        await page.route("**/api/chat", async (route) => {
            asked = true;
            await held;
            await route.fulfill({
                status: 200,
                contentType: "text/event-stream",
                body:
                    'event: session\ndata: {"session_id":"s"}\n\n' +
                    'event: token\ndata: {"content":"answer for A"}\n\n' +
                    'event: done\ndata: {"content":"answer for A"}\n\n',
            });
        });
        const open = (id: string) =>
            page.evaluate((cid) => {
                window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat", id: cid } }));
            }, id);

        await open(a.id);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Thread A", { timeout: 10_000 });
        await shell.locator(".chat-input").fill("question for A");
        await shell.locator(".chat-send-btn").click();
        await expect.poll(() => asked, { timeout: 10_000 }).toBe(true);

        await open(b.id);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Thread B", { timeout: 10_000 });
        release();

        await expect.poll(() => contents(a.id), { timeout: 10_000 }).toContain("answer for A");
        expect(await contents(b.id)).not.toContain("answer for A");
    } finally {
        await ctx.close();
    }
});

// When the gateway says it saves the reply (persists_reply), the client
// must not write it too: that duplicated it, and on Stop stored a second,
// slightly different partial.
test("the client leaves the reply to the gateway when the gateway saves it", async ({ stack, browser }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const shell = await openChat(page, stack);
        const assistantPosts: string[] = [];
        page.on("request", (req) => {
            if (req.method() === "POST" && /\/console\/api\/conversations\/[^/]+\/messages$/.test(req.url())) {
                const body = req.postDataJSON();
                if (body && body.role === "assistant") assistantPosts.push(body.content);
            }
        });
        await page.route("**/api/chat", (route) =>
            route.fulfill({
                status: 200,
                contentType: "text/event-stream",
                body:
                    'event: session\ndata: {"session_id":"s","persists_reply":true}\n\n' +
                    'event: token\ndata: {"content":"saved by the gateway"}\n\n' +
                    'event: done\ndata: {"content":"saved by the gateway"}\n\n',
            }),
        );
        await shell.locator(".chat-input").fill("hello");
        await shell.locator(".chat-send-btn").click();
        await expect(shell.locator(".chat-msg").last()).toContainText("saved by the gateway", { timeout: 10_000 });
        await expect(shell.locator(".chat-send-btn")).toBeEnabled();
        expect(assistantPosts, "client wrote a reply the gateway already saved").toEqual([]);
    } finally {
        await ctx.close();
    }
});

function openConversation(page: Page, id: string) {
    return page.evaluate((cid) => {
        window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat", id: cid } }));
    }, id);
}

// holdRequests holds every request route matches until release(), then
// lets it through. held() reports whether one arrived.
async function holdRequests(page: Page, url: string, method: string) {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let arrived = false;
    await page.route(url, async (route) => {
        if (route.request().method() !== method) return route.fallback();
        arrived = true;
        await gate;
        await route.continue();
    });
    return { release, held: () => arrived };
}

// A new chat whose create request is slow must not take the shell away
// from a conversation opened while it was pending.
test("a slow new chat doesn't take the shell from a conversation opened meanwhile", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const a = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: authed(user),
            data: { title: "Thread A", model: "familiar" },
        })
    ).json();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const create = await holdRequests(page, "**/console/api/conversations", "POST");
        const shell = await openChat(page, stack); // starts a new chat
        await expect.poll(create.held, { timeout: 10_000 }).toBe(true);

        await openConversation(page, a.id);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Thread A", { timeout: 10_000 });

        const created = page.waitForResponse(
            (r) => r.request().method() === "POST" && /\/console\/api\/conversations$/.test(r.url()),
        );
        create.release();
        await created;
        await page.waitForTimeout(500);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Thread A");
    } finally {
        await ctx.close();
    }
});

// Two opens in a row where the first answers last: the shell shows the
// second, not the first's messages under the second's id.
test("a slow conversation load doesn't paint over one opened after it", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const make = async (title: string, text: string) => {
        const c = await (
            await request.post(`${stack.workspaceURL}/console/api/conversations`, {
                headers: authed(user),
                data: { title, model: "familiar" },
            })
        ).json();
        await request.post(`${stack.workspaceURL}/console/api/conversations/${c.id}/messages`, {
            headers: authed(user),
            data: { role: "user", content: text },
        });
        return c;
    };
    const a = await make("Thread A", "message in A");
    const b = await make("Thread B", "message in B");
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        const shell = await openChat(page, stack);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Conversation", { timeout: 10_000 });

        const loadA = await holdRequests(page, `**/console/api/conversations/${a.id}`, "GET");
        await openConversation(page, a.id);
        await expect.poll(loadA.held, { timeout: 10_000 }).toBe(true);
        await openConversation(page, b.id);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Thread B", { timeout: 10_000 });
        await expect(shell.locator(".chat-messages")).toContainText("message in B");

        const loadedA = page.waitForResponse((r) => r.url().endsWith(`/console/api/conversations/${a.id}`));
        loadA.release();
        await loadedA;
        await page.waitForTimeout(500);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Thread B");
        await expect(shell.locator(".chat-messages")).not.toContainText("message in A");
    } finally {
        await ctx.close();
    }
});

// A background reload of the open conversation (here: its research run
// finishing) is not a switch: it must not cancel a New chat the user
// started while it ran.
test("a background refresh doesn't cancel a pending new chat", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const a = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: authed(user),
            data: { title: "Thread A", model: "familiar" },
        })
    ).json();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        let running = true;
        await page.route("**/console/api/research/runs/active*", (route) =>
            route.fulfill({
                status: 200,
                contentType: "application/json",
                body: JSON.stringify({ run: running ? { id: "run-1", status: "researching", topic: "t" } : null }),
            }),
        );
        const shell = await openChat(page, stack);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Conversation", { timeout: 10_000 });
        await openConversation(page, a.id);
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Thread A", { timeout: 10_000 });

        const create = await holdRequests(page, "**/console/api/conversations", "POST");
        await page.evaluate(() => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat" } }));
        });
        await expect.poll(create.held, { timeout: 10_000 }).toBe(true);

        // The run finishes: the next poll reloads Thread A in the background.
        const refetched = page.waitForRequest(
            (r) => r.method() === "GET" && r.url().endsWith(`/console/api/conversations/${a.id}`),
            { timeout: 15_000 },
        );
        running = false;
        await refetched;

        create.release();
        await expect(shell.locator(".chat-conv-title")).toHaveValue("Conversation", { timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});
