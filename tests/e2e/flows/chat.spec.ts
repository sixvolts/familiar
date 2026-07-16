// chat.spec.ts — the chat pipeline end to end against a REAL local
// model (TESTING-MAINFRAME.md: the llama-server on :8090 exists for
// exactly this). Everything between the textarea and the model is
// live: session cookie auth on /api/chat, the pipeline (memory
// retrieval degrades keyword-only — no embedder is configured),
// rule-based model selection (router disabled → first online
// role-less model), SSE streaming back through the workspace proxy,
// and the frontend's persistence of both turns.
//
// Skips (not fails) when no inference server is reachable, so CI —
// which has no GPU and no model — stays green while mainframe runs
// the real thing. Point FAMILIAR_TEST_CHAT_MODEL_URL elsewhere to
// use a different server.
//
// The prompt demands a fixed token ("PONG") so the assertion doesn't
// depend on model creativity. Generous timeouts: a 26B model on
// Vulkan answers a one-liner in seconds, but a cold first request
// can take longer.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

const MODEL_URL = process.env.FAMILIAR_TEST_CHAT_MODEL_URL || "http://127.0.0.1:8090";
const REPLY_TIMEOUT = 120_000;

async function modelIsUp(): Promise<boolean> {
    try {
        const resp = await fetch(`${MODEL_URL}/health`, { signal: AbortSignal.timeout(2_000) });
        return resp.ok;
    } catch {
        return false;
    }
}

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

test.describe.configure({ mode: "serial" });

test.beforeEach(async () => {
    test.skip(!(await modelIsUp()), `no inference server at ${MODEL_URL} — chat specs need a live model`);
});

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

test("a chat turn streams a real model reply over SSE", async ({ stack, request }) => {
    const user = await createTestUser();

    // Raw SSE through the workspace proxy. The request context
    // buffers until the stream closes (the `done` event ends it),
    // then the full frame log is assertable.
    const resp = await request.post(`${stack.workspaceURL}/api/chat`, {
        headers: { ...authed(user), Accept: "text/event-stream" },
        data: { message: "Reply with exactly: PONG" },
        timeout: REPLY_TIMEOUT,
    });
    expect(resp.ok(), `chat: HTTP ${resp.status()}`).toBeTruthy();
    expect(resp.headers()["content-type"]).toContain("text/event-stream");

    const body = await resp.text();
    expect(body).toContain("event: token");
    expect(body).toContain("event: done");
    expect(body).not.toContain("event: error");

    // Reassemble the streamed text from the token frames and check
    // the model actually said the word.
    const text = body
        .split("\n")
        .filter((l) => l.startsWith("data: "))
        .map((l) => {
            try {
                return JSON.parse(l.slice(6))?.chunk ?? "";
            } catch {
                return "";
            }
        })
        .join("");
    expect(text.toUpperCase()).toContain("PONG");

    // done carries the model attribution.
    const doneLine = body
        .split("\n\n")
        .find((frame) => frame.includes("event: done"))
        ?.split("\n")
        .find((l) => l.startsWith("data: "));
    expect(doneLine, "done event must carry a data payload").toBeTruthy();
    const done = JSON.parse(doneLine!.slice(6));
    expect(done.model_id).toBe("test/gemma");
});

test("the chat surface round-trips a conversation with the model", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Open the chat surface and start a fresh conversation.
        await page.locator(".sidebar-cat-chat").click();
        const shell = page.locator(".chat-shell").first();
        await expect(shell).toBeVisible({ timeout: 10_000 });
        await page.evaluate(() => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat" } }));
        });

        const input = shell.locator(".chat-input");
        await expect(input).toBeVisible({ timeout: 10_000 });
        await input.fill("Reply with exactly: PONG");
        await shell.locator(".chat-send-btn").click();

        // The user bubble appears immediately; the assistant bubble
        // fills as tokens stream in.
        await expect(shell.locator(".chat-messages")).toContainText("Reply with exactly: PONG");
        const assistant = shell.locator(".chat-msg-assistant").last();
        await expect(assistant).toContainText(/pong/i, { timeout: REPLY_TIMEOUT });

        // Both turns persisted — a reload must replay the exchange
        // from the DB, not from page state.
        const convs = await (
            await request.get(`${stack.workspaceURL}/console/api/conversations?limit=10`, {
                headers: authed(user),
            })
        ).json();
        expect(convs.items?.length, "conversation row must exist").toBeGreaterThanOrEqual(1);
        const convID = convs.items[0].id;
        await expect
            .poll(
                async () => {
                    const msgs = await (
                        await request.get(
                            `${stack.workspaceURL}/console/api/conversations/${convID}/messages`,
                            { headers: authed(user) },
                        )
                    ).json();
                    const items = msgs.items ?? msgs.messages ?? [];
                    return items.map((m: any) => m.role).sort();
                },
                { timeout: 15_000, message: "user + assistant messages must persist" },
            )
            .toEqual(["assistant", "user"]);

        await page.reload();
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat", id } }));
        }, convID);
        await expect(page.locator(".chat-shell .chat-messages").first()).toContainText(/pong/i, {
            timeout: 15_000,
        });
    } finally {
        await ctx.close();
    }
});

test("streaming doesn't yank the view down while the user reads back", async ({
    stack,
    browser,
    request,
}) => {
    // Regression: the chat used to slam scrollTop to the bottom on every
    // streamed token, so scrolling up to re-read mid-generation was
    // impossible — it snapped you back down. Now an upward scroll detaches
    // sticky autoscroll until you return to the bottom.
    test.setTimeout(180_000);
    const user = await createTestUser();

    // Seed a tall conversation up front so the panel already overflows —
    // the scroll behaviour we're testing must not depend on how much the
    // model happens to generate.
    const conv = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: authed(user),
            data: { title: "Scroll test", model: "familiar" },
        })
    ).json();
    for (let i = 0; i < 30; i++) {
        await request.post(`${stack.workspaceURL}/console/api/conversations/${conv.id}/messages`, {
            headers: authed(user),
            data: {
                role: i % 2 === 0 ? "user" : "assistant",
                content: `Seed line ${i} — filler text to give each bubble some height.`,
            },
        });
    }

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

        // The shell showing our seeded conversation (there may be more
        // than one chat shell in the layout).
        const shell = page.locator(".chat-shell", { hasText: "Seed line 29" });
        await expect(shell).toBeVisible({ timeout: 10_000 });
        const messages = shell.locator(".chat-messages");
        // Seeded history overflows the panel — deterministic, no model.
        await expect
            .poll(async () => messages.evaluate((el) => el.scrollHeight - el.clientHeight), {
                timeout: 15_000,
                message: "seeded history must overflow the panel so there's room to scroll up",
            })
            .toBeGreaterThan(200);

        const input = shell.locator(".chat-input");
        await expect(input).toBeVisible({ timeout: 10_000 });
        // A medium reply gives a streaming window to scroll up during.
        await input.fill("Count from 1 to 40 separated by commas, then stop.");
        await shell.locator(".chat-send-btn").click();
        await expect(shell.locator(".chat-msg-streaming")).toBeVisible({ timeout: REPLY_TIMEOUT });

        // Scroll up the way a user does: an upward wheel gesture (fires
        // the wheel handler → detaches sticky autoscroll synchronously),
        // then actually move the viewport to the top. A synthetic
        // WheelEvent doesn't perform the scroll itself, so set scrollTop
        // alongside it.
        await messages.evaluate((el) => {
            el.dispatchEvent(new WheelEvent("wheel", { deltaY: -6000, bubbles: true, cancelable: true }));
            el.scrollTop = 0;
        });
        expect(
            await messages.evaluate((el) => el.scrollTop),
            "the view moved off the bottom",
        ).toBeLessThan(80);

        // As more tokens stream in, the view must STAY up — never get
        // force-scrolled back to the bottom.
        for (let i = 0; i < 5; i++) {
            expect(
                await messages.evaluate((el) => el.scrollTop),
                "streaming must not force the view back down",
            ).toBeLessThan(140);
            await page.waitForTimeout(400);
        }

        // After the stream completes it's still where the user left it.
        await expect(shell.locator(".chat-msg-streaming")).toHaveCount(0, { timeout: REPLY_TIMEOUT });
        expect(
            await messages.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight),
            "view stays scrolled up after the stream ends, not pinned to the bottom",
        ).toBeGreaterThan(200);

        // Sending again re-engages following: the new turn scrolls down.
        await input.fill("Reply with exactly: DONE");
        await shell.locator(".chat-send-btn").click();
        await expect
            .poll(async () => messages.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight), {
                timeout: REPLY_TIMEOUT,
                message: "a fresh send re-sticks to the bottom",
            })
            .toBeLessThan(140);
    } finally {
        await ctx.close();
    }
});

// ── Shard chat (SKILL-PACKAGES-SPEC Phase 1) ──────────────────────

function envelopeShard(id: string) {
    return {
        id,
        name: `Envelope ${id}`,
        persistence: "persistent",
        visibility: "isolated",
        scope_tag: `shard:${id}`,
        // The marker word proves the shard's system prompt — not the
        // trusted path's — drove the reply.
        system_prompt: "You are EnvelopeBot. Always begin your reply with the word ENVELOPE.",
        tool_allowlist: [],
    };
}

test("a shard-bound conversation runs inside the shard envelope", async ({ stack, request }) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const shardID = `e2e-chatshard-${Date.now().toString(36)}`;
    expect(
        (await request.post(`${stack.workspaceURL}/console/api/shards`, { headers, data: envelopeShard(shardID) })).ok(),
    ).toBeTruthy();

    // Bind a conversation to the shard.
    const created = await request.post(`${stack.workspaceURL}/console/api/conversations`, {
        headers,
        data: { title: "", model: `shard:${shardID}` },
    });
    expect(created.status(), await created.text()).toBe(201);
    const conv = await created.json();
    expect(conv.model).toBe(`shard:${shardID}`);

    // The binding is immutable.
    const repoint = await request.patch(`${stack.workspaceURL}/console/api/conversations/${conv.id}`, {
        headers,
        data: { model: "familiar" },
    });
    expect(repoint.status(), "shard binding must be immutable").toBe(400);

    // Chat: the reply carries the shard's marker and the done frame
    // attributes the shard.
    const resp = await request.post(`${stack.workspaceURL}/api/chat`, {
        headers: { ...headers, Accept: "text/event-stream" },
        data: { message: "Say hello.", conversation_id: conv.id },
        timeout: REPLY_TIMEOUT,
    });
    expect(resp.ok(), `chat: HTTP ${resp.status()}`).toBeTruthy();
    const body = await resp.text();
    expect(body).toContain("event: done");
    expect(body).not.toContain("event: error");
    const text = body
        .split("\n")
        .filter((l) => l.startsWith("data: "))
        .map((l) => {
            try {
                return JSON.parse(l.slice(6))?.chunk ?? "";
            } catch {
                return "";
            }
        })
        .join("");
    expect(text.toUpperCase()).toContain("ENVELOPE");
    expect(body).toContain(`"shard_id":"${shardID}"`);

    // Disabling the shard refuses the NEXT message with a clear 409.
    expect(
        (await request.post(`${stack.workspaceURL}/console/api/shards/${shardID}/disable`, { headers })).ok(),
    ).toBeTruthy();
    const refused = await request.post(`${stack.workspaceURL}/api/chat`, {
        headers: { ...headers, Accept: "text/event-stream" },
        data: { message: "Still there?", conversation_id: conv.id },
    });
    expect(refused.status()).toBe(409);
    expect(await refused.text()).toContain("disabled");
});

test("a shard pins a wiki page for the user it acts for", async ({ stack, request }) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };

    // Seed a book + page owned by the user.
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: `Pin Probe ${Date.now().toString(36)}` },
        })
    ).json();
    const page = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers,
            data: { title: "Taco Soup", content: "soup" },
        })
    ).json();
    expect(page.slug, JSON.stringify(page)).toBeTruthy();

    // A shard allowed exactly one tool: pin_page.
    const shardID = `e2e-pinshard-${Date.now().toString(36)}`;
    const shard = envelopeShard(shardID);
    shard.tool_allowlist = ["pin_page"];
    shard.system_prompt =
        "You are PinBot. When asked to pin a page, call the pin_page tool with the exact book_slug and page_slug given, then confirm.";
    expect(
        (await request.post(`${stack.workspaceURL}/console/api/shards`, { headers, data: shard })).ok(),
    ).toBeTruthy();
    const conv = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers,
            data: { title: "", model: `shard:${shardID}` },
        })
    ).json();

    const resp = await request.post(`${stack.workspaceURL}/api/chat`, {
        headers: { ...headers, Accept: "text/event-stream" },
        data: {
            message: `Pin the wiki page with book_slug "${book.slug}" and page_slug "${page.slug}".`,
            conversation_id: conv.id,
        },
        timeout: REPLY_TIMEOUT,
    });
    expect(resp.ok(), `chat: HTTP ${resp.status()}`).toBeTruthy();
    const body = await resp.text();
    expect(body).not.toContain("event: error");

    // The pin landed on the USER's pin set — Home reflects it.
    const pins = await (
        await request.get(`${stack.workspaceURL}/console/api/home/pins`, { headers })
    ).json();
    const hit = (pins.items ?? []).find((p: any) => p.title === "Taco Soup");
    expect(hit, `home pins should contain the page; got ${JSON.stringify(pins)}`).toBeTruthy();
});

test("shard-conversation bindings are validated at creation", async ({ stack, request }) => {
    const owner = await createTestUser();
    const intruder = await createTestUser();
    const ownerHeaders = { Cookie: owner.cookieHeader, "Content-Type": "application/json" };
    const shardID = `e2e-bindval-${Date.now().toString(36)}`;
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/shards`, {
                headers: ownerHeaders,
                data: envelopeShard(shardID),
            })
        ).ok(),
    ).toBeTruthy();

    // Someone else's shard: refused, and indistinguishable from a
    // nonexistent one.
    const stolen = await request.post(`${stack.workspaceURL}/console/api/conversations`, {
        headers: { Cookie: intruder.cookieHeader, "Content-Type": "application/json" },
        data: { model: `shard:${shardID}` },
    });
    expect(stolen.status()).toBe(400);
    expect(await stolen.text()).toContain("not found");

    // chat_enabled=false: binding refused for the owner too.
    expect(
        (
            await request.patch(`${stack.workspaceURL}/console/api/shards/${shardID}`, {
                headers: ownerHeaders,
                data: { chat_enabled: false },
            })
        ).ok(),
    ).toBeTruthy();
    const muted = await request.post(`${stack.workspaceURL}/console/api/conversations`, {
        headers: ownerHeaders,
        data: { model: `shard:${shardID}` },
    });
    expect(muted.status()).toBe(400);
    expect(await muted.text()).toContain("chat is disabled");
});

test("the chat UI opens a shard chat from the sidebar with the chip visible", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const shardID = `e2e-uishard-${Date.now().toString(36)}`;
    expect(
        (await request.post(`${stack.workspaceURL}/console/api/shards`, { headers, data: envelopeShard(shardID) })).ok(),
    ).toBeTruthy();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Expand the Shards category and click the shard child.
        await page.locator(".sidebar-cat-shards .sidebar-row-chevron").click();
        const child = page.locator(".sidebar-child", { hasText: `Envelope ${shardID}` });
        await expect(child).toBeVisible({ timeout: 10_000 });
        await child.click();

        // A chat tab opens bound to the shard: chip visible, send a
        // message, the envelope's marker comes back.
        const shell = page.locator(".chat-shell").first();
        await expect(shell).toBeVisible({ timeout: 10_000 });
        await expect(shell.locator(".chat-shard-chip")).toBeVisible({ timeout: 10_000 });
        await expect(shell.locator(".chat-shard-chip")).toContainText(`Envelope ${shardID}`);

        const input = shell.locator(".chat-input");
        await expect(input).toBeVisible({ timeout: 10_000 });
        await input.fill("Say hello.");
        await shell.locator(".chat-send-btn").click();
        await expect(shell.locator(".chat-msg-assistant").last()).toContainText(/envelope/i, {
            timeout: REPLY_TIMEOUT,
        });
    } finally {
        await ctx.close();
    }
});
