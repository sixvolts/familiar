// shell.spec.ts — the desktop shell (app.js / workspace.js / app.css):
// Home, the sidebar, System status, the Users flyout, dialogs, and the
// page-event stream. Model-free.

import { test as base, expect, Page, BrowserContext } from "@playwright/test";
import { Client } from "pg";
import * as crypto from "node:crypto";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, seedCredential, TestUser } from "../fixtures/user";

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

async function signedIn(browser: any, stack: GatewayStack, user: TestUser): Promise<{ ctx: BrowserContext; page: Page }> {
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    return { ctx, page };
}

async function note(request: any, stack: GatewayStack, user: TestUser, title: string) {
    return (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers: authed(user),
            data: { title, content: "body" },
        })
    ).json();
}

async function openDoc(page: Page, surface: string, id: string, title: string) {
    await page.evaluate(([s, i, t]) => (window as any).FamiliarWorkspace.openDoc(s, i, t), [surface, id, title]);
    await expect(page.locator(".ws-tab-label", { hasText: title })).toBeVisible({ timeout: 10_000 });
}

// The page-event stream (live refresh for notes, wiki, the sidebar)
// opens once signed in, and opens again after the server refuses it.
// It used to open at script load, so a signed-out page load (or a 502
// while the gateway restarted) ended live refresh for the whole session.
test("the page-event stream opens after sign-in and reopens after a refusal", async ({ stack, browser }) => {
    const user = await createTestUser();
    await seedCredential(user.id);
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    let opens = 0;
    await page.route("**/console/api/events/pages", async (r) => {
        opens++;
        if (opens === 1) await r.fulfill({ status: 502, body: "gateway restarting" });
        else await r.continue();
    });
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-login")).toBeVisible({ timeout: 15_000 });
        await page.waitForTimeout(500);
        expect(opens, "the stream opened on a signed-out page").toBe(0);

        // Sign in; the ceremony is stubbed, the session cookie is real.
        await attachSession(ctx, stack.workspaceURL, user);
        await page.route("**/console/api/auth/login/begin", (r) => r.fulfill({ json: { publicKey: { challenge: "AAAA" } } }));
        await page.route("**/console/api/auth/login/finish", (r) => r.fulfill({ json: { status: "authenticated" } }));
        await page.evaluate(() => {
            const buf = () => new Uint8Array([1]).buffer;
            (navigator.credentials as any).get = async () => ({
                id: "x", rawId: buf(), type: "public-key",
                response: { authenticatorData: buf(), clientDataJSON: buf(), signature: buf(), userHandle: null },
                getClientExtensionResults: () => ({}),
            });
        });
        await page.locator("#btn-login").click();
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        // First open refused (502); the second is the reconnect.
        await expect.poll(() => opens, { timeout: 10_000 }).toBeGreaterThanOrEqual(2);
    } finally {
        await ctx.close();
    }
});

// Viewing someone's dashboard shows only their data. The admin's own
// load was still in flight; whichever response landed last rendered, so
// the admin's own facts appeared under "Viewing dashboard of alice".
test("an admin viewing someone's dashboard sees only theirs", async ({ stack, browser }) => {
    const admin = await createTestUser({ role: "admin", displayName: "Boss Person" });
    const alice = await createTestUser({ displayName: "Alice Viewed" });
    const { ctx, page } = await signedIn(browser, stack, admin);
    try {
        // Delay the admin's own overview so it lands after Alice's.
        await page.route("**/console/api/dashboard/overview*", async (r) => {
            if (!r.request().url().includes("user_id=")) await new Promise((res) => setTimeout(res, 1500));
            await r.continue();
        });
        await page.evaluate(() => (window as any).appSwitchPanel("users"));
        const row = page.locator(`#users-rows tr[data-id="${alice.id}"]`);
        await expect(row).toBeVisible({ timeout: 10_000 });
        await row.click();
        await expect(page.locator("#user-detail-name")).toHaveText("Alice Viewed");
        await page.locator("#user-view-dashboard").click();
        await expect(page.locator("#dash-hero-title")).toContainText("Alice Viewed", { timeout: 10_000 });
        await page.waitForTimeout(2500); // the delayed self load has landed by now
        await expect(page.locator("#dash-hero-title")).toContainText("Alice Viewed");
    } finally {
        await ctx.close();
    }
});

// A silent failover (the chat model moved to its backup) shows the
// banner; System status shows each role's chain. Both were served and
// neither was drawn.
test("a failover shows the banner, and System status shows the roles", async ({ stack, browser }) => {
    const admin = await createTestUser({ role: "admin" });
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, admin);
    const page = await ctx.newPage();
    await page.route("**/console/api/auth/status", async (r) => {
        const resp = await r.fetch();
        const body = await resp.json();
        body.maintenance = { active: false, reason: "failover", model: "backup-model-x", message: "" };
        await r.fulfill({ response: resp, json: body });
    });
    await page.route("**/console/api/status", async (r) => {
        const resp = await r.fetch();
        const body = await resp.json();
        body.roles = [{
            role: "chat", active_id: "backup-model-x", active_tier: 2, degraded: true,
            candidates: [{ model_id: "primary-model", tier: 1, status: "offline" }, { model_id: "backup-model-x", tier: 2, status: "online", active: true }],
        }];
        await r.fulfill({ response: resp, json: body });
    });
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        const banner = page.locator(".maintenance-banner");
        await expect(banner).toContainText("backup-model-x", { timeout: 10_000 });
        await expect(banner).toHaveAttribute("role", "status");

        await page.evaluate(() => (window as any).appSwitchPanel("user"));
        await page.locator('.user-subnav-row[data-user-section="system-status"]').click();
        const roles = page.locator("#dash-roles-rows");
        await expect(roles).toContainText("chat", { timeout: 10_000 });
        await expect(roles).toContainText("primary-model");
        await expect(roles).toContainText("BACKUP");
    } finally {
        await ctx.close();
    }
});

// The Users flyout acts on the user it shows. Click row A then row B
// with A's fetch slow: the flyout showed A (the last to land) while
// every action went to B.
test("the Users flyout shows the last user clicked and acts on them", async ({ stack, browser }) => {
    const admin = await createTestUser({ role: "admin" });
    const early = await createTestUser({ displayName: "Early Clicked" });
    const late = await createTestUser({ displayName: "Late Clicked" });
    const { ctx, page } = await signedIn(browser, stack, admin);
    try {
        await page.evaluate(() => (window as any).appSwitchPanel("users"));
        const rowA = page.locator(`#users-rows tr[data-id="${early.id}"]`);
        const rowB = page.locator(`#users-rows tr[data-id="${late.id}"]`);
        await expect(rowA).toBeVisible({ timeout: 10_000 });
        await page.route(`**/console/api/users/${early.id}`, async (r) => {
            await new Promise((res) => setTimeout(res, 1500));
            await r.continue();
        });
        const statusPosts: string[] = [];
        page.on("request", (req) => {
            if (req.method() === "POST" && req.url().endsWith("/status")) statusPosts.push(req.url());
        });
        await rowA.click();
        await rowB.click();
        await page.waitForTimeout(2500); // A's slow response has landed
        await expect(page.locator("#user-detail-name")).toHaveText("Late Clicked");
        page.once("dialog", (d) => d.accept());
        await page.locator("#user-disable").click();
        await expect.poll(() => statusPosts.length).toBe(1);
        expect(statusPosts[0]).toContain(`/users/${late.id}/status`);
    } finally {
        await ctx.close();
    }
});

// System status refreshes every 30s while open, and reloads on every
// visit. It checked a panel variable the sub-nav never set, so it did
// neither.
test("System status refreshes while open and on every visit", async ({ stack, browser }) => {
    const admin = await createTestUser({ role: "admin" });
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, admin);
    const page = await ctx.newPage();
    let loads = 0;
    page.on("request", (req) => {
        if (req.method() === "GET" && new URL(req.url()).pathname === "/console/api/status") loads++;
    });
    try {
        await page.clock.install();
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.evaluate(() => (window as any).appSwitchPanel("user"));
        const statusRow = page.locator('.user-subnav-row[data-user-section="system-status"]');
        await statusRow.click();
        await expect.poll(() => loads).toBe(1);
        await page.clock.runFor(31_000);
        await expect.poll(() => loads).toBe(2);
        await page.locator('.user-subnav-row[data-user-section="profile"]').click();
        await statusRow.click();
        await expect.poll(() => loads).toBe(3);
    } finally {
        await ctx.close();
    }
});

// Home refreshes on every visit (it loaded once per page, so Recent
// never showed the chat you just had).
test("Home refreshes on every visit", async ({ stack, browser }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    let recentLoads = 0;
    page.on("request", (req) => {
        if (req.url().includes("/console/api/conversations?limit=12")) recentLoads++;
    });
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#panel-home")).toBeVisible({ timeout: 15_000 });
        await expect.poll(() => recentLoads).toBe(1);
        await page.evaluate(() => (window as any).appSwitchPanel("workspace"));
        await page.locator("#sidebar-home-row").click();
        await expect.poll(() => recentLoads).toBe(2);
    } finally {
        await ctx.close();
    }
});

// A kiosk's Home shows neither the owner's blocks (which it doesn't
// load, so they sat on "Loading…") nor shortcuts it can't use.
test("a kiosk's Home hides the owner's blocks and shortcuts it can't use", async ({ stack, browser, request }) => {
    const owner = await createTestUser({ role: "admin" });
    const id = `e2e-shellkiosk-${Date.now().toString(36)}`;
    const created = await request.post(`${stack.workspaceURL}/console/api/shards`, {
        headers: authed(owner),
        data: {
            id, name: `Kiosk ${id}`, description: "e2e", persistence: "persistent", visibility: "isolated",
            scope_tag: `shard:${id}`, system_prompt: "e2e", tool_allowlist: [],
            console_access: true, console_panels: ["books"], chat_enabled: false,
        },
    });
    expect(created.ok(), await created.text()).toBeTruthy();
    const token = crypto.randomBytes(32).toString("base64url");
    const client = new Client({ connectionString: process.env.FAMILIAR_TEST_DSN });
    await client.connect();
    try {
        await client.query(
            `INSERT INTO admin_sessions (token, user_id, principal_type, principal_id, expires_at)
             VALUES ($1, $2, 'shard', $3, NOW() + INTERVAL '1 hour')`,
            [token, owner.id, id],
        );
    } finally {
        await client.end();
    }
    const ctx = await browser.newContext();
    await ctx.addCookies([{ name: "familiar_admin_session", value: token, url: stack.workspaceURL, httpOnly: true, sameSite: "Lax" }]);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.evaluate(() => (window as any).appSwitchPanel("home"));
        await expect(page.locator("#home-greeting-line")).toBeVisible();
        for (const sel of ["#home-block-pinned", "#home-block-recent", "#home-block-weather", "#home-act-chat", "#home-act-shard", "#home-act-scheduled"]) {
            await expect(page.locator(sel), sel).toBeHidden();
        }
    } finally {
        await ctx.close();
    }
});

// Dropping a tab on itself leaves it where it was; it used to jump to
// the end of the strip.
test("dropping a tab on itself leaves it in place", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const n1 = await note(request, stack, user, "Tab One");
    const n2 = await note(request, stack, user, "Tab Two");
    const { ctx, page } = await signedIn(browser, stack, user);
    try {
        await page.evaluate(() => (window as any).FamiliarWorkspace.setLayout("single"));
        await openDoc(page, "notes", n1.id, "Tab One");
        await openDoc(page, "notes", n2.id, "Tab Two");
        const labels = () => page.locator(".ws-tab .ws-tab-label").allTextContents();
        const before = await labels();
        const one = page.locator(".ws-tab", { hasText: "Tab One" });
        await one.dragTo(one, { targetPosition: { x: 4, y: 10 } });
        await page.waitForTimeout(300);
        expect(await labels()).toEqual(before);
    } finally {
        await ctx.close();
    }
});

// The sidebar highlight follows the panel however it was reached: a
// Home card opening a chat used to leave Home highlighted.
test("the sidebar highlight follows the panel", async ({ stack, browser }) => {
    const user = await createTestUser();
    const { ctx, page } = await signedIn(browser, stack, user);
    try {
        await page.evaluate(() => (window as any).appSwitchPanel("home"));
        await expect(page.locator("#sidebar-home-row")).toHaveClass(/is-active/);
        await page.locator("#home-act-chat").click();
        await expect(page.locator(".sidebar-cat-chat")).toHaveClass(/is-active/, { timeout: 10_000 });
        await expect(page.locator("#sidebar-home-row")).not.toHaveClass(/is-active/);
    } finally {
        await ctx.close();
    }
});

// A gateway blip on the first Users visit shows an error with Retry
// (the panel stayed blank until a reload), and a 5xx at boot retries
// instead of sending a signed-in user to the passkey login.
test("a gateway blip doesn't blank Users or sign anyone out", async ({ stack, browser }) => {
    const admin = await createTestUser({ role: "admin" });
    await seedCredential(admin.id);
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, admin);
    const page = await ctx.newPage();
    let statusCalls = 0;
    await page.route("**/console/api/auth/status", async (r) => {
        statusCalls++;
        if (statusCalls === 1) await r.fulfill({ status: 502, body: "gateway restarting" });
        else await r.continue();
    });
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-loading-msg")).toContainText("Can't reach", { timeout: 10_000 });
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await expect(page.locator("#view-login")).toBeHidden();

        let usersCalls = 0;
        await page.route("**/console/api/users", async (r) => {
            usersCalls++;
            if (usersCalls === 1) await r.fulfill({ status: 502, json: { error: "gateway restarting" } });
            else await r.continue();
        });
        await page.evaluate(() => (window as any).appSwitchPanel("users"));
        await expect(page.locator("#users-load-error")).toBeVisible({ timeout: 10_000 });
        await page.locator("#users-load-retry").click();
        await expect(page.locator("#users-section")).toBeVisible({ timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});

// Menus open at the pointer when the UI is zoomed. Positions were set
// in CSS pixels that the zoom scaled again: at 120% the menu landed 20%
// further out.
test("context menus open at the pointer when the UI is zoomed", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const n1 = await note(request, stack, user, "Zoom Tab");
    const ctx = await browser.newContext();
    await ctx.addInitScript(() => localStorage.setItem("familiar-zoom", "large"));
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await openDoc(page, "notes", n1.id, "Zoom Tab");
        const tab = page.locator(".ws-tab", { hasText: "Zoom Tab" });
        const box = (await tab.boundingBox())!;
        const x = box.x + box.width / 2, y = box.y + box.height / 2;
        await page.mouse.click(x, y, { button: "right" });
        const menu = page.locator(".sidebar-ctxmenu");
        await expect(menu).toBeVisible();
        const m = (await menu.boundingBox())!;
        expect(Math.abs(m.x - x), `menu at ${m.x}, pointer at ${x}`).toBeLessThan(3);
        expect(Math.abs(m.y - y), `menu at ${m.y}, pointer at ${y}`).toBeLessThan(3);
    } finally {
        await ctx.close();
    }
});

// Keyboard: a category expands from its chevron button, and Delete
// closes the focused tab. The chevron was a mouse-only span inside a
// link (with the + button, also inside the link), and the tab's × a
// span inside the tab button.
test("the sidebar and tabs work from the keyboard", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const n1 = await note(request, stack, user, "Keyboard Tab");
    const { ctx, page } = await signedIn(browser, stack, user);
    try {
        const chevron = page.locator(".sidebar-cat-notes .sidebar-row-chevron");
        await expect(chevron).toHaveAttribute("aria-expanded", "false");
        await chevron.focus();
        await page.keyboard.press("Enter");
        await expect(chevron).toHaveAttribute("aria-expanded", "true");
        await expect(page.locator('.sidebar-children[data-category="notes"]')).toBeVisible();
        expect(await page.locator(".sidebar-cat a .sidebar-cat-new, .sidebar-cat a button").count(), "a button nested in a link").toBe(0);

        await openDoc(page, "notes", n1.id, "Keyboard Tab");
        await page.locator(".ws-tab", { hasText: "Keyboard Tab" }).focus();
        await page.keyboard.press("Delete");
        await expect(page.locator(".ws-tab-label", { hasText: "Keyboard Tab" })).toHaveCount(0);
    } finally {
        await ctx.close();
    }
});

// Overlays are dialogs: focus moves in, Escape closes them, and toasts
// are announced.
test("the user detail is a dialog: it takes focus and Escape closes it", async ({ stack, browser }) => {
    const admin = await createTestUser({ role: "admin" });
    const other = await createTestUser();
    const { ctx, page } = await signedIn(browser, stack, admin);
    try {
        await expect(page.locator("#toast-container")).toHaveAttribute("aria-live", "polite");
        await page.evaluate(() => (window as any).appSwitchPanel("users"));
        const row = page.locator(`#users-rows tr[data-id="${other.id}"]`);
        await expect(row).toBeVisible({ timeout: 10_000 });
        await row.click();
        const detail = page.locator("#user-detail");
        await expect(detail).toBeVisible();
        await expect(detail).toHaveAttribute("role", "dialog");
        await expect(detail).toHaveAttribute("aria-modal", "true");
        await expect.poll(() => page.evaluate(() => !!document.activeElement?.closest("#user-detail"))).toBe(true);
        await page.keyboard.press("Escape");
        await expect(detail).toBeHidden();
    } finally {
        await ctx.close();
    }
});

// Informational text uses the readable token (the tertiary one measured
// 2.2–2.5:1), and the radius/text aliases components use are defined.
test("informational text is readable and component tokens exist", async ({ stack, browser }) => {
    const user = await createTestUser();
    const { ctx, page } = await signedIn(browser, stack, user);
    try {
        const r = await page.evaluate(() => {
            const probe = (v: string) => {
                const el = document.createElement("span");
                el.style.color = `var(${v})`;
                document.body.appendChild(el);
                const c = getComputedStyle(el).color;
                el.remove();
                return c;
            };
            const count = document.querySelector(".sidebar-row-count") as HTMLElement;
            const root = getComputedStyle(document.documentElement);
            return {
                count: getComputedStyle(count).color,
                fg3: probe("--fg-3"),
                fg4: probe("--fg-4"),
                radiusCard: root.getPropertyValue("--radius-card").trim(),
                textBase: root.getPropertyValue("--text-base").trim(),
            };
        });
        expect(r.count).toBe(r.fg3);
        expect(r.count).not.toBe(r.fg4);
        expect(r.radiusCard).not.toBe("");
        expect(r.textBase).not.toBe("");
    } finally {
        await ctx.close();
    }
});
