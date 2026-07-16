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
