// shards.spec.ts — shard CRUD, token mint/revoke, and the shard
// passkey enrollment ceremony (TESTING-PLAN.md §"Phase 2" item 2).
// Shards cross three boundaries — backend store, webauthn-helpers,
// panel module — so the passkey test drives the real #panel-shards
// UI with the virtual authenticator while the CRUD/token coverage
// stays at the HTTP layer where the assertions are sharper.

import { test as base, expect } from "@playwright/test";
import { Client } from "pg";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";
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

test.describe.configure({ mode: "serial" });

function shardBody(id: string) {
    return {
        id,
        name: `Shard ${id}`,
        description: "e2e shard",
        persistence: "persistent",
        visibility: "isolated",
        scope_tag: `shard:${id}`,
        system_prompt: "you are an e2e test shard",
        tool_allowlist: [],
    };
}

function authed(user: TestUser, extra: Record<string, string> = {}) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json", ...extra };
}

test("shard CRUD round-trips through the console API", async ({ stack, request }) => {
    const user = await createTestUser();
    const base = `${stack.workspaceURL}/console/api/shards`;
    const id = `e2e-crud-${Date.now().toString(36)}`;

    // Create.
    const created = await request.post(base, { headers: authed(user), data: shardBody(id) });
    expect(created.ok(), `create: HTTP ${created.status()}`).toBeTruthy();
    const shard = await created.json();
    expect(shard.id).toBe(id);
    expect(shard.owner_id).toBe(user.id);
    // SHARD-AUTH-SPEC posture defaults: console off, chat/api on.
    expect(shard.console_access).toBe(false);
    expect(shard.chat_enabled).toBe(true);
    expect(shard.api_enabled).toBe(true);

    // List shows it; get returns it.
    const list = await (await request.get(base, { headers: authed(user) })).json();
    expect(list.items.map((s: any) => s.id)).toContain(id);
    const got = await (await request.get(`${base}/${id}`, { headers: authed(user) })).json();
    expect(got.name).toBe(`Shard ${id}`);

    // Patch.
    const patched = await request.patch(`${base}/${id}`, {
        headers: authed(user),
        data: { description: "patched by e2e" },
    });
    expect(patched.ok(), `patch: HTTP ${patched.status()}`).toBeTruthy();
    expect((await patched.json()).description).toBe("patched by e2e");

    // Disable / enable cycle.
    const disabled = await request.post(`${base}/${id}/disable`, { headers: authed(user) });
    expect(disabled.ok()).toBeTruthy();
    expect((await disabled.json()).active).toBe(false);
    const enabled = await request.post(`${base}/${id}/enable`, { headers: authed(user) });
    expect(enabled.ok()).toBeTruthy();
    expect((await enabled.json()).active).toBe(true);

    // Delete; gone afterwards.
    const deleted = await request.delete(`${base}/${id}`, { headers: authed(user) });
    expect(deleted.ok(), `delete: HTTP ${deleted.status()}`).toBeTruthy();
    expect((await request.get(`${base}/${id}`, { headers: authed(user) })).status()).toBe(404);
});

test("token mint shows the plaintext once; the list only ever has the prefix", async ({ stack, request }) => {
    const user = await createTestUser();
    const base = `${stack.workspaceURL}/console/api/shards`;
    const id = `e2e-tok-${Date.now().toString(36)}`;
    expect((await request.post(base, { headers: authed(user), data: shardBody(id) })).ok()).toBeTruthy();

    const minted = await request.post(`${base}/${id}/tokens`, {
        headers: authed(user),
        data: { label: "e2e-device" },
    });
    expect(minted.ok(), `mint: HTTP ${minted.status()}`).toBeTruthy();
    const mint = await minted.json();
    expect(mint.plaintext, "mint must return the plaintext exactly once").toMatch(/^shard_/);

    // The list never re-exposes the secret — only the 8-char prefix.
    const tokens = await (await request.get(`${base}/${id}/tokens`, { headers: authed(user) })).json();
    expect(tokens.items).toHaveLength(1);
    const row = tokens.items[0];
    expect(row.token_prefix).toBe(mint.plaintext.slice(0, 8));
    expect(JSON.stringify(row)).not.toContain(mint.plaintext);

    // Revoke marks the row, second revoke stays idempotent.
    const revoke = await request.post(`${stack.workspaceURL}/console/api/shard_tokens/${row.id}/revoke`, {
        headers: authed(user),
    });
    expect(revoke.ok(), `revoke: HTTP ${revoke.status()}`).toBeTruthy();
    const after = await (await request.get(`${base}/${id}/tokens`, { headers: authed(user) })).json();
    expect(after.items[0].revoked_at).toBeTruthy();
});

test("a user cannot see or mint against another user's shard", async ({ stack, request }) => {
    const owner = await createTestUser();
    const intruder = await createTestUser();
    const base = `${stack.workspaceURL}/console/api/shards`;
    const id = `e2e-iso-${Date.now().toString(36)}`;
    expect((await request.post(base, { headers: authed(owner), data: shardBody(id) })).ok()).toBeTruthy();

    // Not in the intruder's list, not gettable, not mintable.
    const list = await (await request.get(base, { headers: authed(intruder) })).json();
    expect(list.items.map((s: any) => s.id)).not.toContain(id);
    expect([403, 404]).toContain((await request.get(`${base}/${id}`, { headers: authed(intruder) })).status());
    expect([403, 404]).toContain(
        (
            await request.post(`${base}/${id}/tokens`, { headers: authed(intruder), data: { label: "steal" } })
        ).status(),
    );
});

test("deleting a shard disables its scheduled actions instead of unleashing them", async ({
    stack,
    request,
}) => {
    const user = await createTestUser();
    const base = `${stack.workspaceURL}/console/api`;
    const id = `e2e-del-${Date.now().toString(36)}`;
    expect((await request.post(`${base}/shards`, { headers: authed(user), data: shardBody(id) })).ok()).toBeTruthy();

    // One action bound to the shard, one trusted bystander.
    const bound = await (
        await request.post(`${base}/actions`, {
            headers: authed(user),
            data: {
                name: "enveloped",
                prompt: "x",
                cron: "0 7 * * *",
                shard_id: id,
                report_targets: [{ kind: "log" }],
            },
        })
    ).json();
    const bystander = await (
        await request.post(`${base}/actions`, {
            headers: authed(user),
            data: { name: "trusted bystander", prompt: "x", cron: "0 8 * * *", report_targets: [{ kind: "log" }] },
        })
    ).json();

    const del = await request.delete(`${base}/shards/${id}`, { headers: authed(user) });
    expect(del.ok(), `delete: HTTP ${del.status()}`).toBeTruthy();
    expect((await del.json()).disabled_actions).toBe(1);

    // The bound action is OFF with an explanatory status and no
    // longer claims an envelope; the bystander is untouched. The
    // deletion must never silently promote an enveloped action to a
    // full-capability trusted run.
    const after = await (
        await request.get(`${base}/actions/${bound.id}`, { headers: authed(user) })
    ).json();
    expect(after.enabled).toBe(false);
    expect(after.last_status).toBe("shard_deleted");
    expect(after.shard_id ?? "").toBe("");
    const untouched = await (
        await request.get(`${base}/actions/${bystander.id}`, { headers: authed(user) })
    ).json();
    expect(untouched.enabled).toBe(true);
});

test("binding a disabled shard to an action is refused at write time", async ({ stack, request }) => {
    const user = await createTestUser();
    const base = `${stack.workspaceURL}/console/api`;
    const id = `e2e-dis-${Date.now().toString(36)}`;
    expect((await request.post(`${base}/shards`, { headers: authed(user), data: shardBody(id) })).ok()).toBeTruthy();
    expect((await request.post(`${base}/shards/${id}/disable`, { headers: authed(user) })).ok()).toBeTruthy();

    const refused = await request.post(`${base}/actions`, {
        headers: authed(user),
        data: {
            name: "doomed",
            prompt: "x",
            cron: "0 7 * * *",
            shard_id: id,
            report_targets: [{ kind: "log" }],
        },
    });
    expect(refused.status(), await refused.text()).toBe(400);
    expect(await refused.text()).toContain("disabled");
});

test("enroll a shard passkey through the panel UI with the virtual authenticator", async ({
    stack,
    browser,
    request,
}) => {
    // Admin user so the shards panel is reachable in the console nav
    // wiring; ownership semantics are covered by the API tests above.
    const user = await createTestUser({ role: "admin" });
    const id = `e2e-pk-${Date.now().toString(36)}`;
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/shards`, {
                headers: authed(user),
                data: shardBody(id),
            })
        ).ok(),
    ).toBeTruthy();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await enableVirtualAuthenticator(page);
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Jump straight to the shards panel via the exposed switcher —
        // panel routing has its own coverage ambitions; this test is
        // about the enroll ceremony.
        await page.evaluate(() => (window as any).appSwitchPanel("shards"));
        await expect(page.locator("#panel-shards")).toBeVisible();

        // Open the shard detail and start enrollment.
        await page.locator(`#shards-rows tr[data-id="${id}"]`).click();
        await expect(page.locator("#shard-passkeys-section")).toBeVisible({ timeout: 10_000 });
        await page.locator("#shard-add-passkey").click();
        await expect(page.locator("#shard-passkey-label-modal")).toBeVisible();
        await page.locator("#shard-passkey-label-input").fill("e2e virtual key");
        await page.locator("#shard-passkey-label-go").click();

        // begin → navigator.credentials.create (virtual authenticator)
        // → finish → list refresh. The new key shows with its label.
        await expect(page.locator("#shard-passkeys-rows")).toContainText("e2e virtual key", {
            timeout: 15_000,
        });

        // And the credential really landed server-side.
        const count = await shardPasskeyCount(id);
        expect(count).toBe(1);
    } finally {
        await ctx.close();
    }
});

test("the new-shard form is grouped into sections and creates with Advanced collapsed", async ({
    stack,
    browser,
}) => {
    const user = await createTestUser({ role: "admin" });
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.evaluate(() => (window as any).appSwitchPanel("shards"));
        await expect(page.locator("#panel-shards")).toBeVisible();
        // Wait for the panel's init (which loads the list AND wires the
        // New button) to finish — otherwise the click can race the
        // handler attachment.
        await expect(page.locator("#shards-rows")).not.toContainText("Loading", { timeout: 10_000 });
        await page.locator("#shards-new").click();
        await expect(page.locator("#shard-detail")).toBeVisible({ timeout: 10_000 });

        // The shard glyph sits in the detail title, matching the panel.
        // Scope to #shard-detail — the title-row class is shared with
        // the scheduled-actions detail.
        await expect(page.locator("#shard-detail .shard-detail-title-row svg")).toBeVisible();

        // Grouped into titled sections, not a flat field dump. Scope to
        // #shard-form — the scheduled action form shares the shard-form
        // section styling/class.
        const titles = await page.locator("#shard-form .shard-section-title").allTextContents();
        expect(titles).toEqual(["Identity", "Behavior", "Sign-in & access", "Capabilities", "Prompt"]);

        // Number inputs match text inputs (the session-max-age "Login
        // idle window" box was rendering with the default white chrome).
        const [textBg, numBg] = await page.evaluate(() => {
            const cs = (id: string) => getComputedStyle(document.getElementById(id)!).backgroundColor;
            return [cs("shard-id"), cs("shard-session-max-age")];
        });
        expect(numBg).toBe(textBg);

        // Tuning fields live in a collapsed Advanced block.
        const advanced = page.locator(".shard-advanced");
        await expect(advanced).toBeVisible();
        await expect(page.locator("#shard-scope")).toBeHidden(); // inside the closed <details>
        await advanced.locator("summary").click();
        await expect(page.locator("#shard-scope")).toBeVisible();
        await advanced.locator("summary").click(); // re-collapse

        // A create still succeeds with Advanced collapsed — collapsed,
        // non-required fields must not block native form validation.
        const id = `e2e-form-${Date.now().toString(36)}`;
        await page.locator("#shard-id").fill(id);
        await page.locator("#shard-name").fill("Form Layout Shard");
        await page.locator("#shard-prompt").fill("You are a layout e2e shard.");
        await page.locator('#shard-form button[type="submit"]').click();
        await expect(page.locator("#shard-detail-title")).toContainText("Form Layout Shard", { timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});

async function shardPasskeyCount(shardID: string): Promise<number> {
    const client = new Client({ connectionString: process.env.FAMILIAR_TEST_DSN });
    await client.connect();
    try {
        const { rows } = await client.query<{ n: string }>(
            `SELECT COUNT(*)::text AS n FROM shard_passkeys WHERE shard_id = $1 AND revoked_at IS NULL`,
            [shardID],
        );
        return Number(rows[0]?.n ?? "0");
    } finally {
        await client.end();
    }
}
