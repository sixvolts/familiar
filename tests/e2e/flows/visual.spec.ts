// visual.spec.ts — pixel baselines. DOM assertions pass happily while a
// view is visually unusable: a CSS change once tiled a checkbox
// background into a grid overlapping the label text, and every
// locator-based assertion stayed green because the DOM was correct. The
// render was not. These baselines are the regression half of that
// problem; the adjudicator (tier 5) covers the case where no baseline
// exists yet by looking at the screenshot itself.
//
// Runs under BOTH projects — "chromium" (Desktop Chrome) and "mobile"
// (Pixel 7) — so a desktop-only or mobile-only regression can't hide.
// Playwright suffixes each snapshot with the project name and platform,
// so the two viewports keep independent baselines. Baselines are
// platform-specific: these are generated on, and only valid for, the
// macOS runner (icecube). Regenerate with `--update-snapshots`.
//
// Keep these few and load-bearing. A baseline per view is a maintenance
// tax; a baseline on the views that break is insurance.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, seedCredential } from "../fixtures/user";

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

// Shared options. animations:"disabled" freezes CSS transitions so a
// spinner mid-rotation doesn't diff; maxDiffPixelRatio absorbs
// sub-pixel font rasterisation without absorbing a real layout break.
const SHOT = {
    animations: "disabled" as const,
    maxDiffPixelRatio: 0.01,
};

const isMobile = (name: string) => name === "mobile";

// The login screen. Which screen an unauthenticated boot shows depends
// on the database: login once any passkey exists, first-run setup on an
// empty table. This seeds one, so the baseline doesn't depend on which
// specs ran before it (or on a Go tier having truncated the table).
test("the unauthenticated boot screen is visually stable", async ({
    stack,
    page,
}, testInfo) => {
    await seedCredential((await createTestUser()).id);
    await page.goto(stack.workspaceURL);

    if (isMobile(testInfo.project.name)) {
        // Settle off the boot spinner before capturing, or the shot
        // races the auth gate and diffs on the spinner frame.
        await expect(page.locator("#mob-auth-loading")).toBeHidden({
            timeout: 15_000,
        });
        await expect(page.locator("#mob-auth-login")).toBeVisible();
    } else {
        // NOT waitForLoadState("networkidle"): the app holds an SSE stream
        // open for notes-sync, so the network is never idle and that wait
        // can only ever time out. Wait for the boot view to hand over.
        await expect(page.locator("#view-loading")).toBeHidden({ timeout: 15_000 });
        await expect(page.locator("#view-login")).toBeVisible();
    }

    await expect(page).toHaveScreenshot("boot-unauthenticated.png", SHOT);
});

test("the authenticated app shell is visually stable", async ({
    stack,
    page,
    context,
}, testInfo) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(stack.workspaceURL);

    if (isMobile(testInfo.project.name)) {
        await expect(page.locator("#mob-app")).toBeVisible({ timeout: 15_000 });
    } else {
        await expect(page.locator("#view-dashboard")).toBeVisible({
            timeout: 15_000,
        });
    }

    await expect(page).toHaveScreenshot("app-shell-authenticated.png", SHOT);
});

// The checkbox case specifically. §5's example regression was a
// task-list checkbox whose background tiled into a grid over the label
// text. It showed in a note: TOAST UI renders a task-list item's box as
// a background image on `.toastui-editor-contents .task-list-item::before`,
// and mobile.css pads that pseudo-element out to a 44px tap target,
// re-declaring the background properties the vendor shorthand resets.
// So the baseline is a real note in the real editor, not a hand-built
// list: only that has the selector the fix (and the regression) lives on.
test("rendered task-list checkboxes are visually stable", async ({
    stack,
    page,
    context,
    request,
}, testInfo) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    const md = [
        "## Checklist",
        "",
        "- [x] a completed item with a reasonably long label",
        "- [ ] an open item with a reasonably long label",
        "- [ ] a third item",
    ].join("\n");
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
            data: { title: "Checklist", content: md },
        })
    ).json();

    let editor;
    if (isMobile(testInfo.project.name)) {
        await page.goto(`${stack.workspaceURL}/#notes/${note.id}`);
        editor = page.locator("#mob-note-body .toastui-editor-ww-container .ProseMirror").first();
    } else {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes").click();
        const shell = page.locator(".notes-shell").first();
        await expect(shell).toBeVisible({ timeout: 10_000 });
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id } }));
        }, note.id);
        editor = shell.locator(".toastui-editor-ww-container .ProseMirror").first();
    }
    await expect(editor).toContainText("a third item", { timeout: 15_000 });
    // The surface the CSS targets, or this baseline guards nothing.
    await expect(editor.locator(".task-list-item")).toHaveCount(3);
    expect(await editor.evaluate((el) => el.classList.contains("toastui-editor-contents"))).toBe(true);

    // Just the list and a margin round it: the ::before box reaches
    // past the item's edges, and in a shot of the whole editor a tiled
    // checkbox is too few pixels to clear maxDiffPixelRatio.
    const box = (await editor.locator("ul").first().boundingBox())!;
    const pad = 16;
    await expect(page).toHaveScreenshot("task-list-checkboxes.png", {
        ...SHOT,
        clip: { x: Math.max(0, box.x - pad), y: Math.max(0, box.y - pad), width: box.width + 2 * pad, height: box.height + 2 * pad },
    });
});
