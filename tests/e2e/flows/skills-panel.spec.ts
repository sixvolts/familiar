// skills-panel.spec.ts — the Skills panels: New skill doesn't overwrite
// a skill of the same name, the "Use in chat" box keeps telling the
// truth after a failed save, and the System skills catalog opens from
// the keyboard. No model needed.

import { test as base, expect, Page, BrowserContext } from "@playwright/test";
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

async function openPanel(browser: any, stack: GatewayStack, user: TestUser, panel: string): Promise<{ ctx: BrowserContext; page: Page }> {
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    await page.goto(stack.workspaceURL);
    await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
    await page.evaluate((p) => (window as any).appSwitchPanel(p), panel);
    return { ctx, page };
}

// New skill with a name you already use: refused, and the existing
// skill's instructions are intact. It overwrote them with a "saved" toast.
test("New skill doesn't overwrite a skill of the same name", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const name = `rules${Date.now().toString(36)}`;
    const put = await request.put(`${stack.workspaceURL}/console/api/skills/mine/${name}`, {
        headers: authed(user),
        data: { description: "House rules.", body: "# Rules\n\nORIGINAL" },
    });
    expect(put.ok(), await put.text()).toBeTruthy();
    const id = (await put.json()).id;

    const { ctx, page } = await openPanel(browser, stack, user, "skills");
    try {
        await expect(page.locator("#myskills-list")).toContainText(name.toUpperCase(), { timeout: 10_000 });
        await page.locator("#myskills-new").click();
        await page.locator("#skill-editor-name").fill(name);
        await page.locator("#skill-editor-desc").fill("Replacement.");
        await page.locator("#skill-editor-body").fill("# Rules\n\nREPLACED");
        await page.locator("#skill-editor-save").click();
        await expect(page.locator("#skill-editor-error")).toContainText("already have a skill named", { timeout: 10_000 });
        const content = await request.get(`${stack.workspaceURL}/console/api/skills/mine/${id}/content`, { headers: authed(user) });
        expect((await content.json()).body).toContain("ORIGINAL");
    } finally {
        await ctx.close();
    }
});

// A failed "Use in chat" save puts the box back; the next click then
// sends what the box shows. It derived the state from the stale package
// and sent the opposite.
test("Use in chat reverts on a failed save", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const name = `chat${Date.now().toString(36)}`;
    const put = await request.put(`${stack.workspaceURL}/console/api/skills/mine/${name}`, {
        headers: authed(user),
        data: { description: "Chat test.", body: "# Chat\n\nBODY" },
    });
    expect(put.ok()).toBeTruthy();

    const { ctx, page } = await openPanel(browser, stack, user, "skills");
    try {
        const box = page.locator("#myskills-list .skill-card", { hasText: name.toUpperCase() }).locator('input[type="checkbox"]');
        await expect(box).not.toBeChecked({ timeout: 10_000 });
        let fail = true;
        const sent: boolean[] = [];
        await page.route("**/console/api/skills/mine/*/chat", async (route) => {
            sent.push(route.request().postDataJSON().enabled);
            if (fail) return route.fulfill({ status: 500, body: JSON.stringify({ error: "boom" }) });
            return route.continue();
        });
        await box.click();
        await expect(page.locator("#myskills-error")).toBeVisible();
        await expect(box).not.toBeChecked();
        fail = false;
        await box.click();
        await expect.poll(() => sent.length).toBe(2);
        expect(sent).toEqual([true, true]);
        await expect(box).toBeChecked({ timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});

// The System skills catalog's entries open from the keyboard and say
// whether they're open.
test("the skill catalog opens from the keyboard", async ({ stack, browser }) => {
    const admin = await createTestUser({ role: "admin" });
    const { ctx, page } = await openPanel(browser, stack, admin, "system-skills");
    try {
        const header = page.locator("#skills-list .skill-header").first();
        await expect(header).toBeVisible({ timeout: 10_000 });
        await expect(header).toHaveAttribute("aria-expanded", "false");
        await header.focus();
        await page.keyboard.press("Enter");
        const opened = page.locator("#skills-list .skill-header").first();
        await expect(opened).toHaveAttribute("aria-expanded", "true");
        await expect(opened).toBeFocused();
        await expect(page.locator("#skills-list .skill-tools-list").first()).toBeVisible();
        await page.keyboard.press(" ");
        await expect(page.locator("#skills-list .skill-header").first()).toHaveAttribute("aria-expanded", "false");
    } finally {
        await ctx.close();
    }
});
