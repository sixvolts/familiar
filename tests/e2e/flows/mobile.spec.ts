// mobile.spec.ts — the mobile SPA (mobile.html / mobile.js), which had
// ZERO E2E coverage despite being what most beta testers will use. Runs
// ONLY under the "mobile" Playwright project (Pixel 7 descriptor: phone
// viewport + touch + mobile UA), so the workspace's UA sniffing serves
// mobile.html at "/". Focus: the boot/auth gate and — the P0 fix — that
// a session lapsing mid-use lands the user on login instead of a wall
// of failing requests (mobile had no 401 handling and no watchdog).

import { test as base, expect } from "@playwright/test";
import { Client } from "pg";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, setUserStatus, seedCredential } from "../fixtures/user";
import { enableVirtualAuthenticator } from "../fixtures/authenticator";

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

test("the phone UA gets the mobile SPA and an unauthenticated boot lands on an auth screen", async ({
    stack,
    page,
}) => {
    await page.goto(stack.workspaceURL);
    // mobile.html-specific chrome proves the UA routing served the
    // mobile SPA, not the desktop index.
    await expect(page.locator("#mob-auth-loading")).toHaveCount(1);
    // Boot settles off the spinner…
    await expect(page.locator("#mob-auth-loading")).toBeHidden({ timeout: 15_000 });
    // …onto an auth overlay (login or first-run setup), never the app.
    await expect(page.locator("#mob-app")).toBeHidden();
    const authVisible = page.locator("#mob-auth-login:visible, #mob-auth-setup:visible");
    await expect(authVisible.first()).toBeVisible();
});

test("an authenticated boot shows the app shell", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);

    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    await expect(page.locator("#mob-auth-login")).toBeHidden();
    // The bottom tab bar is the mobile shell's signature chrome.
    await expect(page.locator("#mob-tabbar")).toBeVisible();
});

test("a session that lapses mid-use routes back to login (watchdog)", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });

    // The session goes away (expired / revoked) — every auth/status
    // probe now 401s. Before the fix, mobile had no watchdog and would
    // sit on a dead app dumping "HTTP 401" strings.
    await page.route("**/console/api/auth/status", (route) =>
        route.fulfill({ status: 401, contentType: "application/json", body: '{"error":"unauthorized"}' }),
    );
    // Returning to the app fires the watchdog probe.
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));

    await expect(page.locator("#mob-auth-login")).toBeVisible({ timeout: 15_000 });
});

test("MOBILE: create user → enroll a passkey → log in", async ({ stack, page, request }) => {
    // Full onboarding chain on the phone: a new approved user with no
    // passkey self-issues an enrollment token, registers via the
    // enrollment link, then logs into the mobile SPA with that passkey.
    const user = await createTestUser();
    const tokenResp = await request.post(`${stack.workspaceURL}/console/api/auth/enrollment-token`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { target_rp_id: "localhost" },
    });
    expect(tokenResp.ok(), `enrollment-token: HTTP ${tokenResp.status()}`).toBeTruthy();
    const { token } = await tokenResp.json();
    expect(token, "token issued").toBeTruthy();

    await enableVirtualAuthenticator(page);

    // 1. Register the passkey via the enrollment link (enroll.html is
    //    served at /enroll regardless of UA).
    await page.goto(`${stack.workspaceURL}/enroll?token=${encodeURIComponent(token)}`);
    await expect(page.locator("#register-btn")).toBeVisible({ timeout: 10_000 });
    await page.locator("#register-btn").click();
    await expect(page.locator("#status.is-ok")).toContainText(/passkey registered/i, { timeout: 15_000 });

    // 2. Log into the mobile SPA with that passkey (the phone UA gets
    //    mobile.html at "/"; the authenticator + resident credential
    //    persist across the navigation).
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-auth-login")).toBeVisible({ timeout: 15_000 });
    await page.locator("#mob-login-btn").click();
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    await expect(page.locator("#mob-auth-login")).toBeHidden();

    // 3. The right user is signed in.
    const status = await (await page.request.get(`${stack.workspaceURL}/console/api/auth/status`)).json();
    expect(status.authenticated).toBe(true);
    expect(status.user).toBe(user.id);
});

test("MOBILE: the Account screen shows a gated Notifications toggle", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });

    await page.evaluate(() => { location.hash = "account"; });
    await expect(page.locator('.mob-screen[data-screen="account"]')).toHaveClass(/is-active/, { timeout: 10_000 });

    // The Notifications section renders and its state resolves (not stuck
    // on "Checking…"). Headless isn't an installed standalone PWA and the
    // stack has no VAPID configured, so the toggle is gated off — what
    // matters is Push.render ran without error and surfaced a reason.
    await expect(page.locator(".mob-screen.is-active", { hasText: "Notifications" })).toBeVisible();
    const meta = page.locator("#mob-push-meta");
    await expect(meta).not.toHaveText("Checking…", { timeout: 10_000 });
    await expect(meta).not.toHaveText("");
    await expect(page.locator("#mob-push-toggle")).toBeHidden();
});

test("MOBILE: a pending account gets friendly copy, not a raw error", async ({ stack, page, request }) => {
    // Parity with desktop: a pending mobile user must see a sentence, not
    // the raw "account pending approval" string. Enroll a passkey, flip
    // the account to pending, then attempt login.
    const user = await createTestUser();
    const tokenResp = await request.post(`${stack.workspaceURL}/console/api/auth/enrollment-token`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { target_rp_id: "localhost" },
    });
    expect(tokenResp.ok(), `enrollment-token: HTTP ${tokenResp.status()}`).toBeTruthy();
    const { token } = await tokenResp.json();

    await enableVirtualAuthenticator(page);
    await page.goto(`${stack.workspaceURL}/enroll?token=${encodeURIComponent(token)}`);
    await page.locator("#register-btn").click();
    await expect(page.locator("#status.is-ok")).toContainText(/passkey registered/i, { timeout: 15_000 });

    // Gate the account, then try to sign in on the mobile SPA.
    await setUserStatus(user.id, "pending");
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-auth-login")).toBeVisible({ timeout: 15_000 });
    await page.locator("#mob-login-btn").click();

    const err = page.locator("#mob-login-error");
    await expect(err).toBeVisible({ timeout: 15_000 });
    await expect(err).toContainText(/awaiting admin approval/i);
    await expect(err).not.toContainText(/account pending approval/i); // the raw string
    await expect(page.locator("#mob-app")).toBeHidden();
});


test("a stickied wiki page opens even when a different book is active", async ({
    stack,
    page,
    context,
    request,
}) => {
    // Regression: tapping a Home-pinned wiki page intermittently showed
    // "Page not found" + a blank editable page. The pin routed to
    // #wiki/<pageId> (id only); openPage guessed the book by scanning the
    // ACTIVE book's page list, so whenever a different book was active the id
    // was never found. Fix routes #wiki/<bookSlug>/<pageId> and selects the
    // book before resolving. This forces the exact failing condition: page
    // pinned in book A, book B active, then tap the pin.
    const user = await createTestUser({ role: "admin" });
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const suffix = Date.now().toString(36);

    const bookA = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: `Alpha ${suffix}` },
        })
    ).json();
    const bookB = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers,
            data: { name: `Bravo ${suffix}` },
        })
    ).json();

    const pageTitle = `Groceries ${suffix}`;
    const pageA = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${bookA.slug}/pages`, {
            headers,
            data: { title: pageTitle, content: "eggs, milk, bread" },
        })
    ).json();
    // Book B gets a page too so its own page list is non-empty and unrelated.
    await request.post(`${stack.workspaceURL}/console/api/books/${bookB.slug}/pages`, {
        headers,
        data: { title: `Other ${suffix}`, content: "unrelated" },
    });
    // Pin page A so it appears in Home's Pinned section.
    await request.post(
        `${stack.workspaceURL}/console/api/books/${bookA.slug}/page-by-id/${pageA.id}/pin`,
        { headers, data: { pinned: true } },
    );

    await attachSession(context, stack.workspaceURL, user);
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });

    // The bug needs a book OTHER than A (the pinned page's book) active when
    // the pin is tapped. Open the wiki tab, and if A is the active card, switch
    // to B. Books order by updated_at DESC so which one defaults active is not
    // fixed — assert the precondition rather than assume it.
    await page.locator('.mob-tab[data-tab="wiki"]').click();
    const bookACard = page.locator(`.mob-book-card[data-book-slug="${bookA.slug}"]`);
    const bookBCard = page.locator(`.mob-book-card[data-book-slug="${bookB.slug}"]`);
    await expect(bookACard).toBeVisible({ timeout: 10_000 });
    // A click on an already-active card is a no-op, so only click B if A is the
    // one currently active, then poll until A is definitively not active.
    const aActive = await bookACard.evaluate((el) => el.classList.contains("is-active"));
    if (aActive) {
        await bookBCard.click();
    }
    await expect
        .poll(() => bookACard.evaluate((el) => el.classList.contains("is-active")))
        .toBe(false);

    // Back to Home, tap the pinned page (which lives in book A, not B).
    await page.locator('.mob-tab[data-tab="home"]').click();
    const pin = page.locator("#mob-home-pins .mob-row", { hasText: pageTitle });
    await expect(pin).toBeVisible({ timeout: 10_000 });
    await pin.click();

    // It must open A's real page, not the "Page not found" blank.
    const titleInput = page.locator("#mob-wiki-page-title");
    await expect(titleInput).toHaveValue(pageTitle, { timeout: 10_000 });
    await expect(titleInput).not.toHaveAttribute("placeholder", "Page not found");
    await expect
        .poll(() => page.evaluate(() => location.hash))
        .toContain(`wiki/${bookA.slug}/`);
});

// Every mobile thread must send its own conversation id with each chat
// turn. Without it the gateway put every mobile thread in one implicit
// per-user session, so a thread saw another thread's turns, and a
// kiosk session (which must chat inside a shard-bound conversation)
// couldn't chat at all.
test("MOBILE: a chat turn carries its thread's conversation id", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    let chatBody: { message?: string; conversation_id?: string } | null = null;
    await page.route("**/api/chat", async (route) => {
        chatBody = route.request().postDataJSON();
        await route.fulfill({
            status: 200,
            contentType: "text/event-stream",
            body:
                'event: session\ndata: {"session_id":"s"}\n\n' +
                'event: token\ndata: {"content":"ok"}\n\n' +
                'event: done\ndata: {"content":"ok"}\n\n',
        });
    });

    await page.goto(`${stack.workspaceURL}/#chat/new`);
    await expect(page.locator("#mob-thread-input")).toBeVisible({ timeout: 15_000 });
    await page.locator("#mob-thread-input").fill("hello from the phone");
    await page.locator("#mob-thread-form").evaluate((f) => (f as HTMLFormElement).requestSubmit());

    await expect.poll(() => chatBody, { timeout: 10_000 }).not.toBeNull();
    const convID = chatBody!.conversation_id;
    expect(convID, JSON.stringify(chatBody)).toMatch(/^[0-9a-f-]{36}$/);
    // It's the conversation this thread just created, not some other one.
    await expect(page).toHaveURL(new RegExp(`#chat/${convID}$`));
});

// A stream that ends in an error saves nothing. Mobile used to store the
// partial (or an empty reply), which its own recovery then treated as the
// finished answer.
test("MOBILE: a failed stream doesn't save a partial reply", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
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
                'event: session\ndata: {"session_id":"s"}\n\n' +
                'event: token\ndata: {"content":"half an ans"}\n\n' +
                'event: error\ndata: {"message":"model exploded"}\n\n',
        }),
    );
    await page.goto(`${stack.workspaceURL}/#chat/new`);
    await expect(page.locator("#mob-thread-input")).toBeVisible({ timeout: 15_000 });
    await page.locator("#mob-thread-input").fill("tell me something");
    await page.locator("#mob-thread-form").evaluate((f) => (f as HTMLFormElement).requestSubmit());
    await expect(page.getByText("model exploded")).toBeVisible({ timeout: 10_000 });
    // Give any trailing persistence a moment to fire.
    await page.waitForTimeout(500);
    expect(assistantPosts, "a failed stream's partial was saved").toEqual([]);
});

async function mobileNote(stack: GatewayStack, request: any, user: any, content: string) {
    const r = await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { title: "phone note", content },
    });
    return r.json();
}

async function mobileNoteContent(stack: GatewayStack, request: any, user: any, id: string): Promise<string> {
    const r = await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${id}`, {
        headers: { Cookie: user.cookieHeader },
    });
    return (await r.json()).content;
}

// Edits to different paragraphs on the phone and elsewhere both survive,
// and the phone shows the merged note (it used to keep only its own text,
// so its next save would have overwritten the other edit).
test("MOBILE: a disjoint concurrent edit merges into the phone's note", async ({ stack, page, context, request }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    const note = await mobileNote(stack, request, user, "line one\n\nline two\n\nline three");
    await page.goto(`${stack.workspaceURL}/#notes/${note.id}`);
    const editor = page.locator("#mob-note-body .toastui-editor-ww-container .ProseMirror").first();
    await expect(editor).toContainText("line three", { timeout: 15_000 });
    const current = await (
        await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers: { Cookie: user.cookieHeader },
        })
    ).json();

    await editor.click();
    await page.keyboard.press("ControlOrMeta+End");
    await page.keyboard.type(" from the phone");
    const remote = await request.patch(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json", "If-Match": current.updated_at },
        data: { content: current.content.replace("line one", "line one from the desk") },
    });
    expect(remote.ok()).toBeTruthy();

    await expect
        .poll(() => mobileNoteContent(stack, request, user, note.id), { timeout: 10_000 })
        .toContain("line three from the phone");
    expect(await mobileNoteContent(stack, request, user, note.id)).toContain("line one from the desk");
    await expect(editor).toContainText("line one from the desk", { timeout: 10_000 });
});

// An overlapping edit stops saving and offers a choice. It used to say
// "conflict — reload to continue" with no way to reload: reopening the
// same note short-circuited, so every later edit was silently dropped.
test("MOBILE: an overlapping edit offers use-theirs / keep-mine", async ({ stack, page, context, request }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    const note = await mobileNote(stack, request, user, "shared line");
    await page.goto(`${stack.workspaceURL}/#notes/${note.id}`);
    const editor = page.locator("#mob-note-body .toastui-editor-ww-container .ProseMirror").first();
    await expect(editor).toContainText("shared line", { timeout: 15_000 });
    const current = await (
        await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers: { Cookie: user.cookieHeader },
        })
    ).json();

    await editor.click();
    await page.keyboard.press("ControlOrMeta+a");
    await page.keyboard.type("the phone's version");
    const remote = await request.patch(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json", "If-Match": current.updated_at },
        data: { content: "the desk's version" },
    });
    expect(remote.ok()).toBeTruthy();

    const bar = page.locator(".mob-edit-conflict");
    await expect(bar).toBeVisible({ timeout: 10_000 });
    await expect(editor).toContainText("the phone's version");
    await bar.getByRole("button", { name: "Keep mine" }).click();
    await expect
        .poll(() => mobileNoteContent(stack, request, user, note.id), { timeout: 10_000 })
        .toContain("the phone's version");
    await expect(bar).toBeHidden();
});

// Pull-to-refresh refreshes the service worker instead of unregistering
// it: unregistering also ends the worker's push subscription, so each
// pull silently turned notifications off.
test("MOBILE: pull-to-refresh keeps the service worker (and its push subscription)", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.addInitScript(() => {
        const reg: any = {
            scope: "/",
            update: async () => { sessionStorage.setItem("sw", (sessionStorage.getItem("sw") || "") + "update;"); },
            unregister: async () => { sessionStorage.setItem("sw", (sessionStorage.getItem("sw") || "") + "unregister;"); return true; },
            pushManager: { getSubscription: async () => null },
        };
        Object.defineProperty(navigator, "serviceWorker", {
            configurable: true,
            value: { getRegistration: async () => reg, getRegistrations: async () => [reg], register: async () => reg, ready: Promise.resolve(reg) },
        });
    });
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    const header = page.locator(".mob-home-header:visible, .mob-title-header:visible").first();
    await expect(header).toBeVisible();

    // Drag down 150px from the header, past the 70px threshold.
    await header.evaluate((el) => {
        const r = el.getBoundingClientRect();
        const at = (y: number) => [new Touch({ identifier: 1, target: el, clientX: r.left + 10, clientY: y })];
        const y0 = r.top + 5;
        el.dispatchEvent(new TouchEvent("touchstart", { touches: at(y0), bubbles: true, cancelable: true }));
        el.dispatchEvent(new TouchEvent("touchmove", { touches: at(y0 + 150), bubbles: true, cancelable: true }));
        el.dispatchEvent(new TouchEvent("touchend", { touches: [], bubbles: true, cancelable: true }));
    });
    // The refresh reloads the page (?_r=…); sessionStorage survives it.
    await page.waitForURL(/_r=/, { timeout: 10_000 });
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    const calls = await page.evaluate(() => sessionStorage.getItem("sw") || "");
    expect(calls).toContain("update;");
    expect(calls).not.toContain("unregister");
});

// ── Batch 11b: the mobile app ──────────────────────────────────────

async function mobileChat(page: any, stack: GatewayStack, sse: string, delayMs = 0) {
    const seen = { chat: 0, stop: 0 };
    await page.route("**/api/chat", async (route: any) => {
        seen.chat++;
        if (delayMs) await new Promise((r) => setTimeout(r, delayMs));
        await route.fulfill({ status: 200, contentType: "text/event-stream", body: sse }).catch(() => {});
    });
    await page.route("**/api/chat/stop", async (route: any) => {
        seen.stop++;
        await route.fulfill({ status: 200, json: { stopped: true } });
    });
    await page.goto(`${stack.workspaceURL}/#chat/new`);
    await expect(page.locator("#mob-thread-input")).toBeVisible({ timeout: 15_000 });
    return seen;
}

// The final content in the done event is authoritative. What streamed
// can hold reasoning the gateway split out afterwards; mobile showed
// the raw stream.
test("MOBILE: the reply shows the final content, not leaked reasoning", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await mobileChat(page, stack,
        'event: session\ndata: {"session_id":"s","persists_reply":true}\n\n' +
        'event: token\ndata: {"chunk":"Let me think about this LEAKED-THOUGHT. "}\n\n' +
        'event: token\ndata: {"chunk":"The answer is 42."}\n\n' +
        'event: done\ndata: {"content":"The answer is 42.","reasoning_content":"Let me think about this LEAKED-THOUGHT."}\n\n');
    await page.locator("#mob-thread-input").fill("what is it?");
    await page.locator("#mob-thread-form").evaluate((f: HTMLFormElement) => f.requestSubmit());
    const bubble = page.locator(".mob-msg.is-assistant .bubble").last();
    await expect(bubble).toHaveText("The answer is 42.", { timeout: 10_000 });
});

// Return while a reply streams does nothing (it used to stop the reply
// and drop the follow-up); only a tap on Stop stops it. Return sends,
// Shift+Return is a newline (the composer was an <input>, which strips
// pasted newlines).
test("MOBILE: Return sends, keeps newlines, and never stops a reply", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    const sent: string[] = [];
    page.on("request", (req: any) => {
        if (req.method() === "POST" && /\/conversations\/[^/]+\/messages$/.test(req.url())) {
            sent.push((req.postDataJSON() || {}).content);
        }
    });
    const seen = await mobileChat(page, stack,
        'event: session\ndata: {"session_id":"s","persists_reply":true}\n\n' +
        'event: done\ndata: {"content":"ok"}\n\n', 3000);
    const input = page.locator("#mob-thread-input");
    await input.click();
    await page.keyboard.type("line one");
    await page.keyboard.press("Shift+Enter");
    await page.keyboard.type("line two");
    await page.keyboard.press("Enter");
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]).toBe("line one\nline two");

    // Streaming (the reply is held for 3s): Return must not stop it.
    await expect(page.locator(".mob-thread-send")).toHaveClass(/is-stop/);
    await input.click();
    await page.keyboard.type("a follow-up");
    await page.keyboard.press("Enter");
    await page.waitForTimeout(300);
    await expect(page.locator(".mob-thread-send"), "Return stopped the reply").toHaveClass(/is-stop/);
    expect(seen.stop).toBe(0);
    await expect(input).toHaveValue("a follow-up");
    // A tap on Stop does stop it (before the held reply arrives).
    await page.locator(".mob-thread-send").click();
    await expect(page.locator(".mob-thread-send")).not.toHaveClass(/is-stop/, { timeout: 1_000 });
});

// A failed save of the user's message stops the turn, as on desktop.
// Streaming anyway saved an answer with no question above it.
test("MOBILE: a message that can't be saved isn't sent", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    const seen = await mobileChat(page, stack, 'event: done\ndata: {"content":"ok"}\n\n');
    await page.route(/\/conversations\/[^/]+\/messages$/, (route: any) =>
        route.request().method() === "POST" ? route.fulfill({ status: 500, json: { error: "db down" } }) : route.continue());
    await page.locator("#mob-thread-input").fill("please remember this");
    await page.locator("#mob-thread-form").evaluate((f: HTMLFormElement) => f.requestSubmit());
    await expect(page.locator(".mob-msg.is-assistant .bubble").last()).toContainText("Couldn’t save your message", { timeout: 10_000 });
    expect(seen.chat, "the turn streamed without its question saved").toBe(0);
    await expect(page.locator("#mob-thread-input")).toHaveValue("please remember this");
});

// A pinned chat opened directly (not from the 50-row list) shows Unpin
// and unpins. The pin state came from the list cache, so it read Pin.
test("MOBILE: a deep-linked pinned chat can be unpinned", async ({ stack, page, context, request }) => {
    const user = await createTestUser();
    const h = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const conv = await (await request.post(`${stack.workspaceURL}/console/api/conversations`, { headers: h, data: { title: "Pinned one", model: "familiar" } })).json();
    await request.patch(`${stack.workspaceURL}/console/api/conversations/${conv.id}`, { headers: h, data: { pinned: true } });
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(`${stack.workspaceURL}/#chat/${conv.id}`);
    await expect(page.locator("#mob-thread-title")).toHaveText("Pinned one", { timeout: 15_000 });
    await page.locator('.mob-screen[data-screen="chat-thread"] [data-action="toggle-overflow"]').click();
    const item = page.locator("#mob-toggle-pin-chat");
    await expect(item).toHaveText("Unpin chat");
    await item.click();
    await expect.poll(async () => (await (await request.get(`${stack.workspaceURL}/console/api/conversations/${conv.id}`, { headers: h })).json()).conversation.pinned).toBe(false);
});

// The research poll stops when the thread is left; it ran every 5s for
// the rest of the session.
test("MOBILE: the research poll stops when you leave the thread", async ({ stack, page, context, request }) => {
    const user = await createTestUser();
    const h = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const conv = await (await request.post(`${stack.workspaceURL}/console/api/conversations`, { headers: h, data: { title: "Research", model: "familiar" } })).json();
    await attachSession(context, stack.workspaceURL, user);
    let polls = 0;
    page.on("request", (req: any) => { if (req.url().includes("/research/runs")) polls++; });
    await page.clock.install();
    await page.goto(`${stack.workspaceURL}/#chat/${conv.id}`);
    await expect(page.locator("#mob-thread-title")).toHaveText("Research", { timeout: 15_000 });
    await expect.poll(() => polls).toBeGreaterThan(0);
    await page.evaluate(() => { location.hash = "notes"; });
    await expect(page.locator('.mob-screen[data-screen="notes"]')).toHaveClass(/is-active/);
    const atLeave = polls;
    await page.clock.runFor(20_000);
    expect(polls, "polled after leaving the thread").toBe(atLeave);
});

// Leaving a note you only read saves nothing. Back (and unload) saved
// unconditionally: Toast UI's re-serialized markdown went out as an edit
// and was broadcast, 409'ing the device actually editing the note.
test("MOBILE: backing out of an unedited note saves nothing", async ({ stack, page, context, request }) => {
    const user = await createTestUser();
    const note = await mobileNote(stack, request, user, "* a list item\n\n| a | b |\n|---|---|\n| 1 | 2 |");
    await attachSession(context, stack.workspaceURL, user);
    let patches = 0;
    page.on("request", (req: any) => { if (req.method() === "PATCH" && req.url().includes(`/page-by-id/${note.id}`)) patches++; });
    await page.goto(`${stack.workspaceURL}/#notes/${note.id}`);
    await expect(page.locator("#mob-note-body .ProseMirror").first()).toContainText("a list item", { timeout: 15_000 });
    await page.locator('[data-action="back-to-notes-list"]').click();
    await expect(page.locator('.mob-screen[data-screen="notes"]')).toHaveClass(/is-active/);
    await page.waitForTimeout(800);
    expect(patches).toBe(0);
});

// An edit is saved when the app goes to the background, not after a
// debounce a frozen page never runs (iOS fires no unload on an app
// switch, and may evict the app).
test("MOBILE: an edit is saved when the app is backgrounded", async ({ stack, page, context, request }) => {
    const user = await createTestUser();
    const note = await mobileNote(stack, request, user, "start");
    await attachSession(context, stack.workspaceURL, user);
    let patches = 0;
    page.on("request", (req: any) => { if (req.method() === "PATCH" && req.url().includes(`/page-by-id/${note.id}`)) patches++; });
    await page.clock.install();
    await page.goto(`${stack.workspaceURL}/#notes/${note.id}`);
    const editor = page.locator("#mob-note-body .toastui-editor-ww-container .ProseMirror").first();
    await expect(editor).toContainText("start", { timeout: 15_000 });
    await page.clock.pauseAt(Date.now() + 60_000); // the 500ms debounce can't fire
    await editor.click();
    await page.keyboard.press("End");
    await page.keyboard.type(" plus more");
    await page.evaluate(() => {
        Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
        document.dispatchEvent(new Event("visibilitychange"));
    });
    await expect.poll(() => patches, { timeout: 5_000 }).toBe(1);
});

// Someone else signing in on the session-expired overlay gets a fresh
// app; the previous user's open note and thread stayed on screen.
test("MOBILE: signing in as someone else starts clean", async ({ stack, page, context }) => {
    const alice = await createTestUser();
    const bob = await createTestUser();
    await seedCredential(alice.id);
    await attachSession(context, stack.workspaceURL, alice);
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    await page.evaluate(() => { (window as any).__aliceApp = true; });
    const client = new Client({ connectionString: process.env.FAMILIAR_TEST_DSN });
    await client.connect();
    try {
        await client.query(`UPDATE admin_sessions SET expires_at = NOW() - interval '1 minute' WHERE token = $1`, [alice.sessionToken]);
    } finally {
        await client.end();
    }
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await expect(page.locator("#mob-auth-login")).toBeVisible({ timeout: 10_000 });
    await attachSession(context, stack.workspaceURL, bob);
    await page.route("**/console/api/auth/login/begin", (r: any) => r.fulfill({ json: { publicKey: { challenge: "AAAA" } } }));
    await page.route("**/console/api/auth/login/finish", (r: any) => r.fulfill({ json: { status: "authenticated" } }));
    await page.evaluate(() => {
        const buf = () => new Uint8Array([1]).buffer;
        (navigator.credentials as any).get = async () => ({
            id: "x", rawId: buf(), type: "public-key",
            response: { authenticatorData: buf(), clientDataJSON: buf(), signature: buf(), userHandle: null },
            getClientExtensionResults: () => ({}),
        });
    });
    await page.locator("#mob-login-btn").click();
    await expect.poll(() => page.evaluate(() => (window as any).FAMILIAR_SESSION?.user), { timeout: 15_000 }).toBe(bob.id);
    expect(await page.evaluate(() => (window as any).__aliceApp), "Bob got Alice's app, not a fresh one").toBeUndefined();
});

// A failed books fetch says so, with Retry; it said "No books yet —
// create one", and people made duplicates.
test("MOBILE: a failed books load offers Retry, not 'create one'", async ({ stack, page, context }) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    let fail = true;
    await page.route("**/console/api/books", (r: any) =>
        fail && r.request().method() === "GET" ? r.fulfill({ status: 502, json: { error: "gateway restarting" } }) : r.continue());
    await page.goto(`${stack.workspaceURL}/#wiki`);
    const list = page.locator("#mob-wiki-pages");
    await expect(list).toContainText("Couldn't load books", { timeout: 15_000 });
    await expect(list).not.toContainText("No books yet");
    fail = false;
    await page.locator("#mob-wiki-retry").click();
    await expect(list).not.toContainText("Couldn't load books", { timeout: 10_000 });
});

// Wiki links work on the phone: a note's [[Book/Page]] opens that page
// (it routed the slug as an id: "Page not found"), and a page in a book
// joined after the list loaded opens too (a stale list broke it).
test("MOBILE: wiki links and new books' pages open", async ({ stack, page, context, request }) => {
    const owner = await createTestUser();
    const user = await createTestUser();
    const ho = { Cookie: owner.cookieHeader, "Content-Type": "application/json" };
    const suffix = Date.now().toString(36);
    const book = await (await request.post(`${stack.workspaceURL}/console/api/books`, { headers: ho, data: { name: `recipes-${suffix}` } })).json();
    const lasagna = await (await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, { headers: ho, data: { title: "Lasagna", content: "layers" } })).json();
    const note = await mobileNote(stack, request, user, `see [[${book.slug}/Lasagna]]`);
    // A book of their own, so the cached list isn't empty (an empty one
    // was always refreshed; a stale non-empty one never was).
    await request.post(`${stack.workspaceURL}/console/api/books`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { name: `mine-${suffix}` },
    });
    await attachSession(context, stack.workspaceURL, user);

    // Visit Wiki first so the book list is cached without this book…
    await page.goto(`${stack.workspaceURL}/#wiki`);
    await expect(page.locator('.mob-screen[data-screen="wiki"]')).toHaveClass(/is-active/, { timeout: 15_000 });
    await page.waitForTimeout(500);
    // …then join it.
    const add = await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/members`, { headers: ho, data: { user_id: user.id, role: "reader" } });
    expect(add.ok()).toBeTruthy();

    await page.evaluate((id) => { location.hash = "notes/" + id; }, note.id);
    const link = page.locator("#mob-note-body .toastui-editor-ww-container a.wiki-link").first();
    await expect(link).toBeVisible({ timeout: 15_000 });
    await link.click();
    await expect(page).toHaveURL(new RegExp(`#wiki/${book.slug}/${lasagna.id}$`), { timeout: 10_000 });
    await expect(page.locator("#mob-wiki-page-title")).toHaveValue("Lasagna", { timeout: 10_000 });
});

// Styling and affordances: inputs are 16px (iOS zooms into anything
// smaller), editor links are a link color and underlined (the color
// token was undefined on mobile), dead search buttons are hidden, touch
// targets reach 44pt, and overflow menus say whether they're open.
test("MOBILE: inputs, links, search, touch targets and menus", async ({ stack, page, context, request }) => {
    const user = await createTestUser();
    const note = await mobileNote(stack, request, user, "a [link](https://example.com) here");
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(`${stack.workspaceURL}/#scheduled/new`);
    const name = page.locator(".mob-sa-input").first();
    await expect(name).toBeVisible({ timeout: 15_000 });
    expect(await name.evaluate((el) => parseFloat(getComputedStyle(el).fontSize))).toBeGreaterThanOrEqual(16);

    await page.evaluate((id) => { location.hash = "notes/" + id; }, note.id);
    const a = page.locator("#mob-note-body .toastui-editor-ww-container .toastui-editor-contents a").first();
    await expect(a).toBeVisible({ timeout: 15_000 });
    const style = await a.evaluate((el) => {
        const cs = getComputedStyle(el);
        // Against the text it sits in: an undefined color variable makes
        // the link inherit exactly that.
        const around = getComputedStyle(el.parentElement!);
        return { color: cs.color, text: around.color, underline: cs.textDecorationLine };
    });
    expect(style.color).not.toBe(style.text);
    expect(style.underline).toContain("underline");

    await page.evaluate(() => { location.hash = "home"; });
    await expect(page.locator(".mob-search-pill")).toBeHidden();

    const more = page.locator('.mob-screen.is-active [data-action="toggle-overflow"]').first();
    await expect(more).toHaveAttribute("aria-expanded", "false");
    const box = (await more.boundingBox())!;
    // 5px outside the drawn button still hits it.
    const hit = await page.evaluate(([x, y]) => {
        const el = document.elementFromPoint(x, y);
        return !!(el && el.closest('[data-action="toggle-overflow"]'));
    }, [box.x - 5, box.y + box.height / 2]);
    expect(hit, "no hit area outside the 34px button").toBe(true);
    await more.click();
    await expect(more).toHaveAttribute("aria-expanded", "true");
    await expect(page.locator('.mob-overflow.is-open [role="menuitem"]').first()).toBeVisible();
});
