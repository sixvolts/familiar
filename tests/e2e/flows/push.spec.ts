// push.spec.ts — the Web Push HTTP surface against a push-enabled stack
// (fixed test VAPID keypair). The browser-side subscribe ceremony +
// service-worker push handling need a real push service / installed PWA,
// so those are validated on-device (Phase 4); here we pin the API
// contract: the VAPID key is served, subscriptions persist + are caller-
// scoped, and a `push` action target still creates its thread.

import { test as base, expect, Page } from "@playwright/test";
import { Client } from "pg";
import { start, GatewayStack, TEST_VAPID_PUBLIC } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

const test = base.extend<{}, { stack: GatewayStack }>({
    stack: [
        async ({}, use) => {
            const stack = await start({ admin: true, push: true });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

// subscriptionOwner reads who holds an endpoint, straight from the table:
// unsubscribe answers 200 whether or not it deleted anything, so the
// response alone can't show whose device went.
async function subscriptionOwner(endpoint: string): Promise<string | null> {
    const client = new Client({ connectionString: process.env.FAMILIAR_TEST_DSN });
    await client.connect();
    try {
        const { rows } = await client.query<{ user_id: string }>(
            "SELECT user_id FROM push_subscriptions WHERE endpoint = $1",
            [endpoint],
        );
        return rows[0]?.user_id ?? null;
    } finally {
        await client.end();
    }
}

test("the VAPID public key is served when push is configured", async ({ stack, request }) => {
    const user = await createTestUser();
    const resp = await request.get(`${stack.workspaceURL}/console/api/push/key`, { headers: authed(user) });
    expect(resp.ok(), `HTTP ${resp.status()}`).toBeTruthy();
    const body = await resp.json();
    expect(body.public_key).toBe(TEST_VAPID_PUBLIC);
});

test("subscribe persists and unsubscribe removes a device, caller-scoped", async ({ stack, request }) => {
    const user = await createTestUser();
    const intruder = await createTestUser();
    const endpoint = `https://push.example.com/ep-${Date.now().toString(36)}`;
    const sub = { endpoint, keys: { p256dh: "BPtESTp256dhKEY", auth: "AUTHsecret" } };

    // Subscribe.
    const s = await request.post(`${stack.workspaceURL}/console/api/push/subscribe`, {
        headers: authed(user),
        data: sub,
    });
    expect(s.ok(), `subscribe: HTTP ${s.status()}`).toBeTruthy();
    expect(await subscriptionOwner(endpoint)).toBe(user.id);

    // A bad body is rejected.
    const bad = await request.post(`${stack.workspaceURL}/console/api/push/subscribe`, {
        headers: authed(user),
        data: { endpoint: "" },
    });
    expect(bad.status()).toBe(400);

    // Another user can't delete this device (caller-scoped delete is a
    // no-op for a non-owner; then the owner's delete succeeds).
    const intruderDel = await request.delete(`${stack.workspaceURL}/console/api/push/subscribe`, {
        headers: authed(intruder),
        data: { endpoint },
    });
    expect(intruderDel.ok()).toBeTruthy(); // no-op, not an error
    expect(await subscriptionOwner(endpoint), "another user's delete must leave the device").toBe(user.id);

    const ownerDel = await request.delete(`${stack.workspaceURL}/console/api/push/subscribe`, {
        headers: authed(user),
        data: { endpoint },
    });
    expect(ownerDel.ok(), `unsubscribe: HTTP ${ownerDel.status()}`).toBeTruthy();
    expect(await subscriptionOwner(endpoint), "the owner's delete removes it").toBeNull();
});

test("a push-target action creates its notification thread", async ({ stack, request }) => {
    const user = await createTestUser();
    const created = await (
        await request.post(`${stack.workspaceURL}/console/api/actions`, {
            headers: authed(user),
            data: {
                name: `push api ${Date.now().toString(36)}`,
                prompt: "notify me",
                cron: "0 7 * * *",
                report_targets: [{ kind: "push" }],
            },
        })
    ).json();
    const t = (created.report_targets || [])[0];
    expect(t.kind).toBe("push");
    expect(t.conversation_id, "push target auto-creates a thread").toMatch(/[0-9a-f-]{36}/);
});

// The phone side of push, on the mobile shell (?ui=mobile). Headless
// Chromium has no push service, so a stub stands in for the service
// worker registration and its subscription; everything between it and
// the database is the real app.
async function stubPushSubscription(page: Page, endpoint: string, keyB64: string) {
    await page.addInitScript(
        ({ endpoint, keyB64 }) => {
            const pad = "=".repeat((4 - (keyB64.length % 4)) % 4);
            const raw = atob((keyB64 + pad).replace(/-/g, "+").replace(/_/g, "/"));
            const key = Uint8Array.from(raw, (c) => c.charCodeAt(0)).buffer;
            let sub: any = {
                endpoint,
                options: { applicationServerKey: key },
                toJSON: () => ({ endpoint, keys: { p256dh: "BPtESTp256dhKEY", auth: "AUTHsecret" } }),
                unsubscribe: async () => {
                    sessionStorage.setItem("stub-unsubscribed", "1");
                    sub = null;
                    return true;
                },
            };
            if (sessionStorage.getItem("stub-unsubscribed")) sub = null;
            const reg: any = { scope: "/", update: async () => {}, pushManager: { getSubscription: async () => sub } };
            Object.defineProperty(navigator, "serviceWorker", {
                configurable: true,
                value: {
                    getRegistration: async () => reg,
                    getRegistrations: async () => [reg],
                    register: async () => reg,
                    ready: Promise.resolve(reg),
                },
            });
            // The installed-PWA check (notifications need a home-screen app).
            const mm = window.matchMedia.bind(window);
            window.matchMedia = ((q: string) =>
                q.includes("standalone") ? ({ matches: true, media: q, addEventListener() {}, removeEventListener() {} } as any) : mm(q)) as any;
        },
        { endpoint, keyB64 },
    );
}

async function openMobileAccount(page: Page, stack: GatewayStack) {
    await page.goto(`${stack.workspaceURL}/?ui=mobile`);
    await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    await page.evaluate(() => { location.hash = "account"; });
    await expect(page.locator('.mob-screen[data-screen="account"]')).toHaveClass(/is-active/, { timeout: 10_000 });
}

// Signing out on a phone unsubscribes it. It used to only end the
// session, so the next person to sign in on that phone got the previous
// user's notifications, previews included.
test("MOBILE: signing out unsubscribes the device from its user's notifications", async ({ stack, page, context, request }) => {
    const user = await createTestUser();
    const endpoint = `https://push.example.com/ep-out-${Date.now().toString(36)}`;
    const s = await request.post(`${stack.workspaceURL}/console/api/push/subscribe`, {
        headers: authed(user),
        data: { endpoint, keys: { p256dh: "BPtESTp256dhKEY", auth: "AUTHsecret" } },
    });
    expect(s.ok()).toBeTruthy();
    await stubPushSubscription(page, endpoint, TEST_VAPID_PUBLIC);
    await attachSession(context, stack.workspaceURL, user);
    await openMobileAccount(page, stack);

    await page.locator(".mob-account-signout").click();
    await expect(page.locator("#mob-auth-login")).toBeVisible({ timeout: 15_000 });
    expect(await subscriptionOwner(endpoint), "the device's subscription outlived sign-out").toBeNull();
    expect(await page.evaluate(() => sessionStorage.getItem("stub-unsubscribed"))).toBe("1");
});

// The Account screen re-registers the device's subscription for the
// signed-in user: after the server pruned it (or a restore lost it), the
// toggle used to read On from browser state while nothing arrived.
test("MOBILE: the Account screen re-registers a subscription the server lost", async ({ stack, page, context }) => {
    const user = await createTestUser();
    const endpoint = `https://push.example.com/ep-lost-${Date.now().toString(36)}`;
    await stubPushSubscription(page, endpoint, TEST_VAPID_PUBLIC);
    await attachSession(context, stack.workspaceURL, user);
    await openMobileAccount(page, stack);

    await expect(page.locator("#mob-push-meta")).toHaveText(/^On/, { timeout: 10_000 });
    await expect.poll(() => subscriptionOwner(endpoint)).toBe(user.id);
});

// A subscription made under a key the server no longer uses can never
// deliver. It is dropped and the toggle says Off, instead of On forever.
test("MOBILE: a subscription under an old server key is dropped, not shown as On", async ({ stack, page, context }) => {
    const user = await createTestUser();
    const endpoint = `https://push.example.com/ep-oldkey-${Date.now().toString(36)}`;
    const otherKey = "BAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
    await stubPushSubscription(page, endpoint, otherKey);
    await attachSession(context, stack.workspaceURL, user);
    await openMobileAccount(page, stack);

    await expect(page.locator("#mob-push-meta")).toHaveText("Off", { timeout: 10_000 });
    expect(await page.evaluate(() => sessionStorage.getItem("stub-unsubscribed"))).toBe("1");
    expect(await subscriptionOwner(endpoint)).toBeNull();
});
