// security.spec.ts — black-box checks for the audit invariants
// (TESTING-PLAN.md §"Phase 2" item 4). These run as HTTP tests
// through the workspace proxy — no browser rendering — because each
// invariant is a wire-level promise:
//
//   1. Passkey registration is first-run-only: once any credential
//      exists, register/begin demands a session (the old
//      email-spoofing takeover is closed).
//   2. Identity headers (X-User-Email et al.) are stripped by the
//      proxy and never authenticate anything.
//   3. A shard bearer token is an invoke credential, not a console
//      session — /console/api refuses it.
//   4. Disabling a user kills their LIVE sessions at the next
//      request, not just future logins.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, seedCredential, setUserStatus } from "../fixtures/user";

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

test.describe.configure({ mode: "serial" });

test("passkey registration requires a session once any credential exists", async ({ stack, request }) => {
    // Plant a credential row directly — the auth spec covers the real
    // ceremony; here only the COUNT matters to the gate.
    const owner = await createTestUser();
    await seedCredential(owner.id);

    const status = await (
        await request.get(`${stack.workspaceURL}/console/api/auth/register/status`)
    ).json();
    expect(status.credentials_registered).toBeGreaterThan(0);

    // Anonymous register/begin must be refused — and must NOT honor an
    // attacker-supplied email (the closed takeover vector).
    const anon = await request.post(`${stack.workspaceURL}/console/api/auth/register/begin`, {
        headers: { "Content-Type": "application/json" },
        data: { email: owner.email, display_name: "attacker" },
    });
    expect([401, 403]).toContain(anon.status());

    // With a real session the same call succeeds — the gate is on
    // identity, not a blanket lockout.
    const authed = await request.post(`${stack.workspaceURL}/console/api/auth/register/begin`, {
        headers: { Cookie: owner.cookieHeader, "Content-Type": "application/json" },
        data: {},
    });
    expect(authed.ok(), `authed register/begin: HTTP ${authed.status()}`).toBeTruthy();
});

test("identity headers never authenticate through the proxy", async ({ stack, request }) => {
    const victim = await createTestUser();
    const spoofHeaders = {
        "X-User-Email": victim.email,
        "X-User-Id": victim.id,
        "X-Familiar-User": victim.id,
    };

    // auth/status stays unauthenticated.
    const status = await (
        await request.get(`${stack.workspaceURL}/console/api/auth/status`, { headers: spoofHeaders })
    ).json();
    expect(status.authenticated).toBeFalsy();

    // A protected resource refuses outright.
    const notes = await request.get(`${stack.workspaceURL}/console/api/books/personal/pages`, {
        headers: spoofHeaders,
    });
    expect(notes.status()).toBe(401);
});

test("a shard bearer token is not a console session", async ({ stack, request }) => {
    const owner = await createTestUser();
    const id = `e2e-sec-${Date.now().toString(36)}`;
    const headers = { Cookie: owner.cookieHeader, "Content-Type": "application/json" };
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/shards`, {
                headers,
                data: {
                    id,
                    name: "sec shard",
                    persistence: "persistent",
                    visibility: "isolated",
                    scope_tag: `shard:${id}`,
                    system_prompt: "x",
                    tool_allowlist: [],
                },
            })
        ).ok(),
    ).toBeTruthy();
    const mint = await (
        await request.post(`${stack.workspaceURL}/console/api/shards/${id}/tokens`, {
            headers,
            data: { label: "sec" },
        })
    ).json();
    expect(mint.plaintext).toMatch(/^shard_/);

    // The owner's own shard token must not open the owner's console.
    for (const url of [
        `${stack.workspaceURL}/console/api/shards`,
        `${stack.workspaceURL}/console/api/books/personal/pages`,
        `${stack.workspaceURL}/console/api/auth/passkeys`,
    ]) {
        const resp = await request.get(url, {
            headers: { Authorization: `Bearer ${mint.plaintext}` },
        });
        expect([401, 403], `${url} accepted a shard bearer token (HTTP ${resp.status()})`).toContain(
            resp.status(),
        );
    }
});

test("disabling a user kills their live session on the next request", async ({ stack, request }) => {
    const user = await createTestUser();
    const url = `${stack.workspaceURL}/console/api/books/personal/pages`;
    const headers = { Cookie: user.cookieHeader };

    // Session works…
    expect((await request.get(url, { headers })).ok()).toBeTruthy();

    // …status flips to disabled…
    await setUserStatus(user.id, "disabled");

    // …and the SAME session token is now refused. The authz
    // middleware re-checks user status per request; a disabled user
    // doesn't get to coast on a pre-existing cookie.
    const after = await request.get(url, { headers });
    expect([401, 403], `live session survived disable (HTTP ${after.status()})`).toContain(after.status());

    // Re-approval restores access without a new login — proving the
    // gate really was the status column, not session deletion.
    await setUserStatus(user.id, "approved");
    expect((await request.get(url, { headers })).ok()).toBeTruthy();
});

// A state change carrying the session cookie from another origin is
// refused, on the console API and on /api/chat. There was no check:
// SameSite=Lax still sends the cookie from a sibling subdomain.
test("state changes from another origin are refused, even with the session cookie", async ({ stack, request }) => {
    const user = await createTestUser();
    const from = (origin: string) => ({ Cookie: user.cookieHeader, "Content-Type": "application/json", Origin: origin });
    const evil = from("https://evil.example");
    expect((await request.post(`${stack.workspaceURL}/console/api/books`, { headers: evil, data: { name: "csrf" } })).status()).toBe(403);
    expect((await request.post(`${stack.workspaceURL}/api/chat`, { headers: evil, data: { message: "hi" } })).status()).toBe(403);
    // The app's own origin is fine.
    const ok = await request.post(`${stack.workspaceURL}/console/api/books`, {
        headers: from(stack.workspaceURL),
        data: { name: `csrf ok ${Date.now().toString(36)}` },
    });
    expect(ok.ok(), `own origin: HTTP ${ok.status()}`).toBeTruthy();
});
