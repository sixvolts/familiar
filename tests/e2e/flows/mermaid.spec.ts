// mermaid.spec.ts — MEDIA-DIAGRAMS Phase 0: ```mermaid fences render
// as inline SVG diagrams in markdown previews. Two layers: the
// render pipeline itself (vendored bundle + init + render), and the
// notes-editor wiring (fence → .mermaid-block → SVG in the preview).

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

const FENCED_NOTE = "# Diagram note\n\n```mermaid\ngraph TD;\n  A[Start] --> B[End];\n```\n\ntail text\n";

test("the mermaid pipeline renders a fence to SVG, and bad syntax degrades", async ({
    stack,
    browser,
}) => {
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        const result = await page.evaluate(async () => {
            // The vendor bundle is deferred — wait for it.
            for (let i = 0; i < 100 && !(window as any).mermaid; i++) {
                await new Promise((r) => setTimeout(r, 100));
            }
            const fm = (window as any).familiarMermaid;
            if (!fm || !(window as any).mermaid) return { ok: false, why: "scripts missing" };

            const host = document.createElement("div");
            host.innerHTML =
                '<div class="mermaid-block" data-mermaid-pending="1">graph TD; A--&gt;B;</div>' +
                '<div class="mermaid-block" data-mermaid-pending="1">this is not mermaid {{{</div>';
            document.body.appendChild(host);
            await fm.renderAll(host);
            const blocks = host.querySelectorAll(".mermaid-block");
            const out = {
                ok: true,
                rendered: !!blocks[0].querySelector("svg") && blocks[0].classList.contains("is-rendered"),
                errored: blocks[1].classList.contains("is-error"),
                errorKeepsSource: (blocks[1].textContent || "").includes("not mermaid"),
            };
            host.remove();
            return out;
        });
        expect(result.ok, JSON.stringify(result)).toBe(true);
        expect(result.rendered, "valid graph must render to SVG").toBe(true);
        expect(result.errored, "bad syntax must mark is-error").toBe(true);
        expect(result.errorKeepsSource, "bad syntax must keep the source visible").toBe(true);
    } finally {
        await ctx.close();
    }
});

test("a mermaid fence survives both editor modes uncorrupted", async ({
    stack,
    browser,
    request,
}) => {
    // Desktop diagram EXPOSURE is an open design question (likely a
    // dedicated workspace tab) — what this pins is that the renderer
    // hook never corrupts content: the fence stays an editable code
    // block in Rich Text and intact source in Markdown mode, and a
    // save round-trips the fence byte-for-byte.
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Mermaid Note", content: FENCED_NOTE },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Mermaid Note" })
            .click();

        // Rich Text (default): the fence renders INLINE as SVG via
        // the WYSIWYG node view (read-only; click opens the tab).
        await expect(
            page.locator(".ProseMirror.toastui-editor-contents .mermaid-block.is-ww.is-rendered svg"),
        ).toBeVisible({ timeout: 10_000 });

        // Markdown mode: the raw fence is intact.
        await page.locator('button[data-mode="markdown"]').click();
        const md = await page.evaluate(() => {
            // One Toast UI instance mounts BOTH ProseMirror panes;
            // the markdown one lacks the -contents class.
            const el = document.querySelector(".toastui-editor .ProseMirror:not(.toastui-editor-contents)");
            return el ? (el as HTMLElement).innerText : "";
        });
        expect(md).toContain("```mermaid");
        expect(md).toContain("graph TD;");

        // Type into the note to trigger a save, then confirm the
        // stored markdown still carries the fence.
        await page.locator(".toastui-editor .ProseMirror:not(.toastui-editor-contents)").click();
        await page.keyboard.press("End");
        await page.keyboard.type(" edited");
        await expect
            .poll(async () => {
                const p = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
                        { headers },
                    )
                ).json();
                return p.content || "";
            }, { timeout: 10_000 })
            .toContain("```mermaid");
    } finally {
        await ctx.close();
    }
});

test("clicking an inline diagram opens the orange diagram tab; saving patches the fence", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Tab Diagram", content: FENCED_NOTE },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Tab Diagram" })
            .click();

        const inline = page.locator(".mermaid-block.is-ww.is-rendered");
        await expect(inline).toBeVisible({ timeout: 10_000 });
        await inline.click();

        // A diagram tab opens (never overriding the note tab), with
        // the tangerine category stripe and the live-editor split.
        const diagTab = page.locator('.ws-tab[data-category="diagram"]');
        await expect(diagTab).toBeVisible({ timeout: 10_000 });
        const stripe = await diagTab.evaluate((el) =>
            getComputedStyle(el).getPropertyValue("--cat-color").trim());
        expect(stripe).toBe("#E8853D");
        const source = page.locator(".diagram-source");
        await expect(source).toHaveValue(/graph TD;/, { timeout: 10_000 });
        await expect(page.locator(".diagram-preview .mermaid-block.is-rendered svg")).toBeVisible({
            timeout: 10_000,
        });

        // Edit the source and save — the page's fence updates while
        // the rest of the content survives.
        await source.fill("graph TD;\n  A[Start] --> B[End];\n  B --> C[New];");
        await page.locator(".diagram-shell button", { hasText: "Save to page" }).click();
        await expect
            .poll(async () => {
                const p = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`,
                        { headers },
                    )
                ).json();
                return p.content || "";
            }, { timeout: 10_000 })
            .toContain("B --> C[New];");
        const after = await (
            await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
                headers,
            })
        ).json();
        expect(after.content).toContain("# Diagram note");
        expect(after.content).toContain("tail text");
        expect(after.content).toContain("```mermaid");

        // Clicking the inline block again dedups onto the same tab.
        await page.locator(".ws-tab", { hasText: "Tab Diagram" }).first().click();
        const tabCount = await page.locator(".ws-tab").count();
        await page.locator(".mermaid-block.is-ww").first().click();
        await expect(page.locator('.ws-tab[data-category="diagram"].is-active')).toBeVisible({
            timeout: 10_000,
        });
        expect(await page.locator(".ws-tab").count()).toBe(tabCount);

        // RESTORE: a reload re-pulls the fence from the page into the
        // persisted diagram tab (the diagram.js restore branch).
        await page.reload();
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        // Boot lands in Home mode — bring the workspace forward.
        await page.evaluate(() => (window as any).appSwitchPanel("workspace"));
        await page.locator('.ws-tab[data-category="diagram"]').click();
        await expect(page.locator(".diagram-source")).toHaveValue(/B --> C\[New\]/, {
            timeout: 10_000,
        });
    } finally {
        await ctx.close();
    }
});

test("saving a diagram whose fence was deleted errors instead of clobbering", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Conflict Note", content: FENCED_NOTE },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Conflict Note" })
            .click();
        await page.locator(".mermaid-block.is-ww.is-rendered").click();
        await expect(page.locator(".diagram-source")).toHaveValue(/graph TD;/, { timeout: 10_000 });

        // Yank the fence out from under the open tab.
        const replaced = "# Diagram note\n\nno more diagram\n";
        await request.patch(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers,
            data: { content: replaced },
        });

        await page.locator(".diagram-source").fill("graph TD;\n  X --> Y;");
        await page.locator(".diagram-shell button", { hasText: "Save to page" }).click();
        // Error surfaces; the page is NOT clobbered.
        await expect(page.locator(".toast", { hasText: "no longer has this diagram" })).toBeVisible({
            timeout: 10_000,
        });
        const after = await (
            await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
                headers,
            })
        ).json();
        expect(after.content).toBe(replaced);
    } finally {
        await ctx.close();
    }
});

test("Toast UI's code-block language badge is suppressed by our CSS", async ({ stack, browser }) => {
    // Toast UI's WYSIWYG code block renders a floating "language ✎"
    // badge as `.toastui-editor-ww-code-block::after` plus a popup
    // input. We edit code inline and never need the language
    // switcher, so app.css hides both. Which Toast UI build emits the
    // ww-code-block class varies (older cached bundles add it; the
    // current vendored one renders pre.lang-text), so this guards the
    // OVERRIDE directly: inject the elements Toast UI would create and
    // assert the badge + popup are gone.
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        const r = await page.evaluate(() => {
            const host = document.createElement("div");
            host.className = "toastui-editor-contents";
            const pre = document.createElement("pre");
            pre.className = "toastui-editor-ww-code-block";
            pre.setAttribute("data-language", "text");
            host.appendChild(pre);
            const popup = document.createElement("div");
            popup.className = "toastui-editor-ww-code-block-language";
            host.appendChild(popup);
            document.body.appendChild(host);
            return {
                after: getComputedStyle(pre, "::after").display,
                popup: getComputedStyle(popup).display,
            };
        });
        expect(r.after, "language badge ::after must be hidden").toBe("none");
        expect(r.popup, "language popup must be hidden").toBe("none");
    } finally {
        await ctx.close();
    }
});

test("editor headings have real space below them, not Toast UI's near-zero default", async ({
    stack,
    browser,
}) => {
    // Toast UI's default heading margins are lopsided (h3 18px/2px,
    // h5/h6 9px/-4px), crowding body text under each header (user
    // report 2026-06-14). app.css rebalances them. Inject the editor
    // DOM shape and assert the bottom margins are no longer ~zero.
    const user = await createTestUser();
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        const margins = await page.evaluate(() => {
            const wrap = document.createElement("div");
            wrap.className = "notes-editor";
            const contents = document.createElement("div");
            contents.className = "toastui-editor-contents";
            wrap.appendChild(contents);
            document.body.appendChild(wrap);
            const out: Record<string, number> = {};
            for (const tag of ["h1", "h2", "h3", "h4", "h5", "h6"]) {
                const h = document.createElement(tag);
                h.textContent = "x";
                contents.appendChild(h);
                out[tag] = parseFloat(getComputedStyle(h).marginBottom);
            }
            return out;
        });
        for (const [tag, mb] of Object.entries(margins)) {
            expect(mb, `${tag} bottom margin should give breathing room`).toBeGreaterThanOrEqual(7);
        }
    } finally {
        await ctx.close();
    }
});
