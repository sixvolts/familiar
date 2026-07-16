// share-public.spec.ts — public-link sharing and the host gate
// (TESTING-PLAN.md §"Phase 2" item 3). /p/{key} must render on the
// configured public host and 404 everywhere else — the gate is what
// keeps Tailscale-direct / internal hostnames from serving shared
// pages. The gate reads the inbound Host header, so the spec spoofs
// Host on requests to the same listener instead of needing DNS.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

const PUBLIC_HOST = "share.e2e.test";

const test = base.extend<{}, { stack: GatewayStack }>({
    stack: [
        async ({}, use) => {
            const stack = await start({ admin: true, publicHosts: [PUBLIC_HOST] });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

test.describe.configure({ mode: "serial" });

async function createSharedNote(request: any, stack: GatewayStack, user: TestUser) {
    const created = await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
        headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
        data: { title: "Public Note", content: "# Shared heading\n\nshared-marker-content" },
    });
    expect(created.ok(), `create note: HTTP ${created.status()}`).toBeTruthy();
    const note = await created.json();

    const toggled = await request.post(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/share`,
        {
            headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
            data: { enabled: true },
        },
    );
    expect(toggled.ok(), `share toggle: HTTP ${toggled.status()}`).toBeTruthy();
    const share = await toggled.json();
    expect(share.enabled).toBe(true);
    expect(share.share_key).toMatch(/^[A-Za-z0-9]{16}$/);
    expect(share.public_url).toBe(`http://${PUBLIC_HOST}/p/${share.share_key}`);
    return { note, share };
}

test("a shared page renders on the public host only", async ({ stack, request }) => {
    const user = await createTestUser();
    const { share } = await createSharedNote(request, stack, user);
    const url = `${stack.workspaceURL}/p/${share.share_key}`;

    // Public host → renders, anonymously, with the lockdown headers.
    const pub = await request.get(url, { headers: { Host: PUBLIC_HOST } });
    expect(pub.status(), "public host should render the share").toBe(200);
    const html = await pub.text();
    expect(html).toContain("Shared heading");
    expect(html).toContain("shared-marker-content");
    const headers = pub.headers();
    expect(headers["content-security-policy"]).toContain("default-src 'none'");
    expect(headers["x-robots-tag"]).toContain("noindex");
    expect(headers["referrer-policy"]).toBe("no-referrer");

    // Same key on the internal host (the workspace's own hostname) →
    // 404. This is the host gate, not auth: the page exists, the
    // hostname is wrong.
    const internal = await request.get(url);
    expect(internal.status(), "non-public host must refuse the share").toBe(404);
});

test("unknown and disabled share keys 404 on the public host", async ({ stack, request }) => {
    const user = await createTestUser();
    const { note, share } = await createSharedNote(request, stack, user);

    // Well-formed but nonexistent key.
    const bogus = await request.get(`${stack.workspaceURL}/p/AAAAbbbbCCCCdddd`, {
        headers: { Host: PUBLIC_HOST },
    });
    expect(bogus.status()).toBe(404);

    // Disable the share → the old key dies immediately.
    const off = await request.post(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/share`,
        {
            headers: { Cookie: user.cookieHeader, "Content-Type": "application/json" },
            data: { enabled: false },
        },
    );
    expect(off.ok()).toBeTruthy();
    const dead = await request.get(`${stack.workspaceURL}/p/${share.share_key}`, {
        headers: { Host: PUBLIC_HOST },
    });
    expect(dead.status(), "disabled share must 404").toBe(404);
});

test("the public render carries no session-bearing markup", async ({ stack, request }) => {
    const user = await createTestUser();
    const { share } = await createSharedNote(request, stack, user);

    // Anonymous fetch — no cookie at all — still renders (public means
    // public), and the response sets no cookies of its own.
    const pub = await request.get(`${stack.workspaceURL}/p/${share.share_key}`, {
        headers: { Host: PUBLIC_HOST },
    });
    expect(pub.status()).toBe(200);
    expect(pub.headers()["set-cookie"]).toBeUndefined();
    const html = await pub.text();
    // The standalone template must not pull in the console app (which
    // would leak app structure to anonymous readers).
    expect(html).not.toContain("app.js");
    expect(html).not.toContain("/console/api");
});

test("shared pages serve their images anonymously with #w= widths applied", async ({
    stack,
    request,
    browser,
}) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };
    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Pic Share", content: "placeholder" },
        })
    ).json();

    // Generate a real PNG in a browser (no Node image deps) and
    // attach it to the page.
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.goto(stack.workspaceURL);
    const dataURL = await page.evaluate(() => {
        const c = document.createElement("canvas");
        c.width = 640; c.height = 480;
        c.getContext("2d")!.fillRect(0, 0, 640, 480);
        return c.toDataURL("image/png");
    });
    await ctx.close();
    const png = Buffer.from(dataURL.split(",")[1], "base64");
    const up = await request.post(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/media`,
        { headers: { Cookie: user.cookieHeader }, multipart: { file: { name: "p.png", mimeType: "image/png", buffer: png } } },
    );
    const meta = await up.json();
    await request.patch(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
        headers,
        data: { content: `# Pic\n\n![pic](${meta.url}#w=50)\n` },
    });
    const share = await (
        await request.post(
            `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/share`,
            { headers, data: { enabled: true } },
        )
    ).json();

    // The public render rewrites the image to the share-scoped proxy
    // and applies the fragment width inline.
    const pub = await request.get(`${stack.workspaceURL}/p/${share.share_key}`, {
        headers: { Host: PUBLIC_HOST },
    });
    expect(pub.status()).toBe(200);
    const html = await pub.text();
    expect(html).toContain(`/p/${share.share_key}/media/${meta.id}`);
    expect(html).toContain("width:50%");
    expect(html).not.toContain("/console/api/media/");

    // The proxied bytes serve ANONYMOUSLY on the public host…
    const img = await request.get(
        `${stack.workspaceURL}/p/${share.share_key}/media/${meta.id}`,
        { headers: { Host: PUBLIC_HOST } },
    );
    expect(img.status()).toBe(200);
    expect(img.headers()["content-type"]).toBe("image/png");
    expect((await img.body()).length).toBe(png.length);

    // …but not on the internal host, not with a foreign media id,
    // and not after the share dies.
    expect(
        (await request.get(`${stack.workspaceURL}/p/${share.share_key}/media/${meta.id}`)).status(),
    ).toBe(404);
    const other = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Other", content: "x" },
        })
    ).json();
    const up2 = await request.post(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${other.id}/media`,
        { headers: { Cookie: user.cookieHeader }, multipart: { file: { name: "q.png", mimeType: "image/png", buffer: png } } },
    );
    const foreign = await up2.json();
    expect(
        (
            await request.get(`${stack.workspaceURL}/p/${share.share_key}/media/${foreign.id}`, {
                headers: { Host: PUBLIC_HOST },
            })
        ).status(),
        "a share key must not unlock another page's media",
    ).toBe(404);
    await request.post(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/share`,
        { headers, data: { enabled: false } },
    );
    expect(
        (
            await request.get(`${stack.workspaceURL}/p/${share.share_key}/media/${meta.id}`, {
                headers: { Host: PUBLIC_HOST },
            })
        ).status(),
    ).toBe(404);
});

// ── Wiki-book sharing (the personal-book path above is "notes"; this
// covers a real shared book: writer-gated authz + the public render). ──

function H(u: TestUser) {
    return { Cookie: u.cookieHeader, "Content-Type": "application/json" };
}

test("a wiki-book page shares + renders publicly; non-writers can't share", async ({ stack, request }) => {
    const owner = await createTestUser();
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers: H(owner),
            data: { name: `Shared Wiki ${Date.now().toString(36)}` },
        })
    ).json();
    const page = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers: H(owner),
            data: { title: "Public Wiki Page", content: "# Wiki heading\n\nwiki-shared-marker" },
        })
    ).json();
    const shareURL = `${stack.workspaceURL}/console/api/books/${book.slug}/page-by-id/${page.id}/share`;

    // A reader member must NOT be able to share (requirePageWrite) — this
    // is the authz difference from notes, where you own your personal book.
    const reader = await createTestUser();
    const add = await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/members`, {
        headers: H(owner),
        data: { user_id: reader.id, role: "reader" },
    });
    expect(add.ok(), `add reader: HTTP ${add.status()}`).toBeTruthy();
    const readerToggle = await request.post(shareURL, { headers: H(reader), data: { enabled: true } });
    expect(readerToggle.status(), "reader must not be able to share").toBe(403);
    // A non-member can't even see the book/page.
    const outsider = await createTestUser();
    expect([403, 404]).toContain(
        (await request.post(shareURL, { headers: H(outsider), data: { enabled: true } })).status(),
    );

    // The owner (writer) shares it.
    const toggled = await request.post(shareURL, { headers: H(owner), data: { enabled: true } });
    expect(toggled.ok(), `owner share: HTTP ${toggled.status()}`).toBeTruthy();
    const share = await toggled.json();
    expect(share.enabled).toBe(true);
    expect(share.public_url).toBe(`http://${PUBLIC_HOST}/p/${share.share_key}`);

    // It renders on the public host, and the host gate still applies.
    const pub = await request.get(`${stack.workspaceURL}/p/${share.share_key}`, { headers: { Host: PUBLIC_HOST } });
    expect(pub.status()).toBe(200);
    expect(await pub.text()).toContain("wiki-shared-marker");
    expect((await request.get(`${stack.workspaceURL}/p/${share.share_key}`)).status()).toBe(404);
});

test("the wiki ⋯ menu shares a page and surfaces the globe indicator", async ({ stack, browser, request }) => {
    const owner = await createTestUser();
    const book = await (
        await request.post(`${stack.workspaceURL}/console/api/books`, {
            headers: H(owner),
            data: { name: `UI Share Wiki ${Date.now().toString(36)}` },
        })
    ).json();
    const page = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${book.slug}/pages`, {
            headers: H(owner),
            data: { title: "Share me", content: "# Hi\n\nbody text" },
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, owner);
    const p = await ctx.newPage();
    try {
        await p.goto(stack.workspaceURL);
        await expect(p.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await p.evaluate(({ slug, id }) => {
            const ws: any = (window as any).FamiliarWorkspace;
            ws.openDoc("wiki", slug, "Share me", { pageId: id });
        }, { slug: book.slug, id: page.id });

        // No globe yet; share from the ⋯ menu.
        await expect(p.locator(".notes-share-indicator:visible")).toHaveCount(0);
        await p.locator(".notes-overflow-btn:visible").click();
        await p.locator(".notes-overflow.is-open .notes-overflow-item", { hasText: "Share publicly" }).click();

        // Globe appears, and re-opening the menu now offers "Stop sharing".
        await expect(p.locator(".notes-share-indicator:visible")).toBeVisible({ timeout: 10_000 });
        await p.locator(".notes-overflow-btn:visible").click();
        await expect(
            p.locator(".notes-overflow.is-open .notes-overflow-item", { hasText: "Stop sharing publicly" }),
        ).toBeVisible();

        // Backend agrees: the page now has a share the public host serves.
        const pg = await (
            await request.get(`${stack.workspaceURL}/console/api/books/${book.slug}/pages/${page.slug}`, { headers: H(owner) })
        ).json();
        expect(pg.share, "page-by-slug GET reports the share").toBeTruthy();
        const pub = await request.get(`${stack.workspaceURL}/p/${pg.share.share_key}`, { headers: { Host: PUBLIC_HOST } });
        expect(pub.status()).toBe(200);
        expect(await pub.text()).toContain("body text");
    } finally {
        await ctx.close();
    }
});

test("shared pages serve diagrams as pre-rendered PNGs; the page stays script-free", async ({
    stack,
    request,
    browser,
}) => {
    const user = await createTestUser();
    const headers = { Cookie: user.cookieHeader, "Content-Type": "application/json" };

    const note = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: {
                title: "Diagram Share",
                content: "# Flow\n\n```mermaid\ngraph TD;\n  A[Shared] --> B[Diagram];\n```\n",
            },
        })
    ).json();
    // The owner shares the note from the ⋯ menu — the toggle
    // trigger rasterizes the fence and uploads mermaid-<hash>.png
    // immediately, so the share works without a later save.
    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    let shareKey = "";
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes .sidebar-row-chevron").click();
        await page
            .locator('.sidebar-children[data-category="notes"] a.sidebar-child', { hasText: "Diagram Share" })
            .click();
        await expect(page.locator(".mermaid-block.is-ww.is-rendered")).toBeVisible({ timeout: 10_000 });
        await page.locator(".notes-overflow-btn:visible").click();
        await page
            .locator(".notes-overflow.is-open .notes-overflow-item", { hasText: "Share publicly" })
            .click();

        // The render lands in the page's media set.
        await expect
            .poll(async () => {
                const l = await (
                    await request.get(
                        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/media`,
                        { headers },
                    )
                ).json();
                return (l.items ?? []).some((m: any) => /^mermaid-[0-9a-f]{12}\.png$/.test(m.filename));
            }, { timeout: 20_000 })
            .toBe(true);
    } finally {
        await ctx.close();
    }

    const shareState = await (
        await request.post(
            `${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}/share`,
            { headers, data: { enabled: true } },
        )
    ).json();
    shareKey = shareState.share_key;

    // Without any render the fence would fall back to a code block;
    // with the pre-render in place the share serves a share-scoped
    // PNG — still zero script — and the bytes load anonymously.
    const after = await request.get(`${stack.workspaceURL}/p/${shareKey}`, {
        headers: { Host: PUBLIC_HOST },
    });
    const html = await after.text();
    expect(html).not.toContain("<script");
    const m = new RegExp(`/p/${shareKey}/media/([0-9a-f-]+)`).exec(html);
    expect(m, "share should embed the pre-rendered diagram PNG").toBeTruthy();
    const img = await request.get(`${stack.workspaceURL}/p/${shareKey}/media/${m![1]}`, {
        headers: { Host: PUBLIC_HOST },
    });
    expect(img.status()).toBe(200);
    expect(img.headers()["content-type"]).toBe("image/png");
    expect((await img.body()).length).toBeGreaterThan(500);
});
