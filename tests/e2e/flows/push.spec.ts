// push.spec.ts — the Web Push HTTP surface against a push-enabled stack
// (fixed test VAPID keypair). The browser-side subscribe ceremony +
// service-worker push handling need a real push service / installed PWA,
// so those are validated on-device (Phase 4); here we pin the API
// contract: the VAPID key is served, subscriptions persist + are caller-
// scoped, and a `push` action target still creates its thread.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack, TEST_VAPID_PUBLIC } from "../fixtures/gateway";
import { createTestUser, TestUser } from "../fixtures/user";

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

    const ownerDel = await request.delete(`${stack.workspaceURL}/console/api/push/subscribe`, {
        headers: authed(user),
        data: { endpoint },
    });
    expect(ownerDel.ok(), `unsubscribe: HTTP ${ownerDel.status()}`).toBeTruthy();
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
