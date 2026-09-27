// mobile.spec.ts — the mobile SPA (mobile.html / mobile.js), which had
// ZERO E2E coverage despite being what most beta testers will use. Runs
// ONLY under the "mobile" Playwright project (Pixel 7 descriptor: phone
// viewport + touch + mobile UA), so the workspace's UA sniffing serves
// mobile.html at "/". Focus: the boot/auth gate and — the P0 fix — that
// a session lapsing mid-use lands the user on login instead of a wall
// of failing requests (mobile had no 401 handling and no watchdog).

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, setUserStatus } from "../fixtures/user";
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
