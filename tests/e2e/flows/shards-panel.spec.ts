// shards-panel.spec.ts — the shard edit form keeps what it doesn't
// show: book confinement, the system prompt of a console-only shard,
// the temperature. Also: Cancel on the token label mints nothing, and
// shards open from the keyboard. Everything is set up through the
// console API; the form is driven through the real #panel-shards UI.

import { test as base, expect, Page, BrowserContext, APIRequestContext } from "@playwright/test";
import * as crypto from "node:crypto";
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

async function createShard(request: APIRequestContext, stack: GatewayStack, user: TestUser, extra: Record<string, unknown> = {}) {
    const id = `e2e-sp-${crypto.randomBytes(4).toString("hex")}`;
    const resp = await request.post(`${stack.workspaceURL}/console/api/shards`, {
        headers: authed(user),
        data: {
            id,
            name: `Panel ${id}`,
            description: "before",
            persistence: "persistent",
            visibility: "isolated",
            scope_tag: `shard:${id}`,
            system_prompt: "You answer questions about recipes, tersely.",
            tool_allowlist: [],
            ...extra,
        },
    });
    expect(resp.ok(), `create shard: HTTP ${resp.status()} ${await resp.text()}`).toBeTruthy();
    return id;
}

async function getShard(request: APIRequestContext, stack: GatewayStack, user: TestUser, id: string) {
    const resp = await request.get(`${stack.workspaceURL}/console/api/shards/${id}`, { headers: authed(user) });
    expect(resp.ok()).toBeTruthy();
    return resp.json();
}

async function createBook(request: APIRequestContext, stack: GatewayStack, user: TestUser, name: string, archive = false) {
    const resp = await request.post(`${stack.workspaceURL}/console/api/books`, { headers: authed(user), data: { name } });
    expect(resp.ok(), `create book: HTTP ${resp.status()}`).toBeTruthy();
    const b = await resp.json();
    if (archive) {
        const a = await request.patch(`${stack.workspaceURL}/console/api/books/${b.slug}`, {
            headers: authed(user),
            data: { archive: true },
        });
        expect(a.ok(), `archive book: HTTP ${a.status()}`).toBeTruthy();
    }
    return b as { id: string; slug: string };
}

async function openPanel(browser: any, stack: GatewayStack, user: TestUser): Promise<{ ctx: BrowserContext; page: Page }> {
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await page.evaluate(() => (window as any).appSwitchPanel("shards"));
    await expect(page.locator("#panel-shards")).toBeVisible();
    await expect(page.locator("#shards-rows")).not.toContainText("Loading", { timeout: 10_000 });
    return { ctx, page };
}

// The list is hidden while a shard is open; go back to it first.
async function backToList(page: Page) {
    if (await page.locator("#shard-detail-back").isVisible()) await page.locator("#shard-detail-back").click();
    await expect(page.locator("#shards-rows")).toBeVisible();
}

async function openShard(page: Page, id: string) {
    await backToList(page);
    await page.locator(`#shards-rows tr[data-id="${id}"]`).click();
    await expect(page.locator("#shard-id")).toHaveValue(id, { timeout: 10_000 });
}

async function save(page: Page, id: string) {
    const done = page.waitForResponse(
        (r) => r.url().endsWith(`/console/api/shards/${id}`) && r.request().method() === "PATCH",
    );
    await page.locator('#shard-form button[type="submit"]').click();
    const resp = await done;
    expect(resp.ok(), `PATCH: HTTP ${resp.status()} ${await resp.text()}`).toBeTruthy();
    await expect(page.locator("#shard-form-error")).toBeHidden();
}

async function openAdvanced(page: Page) {
    const adv = page.locator("#shard-form .shard-advanced");
    if (!(await adv.evaluate((d: HTMLDetailsElement) => d.open))) await adv.locator("summary").click();
}

// An admin fixing a typo in someone else's confined shard saved
// book_access = the boxes ticked in a picker listing the ADMIN's books
// — none — and an empty list means all the owner's books. Archived and
// unlisted books were dropped the same way for the owner. The picker
// now lists the owner's books (archived ones marked), keeps IDs it
// can't name, and the save only sends book_access once it's touched.
test("an edit keeps the shard's book confinement", async ({ stack, browser, request }) => {
    const owner = await createTestUser();
    const admin = await createTestUser({ role: "admin" });
    const recipes = await createBook(request, stack, owner, `Owner Recipes ${crypto.randomBytes(3).toString("hex")}`);
    const old = await createBook(request, stack, owner, `Old Stuff ${crypto.randomBytes(3).toString("hex")}`, true);
    const ghost = crypto.randomUUID();
    const books = [recipes.id, old.id, ghost];
    const id = await createShard(request, stack, owner, { book_access: books });

    const { ctx, page } = await openPanel(browser, stack, admin);
    try {
        // The row opens from the keyboard (its name is a button).
        const open = page.getByRole("button", { name: `Open shard: Panel ${id}` });
        await open.focus();
        await page.keyboard.press("Enter");
        await expect(page.locator("#shard-id")).toHaveValue(id, { timeout: 10_000 });

        const picker = page.locator("#shard-book-access");
        await expect(picker.locator(`input[value="${recipes.id}"]`)).toBeChecked();
        await expect(picker.locator(`input[value="${old.id}"]`)).toBeChecked();
        await expect(picker.locator(`input[value="${ghost}"]`)).toBeChecked();
        await expect(picker).toContainText("(archived)");
        await expect(picker).toContainText(`(unavailable) ${ghost}`);

        await page.locator("#shard-description").fill("after");
        const patch = page.waitForRequest((r) => r.method() === "PATCH" && r.url().endsWith(`/shards/${id}`));
        await save(page, id);
        expect(Object.keys((await patch).postDataJSON())).not.toContain("book_access");
        let s = await getShard(request, stack, owner, id);
        expect(s.description).toBe("after");
        expect([...s.book_access].sort()).toEqual([...books].sort());

        // Touching the picker does send it.
        await openShard(page, id);
        await picker.locator(`input[value="${ghost}"]`).uncheck();
        await save(page, id);
        s = await getShard(request, stack, owner, id);
        expect([...s.book_access].sort()).toEqual([recipes.id, old.id].sort());
    } finally {
        await ctx.close();
    }
});

// Unticking both Chat and API emptied the prompt textarea, and the
// save persisted the empty prompt.
test("turning chat and API off keeps the system prompt", async ({ stack, browser, request }) => {
    const owner = await createTestUser({ role: "admin" });
    const id = await createShard(request, stack, owner);
    const { ctx, page } = await openPanel(browser, stack, owner);
    try {
        await openShard(page, id);
        await page.locator("#shard-chat-enabled").uncheck();
        await page.locator("#shard-api-enabled").uncheck();
        await expect(page.locator("#shard-prompt")).toBeDisabled();
        await expect(page.locator("#shard-prompt")).toHaveValue("You answer questions about recipes, tersely.");
        await save(page, id);
        const s = await getShard(request, stack, owner, id);
        expect(s.chat_enabled).toBe(false);
        expect(s.api_enabled).toBe(false);
        expect(s.system_prompt).toBe("You answer questions about recipes, tersely.");

        await openShard(page, id);
        await page.locator("#shard-chat-enabled").check();
        await expect(page.locator("#shard-prompt")).toBeEnabled();
        await expect(page.locator("#shard-prompt")).toHaveValue("You answer questions about recipes, tersely.");
    } finally {
        await ctx.close();
    }
});

// Cancel on the label prompt fell through to the mint.
test("cancelling the token label mints nothing", async ({ stack, browser, request }) => {
    const owner = await createTestUser({ role: "admin" });
    const id = await createShard(request, stack, owner);
    const tokens = async () => {
        const r = await request.get(`${stack.workspaceURL}/console/api/shards/${id}/tokens`, { headers: authed(owner) });
        expect(r.ok()).toBeTruthy();
        return ((await r.json()).items || []).length;
    };
    const { ctx, page } = await openPanel(browser, stack, owner);
    try {
        await openShard(page, id);
        let posts = 0;
        page.on("request", (r) => {
            if (r.method() === "POST" && r.url().endsWith(`/shards/${id}/tokens`)) posts++;
        });
        page.once("dialog", (d) => d.dismiss());
        await page.locator("#shard-mint-token").click();
        await page.waitForTimeout(500);
        expect(posts).toBe(0);
        await expect(page.locator("#token-modal")).toBeHidden();
        expect(await tokens()).toBe(0);

        // OK still mints (the test can see a mint).
        page.once("dialog", (d) => d.accept("kiosk"));
        await page.locator("#shard-mint-token").click();
        await expect(page.locator("#token-modal")).toBeVisible({ timeout: 10_000 });
        expect(await tokens()).toBe(1);
    } finally {
        await ctx.close();
    }
});

// A blank temperature was sent as 0 (now honored as greedy sampling),
// and an emptied idle window or schema was never sent, so it stayed.
test("temperature, idle window and schemas save as shown", async ({ stack, browser, request }) => {
    const owner = await createTestUser({ role: "admin" });
    const id = await createShard(request, stack, owner, {
        temperature: 0.3,
        session_max_age: 1800,
        input_schema: { type: "object" },
    });
    const { ctx, page } = await openPanel(browser, stack, owner);
    try {
        await openShard(page, id);
        await openAdvanced(page);
        await page.locator("#shard-temperature").fill("");
        await page.locator("#shard-session-max-age").fill("");
        const schema = page.locator("#shard-form details.shard-schema").first();
        if (!(await schema.evaluate((d: HTMLDetailsElement) => d.open))) await schema.locator("summary").click();
        await page.locator("#shard-input-schema").fill("");
        await save(page, id);
        let s = await getShard(request, stack, owner, id);
        expect(s.temperature).toBeCloseTo(0.3, 5);
        expect(s.session_max_age ?? null).toBeNull();
        expect(s.input_schema ?? null).toBeNull();

        await openShard(page, id);
        await openAdvanced(page);
        await page.locator("#shard-temperature").fill("0");
        await save(page, id);
        s = await getShard(request, stack, owner, id);
        expect(s.temperature).toBe(0);

        // Create with the temperature blanked gets the default.
        await backToList(page);
        await page.locator("#shards-new").click();
        await expect(page.locator("#shard-id")).toBeEnabled();
        const nid = `e2e-sp-${crypto.randomBytes(4).toString("hex")}`;
        await page.locator("#shard-id").fill(nid);
        await page.locator("#shard-name").fill("Blank Temperature");
        await page.locator("#shard-prompt").fill("You are a test shard.");
        await openAdvanced(page);
        await page.locator("#shard-temperature").fill("");
        const created = page.waitForResponse(
            (r) => r.url().endsWith("/console/api/shards") && r.request().method() === "POST",
        );
        await page.locator('#shard-form button[type="submit"]').click();
        expect((await created).ok()).toBeTruthy();
        s = await getShard(request, stack, owner, nid);
        expect(s.temperature).toBeCloseTo(0.7, 5);
    } finally {
        await ctx.close();
    }
});
