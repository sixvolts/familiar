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
import { createTestUser, attachSession } from "../fixtures/user";

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

test("the unauthenticated boot screen is visually stable", async ({
    stack,
    page,
}, testInfo) => {
    await page.goto(stack.workspaceURL);

    if (isMobile(testInfo.project.name)) {
        // Settle off the boot spinner before capturing, or the shot
        // races the auth gate and diffs on the spinner frame.
        await expect(page.locator("#mob-auth-loading")).toBeHidden({
            timeout: 15_000,
        });
        await expect(
            page.locator("#mob-auth-login:visible, #mob-auth-setup:visible").first(),
        ).toBeVisible();
    } else {
        // Desktop serves the SPA index. NOT waitForLoadState("networkidle"):
        // the app holds an SSE stream open for notes-sync, so the network
        // is never idle and that wait can only ever time out. Wait for the
        // document to be interactive instead.
        await page.waitForLoadState("domcontentloaded");
        await expect(page.locator("body")).toBeVisible();
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
// text — rendered markdown, not a native control. This asserts the
// rendered surface rather than the editor, because that's where the
// break showed.
test("rendered task-list checkboxes are visually stable", async ({
    stack,
    page,
    context,
}, testInfo) => {
    const user = await createTestUser();
    await attachSession(context, stack.workspaceURL, user);
    await page.goto(stack.workspaceURL);

    const shell = isMobile(testInfo.project.name)
        ? page.locator("#mob-app")
        : page.locator("#view-dashboard");
    await expect(shell).toBeVisible({ timeout: 15_000 });

    // Render a task list through the app's own markdown pipeline rather
    // than asserting against a hand-built DOM, so the baseline covers
    // the real renderer + CSS, which is what regressed.
    const md = [
        "## Checklist",
        "",
        "- [x] a completed item with a reasonably long label",
        "- [ ] an open item with a reasonably long label",
        "- [ ] a third item",
    ].join("\n");

    const host = await page.evaluateHandle((markdown) => {
        const el = document.createElement("div");
        el.id = "visual-md-probe";
        el.style.cssText =
            "position:fixed;inset:0;z-index:99999;background:var(--bg,#fff);padding:24px;overflow:auto;";
        // Prefer the app's own renderer when it exposes one; fall back
        // to a minimal task-list DOM that still exercises the CSS.
        const w = window as unknown as Record<string, any>;
        const render = w.renderMarkdown || w.md?.render?.bind(w.md);
        if (typeof render === "function") {
            el.innerHTML = render(markdown);
        } else {
            el.innerHTML =
                '<h2>Checklist</h2><ul class="contains-task-list">' +
                markdown
                    .split("\n")
                    .filter((l) => l.startsWith("- ["))
                    .map(
                        (l) =>
                            '<li class="task-list-item"><input type="checkbox" ' +
                            (l.includes("[x]") ? "checked " : "") +
                            "disabled> " +
                            l.slice(6) +
                            "</li>",
                    )
                    .join("") +
                "</ul>";
        }
        document.body.appendChild(el);
        return el;
    }, md);

    await expect(page.locator("#visual-md-probe")).toBeVisible();
    await expect(page.locator("#visual-md-probe")).toHaveScreenshot(
        "task-list-checkboxes.png",
        SHOT,
    );

    await host.dispose();
});
