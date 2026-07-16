// home.spec.ts — the 4-quadrant launchpad (TESTING-PLAN.md §"Phase 3"
// home.spec: launchpad + weather widget). Home is the post-login
// landing view, so a broken quadrant is the first thing every user
// sees. Quadrants: Pinned · action quad · Recent · Weather.

import { test as base, expect } from "@playwright/test";
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

test.describe.configure({ mode: "serial" });

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

test("the launchpad renders all four quadrants with live data", async ({ stack, browser, request }) => {
    const user = await createTestUser();

    // Seed one pinned note (pins quadrant) — a recent edit also makes
    // it Recent-quadrant material.
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers: authed(user),
            data: { title: "Pinned Landmark", content: "home spec content" },
        })
    ).json();
    const pin = await request.post(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/pin`,
        { headers: authed(user), data: { pinned: true } },
    );
    expect(pin.ok(), `pin: HTTP ${pin.status()}`).toBeTruthy();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        // Home IS the landing panel after boot.
        await expect(page.locator("#panel-home")).toBeVisible({ timeout: 15_000 });
        await expect(page.locator("#home-greeting-line")).not.toHaveText("");

        // Pinned quadrant shows the seeded pin.
        await expect(page.locator("#home-pinned-grid")).toContainText("Pinned Landmark", {
            timeout: 10_000,
        });

        // Recent quadrant feeds off pipeline activity (extracted
        // memories + chat sessions), which a fresh user has none of —
        // the contract here is that it SETTLES on the empty state
        // instead of hanging on "Loading…" when both feeds are empty.
        await expect(page.locator("#home-recent-list")).toContainText("Nothing yet", {
            timeout: 10_000,
        });

        // Weather quadrant: this user has no stored location, the
        // endpoint answers {"error":"no_location"}, and headless
        // Chromium refuses the geolocation fallback — so the widget
        // must settle on the share-location prompt rather than
        // spinning on "Loading…" forever.
        await expect(page.locator("#home-weather .home-wx-prompt")).toBeVisible({ timeout: 10_000 });
        await expect(page.locator("#home-weather")).toContainText("location", { ignoreCase: true });
    } finally {
        await ctx.close();
    }
});

test("the New-note action card opens the notes surface with a fresh note", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#panel-home")).toBeVisible({ timeout: 15_000 });

        await page.locator("#home-act-note").click();

        // The card routes to the workspace, focuses the notes
        // surface, and newNote() lands in the editor.
        await expect(page.locator("#panel-workspace")).toBeVisible({ timeout: 10_000 });
        const editor = page.locator(".notes-shell .toastui-editor-ww-container .ProseMirror").first();
        await expect(editor).toBeVisible({ timeout: 10_000 });

        // The note really exists server-side.
        const list = await (
            await request.get(`${stack.workspaceURL}/console/api/books/personal/pages`, {
                headers: authed(user),
            })
        ).json();
        expect((list.items ?? []).length).toBeGreaterThanOrEqual(1);
    } finally {
        await ctx.close();
    }
});

test("a pinned-quadrant row routes to its surface on click", async ({ stack, browser, request }) => {
    const user = await createTestUser();
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers: authed(user),
            data: { title: "Routed Pin", content: "click me from home" },
        })
    ).json();
    await request.post(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/pin`, {
        headers: authed(user),
        data: { pinned: true },
    });

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#panel-home")).toBeVisible({ timeout: 15_000 });

        await page.locator("#home-pinned-grid").getByText("Routed Pin").first().click();

        // Lands on the workspace with the pinned note open and its
        // content loaded — the full pin → openDoc route.
        await expect(page.locator("#panel-workspace")).toBeVisible({ timeout: 10_000 });
        const editor = page.locator(".notes-shell .toastui-editor-ww-container .ProseMirror").first();
        await expect(editor).toContainText("click me from home", { timeout: 10_000 });
    } finally {
        await ctx.close();
    }
});
