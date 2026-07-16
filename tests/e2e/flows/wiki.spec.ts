// wiki.spec.ts — books, membership roles, pages, revisions
// (TESTING-PLAN.md §"Phase 3", pulled forward while we're building
// out the suite). API-level through the workspace proxy: the wiki's
// regression surface is authorization and slug semantics, which
// assert crisply over HTTP.
//
// Worth singling out, because both have bitten before:
//   - Slug reuse after soft-delete. wiki_pages once had a full-table
//     UNIQUE (book_id, slug) and re-creating a page whose slug had
//     ever existed 23505'd; the partial index (deleted_at IS NULL)
//     fixed it. This spec pins that.
//   - Cross-book page-id smuggling: page-by-id routes filter by
//     (book_id, id), so a page can't be reached through a book the
//     caller doesn't belong to, even with a valid page UUID.

import { test as base, expect, APIRequestContext } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, TestUser } from "../fixtures/user";

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

async function createBook(api: APIRequestContext, stack: GatewayStack, owner: TestUser, slug: string) {
    const resp = await api.post(`${stack.workspaceURL}/console/api/books`, {
        headers: authed(owner),
        data: { name: `Book ${slug}`, slug, description: "e2e book" },
    });
    expect(resp.ok(), `create book: HTTP ${resp.status()}`).toBeTruthy();
    return resp.json();
}

async function createPage(
    api: APIRequestContext,
    stack: GatewayStack,
    user: TestUser,
    bookSlug: string,
    title: string,
    content: string,
) {
    const resp = await api.post(`${stack.workspaceURL}/console/api/books/${bookSlug}/pages`, {
        headers: authed(user),
        data: { title, content },
    });
    expect(resp.ok(), `create page "${title}": HTTP ${resp.status()}`).toBeTruthy();
    return resp.json();
}

test("membership roles gate reading, writing, and member management", async ({ stack, request }) => {
    const owner = await createTestUser();
    const writer = await createTestUser();
    const reader = await createTestUser();
    const outsider = await createTestUser();
    const slug = `roles-${Date.now().toString(36)}`;
    await createBook(request, stack, owner, slug);
    const page = await createPage(request, stack, owner, slug, "Role Target", "owner content");
    const bookURL = `${stack.workspaceURL}/console/api/books/${slug}`;

    // A non-member can't even see the book or its pages.
    expect([403, 404]).toContain((await request.get(bookURL, { headers: authed(outsider) })).status());
    expect([403, 404]).toContain(
        (await request.get(`${bookURL}/pages`, { headers: authed(outsider) })).status(),
    );

    // Owner grants roles: writer + reader.
    for (const [member, role] of [
        [writer, "writer"],
        [reader, "reader"],
    ] as const) {
        const add = await request.post(`${bookURL}/members`, {
            headers: authed(owner),
            data: { user_id: member.id, role },
        });
        expect(add.ok(), `add ${role}: HTTP ${add.status()}`).toBeTruthy();
    }

    // Writer: page CRUD yes, member management no.
    const writerEdit = await request.patch(`${bookURL}/page-by-id/${page.id}`, {
        headers: authed(writer),
        data: { title: "Role Target", content: "writer was here" },
    });
    expect(writerEdit.ok(), `writer edit: HTTP ${writerEdit.status()}`).toBeTruthy();
    expect(
        (
            await request.post(`${bookURL}/members`, {
                headers: authed(writer),
                data: { user_id: outsider.id, role: "writer" },
            })
        ).status(),
        "writers must not manage members",
    ).toBe(403);

    // Reader: read yes, write no.
    const readerGet = await request.get(`${bookURL}/page-by-id/${page.id}`, { headers: authed(reader) });
    expect(readerGet.ok()).toBeTruthy();
    expect((await readerGet.json()).content).toBe("writer was here");
    expect(
        (
            await request.patch(`${bookURL}/page-by-id/${page.id}`, {
                headers: authed(reader),
                data: { title: "Role Target", content: "reader sneaking a write" },
            })
        ).status(),
        "readers must not write",
    ).toBe(403);
    expect(
        (
            await request.post(`${bookURL}/pages`, {
                headers: authed(reader),
                data: { title: "Reader Page", content: "nope" },
            })
        ).status(),
    ).toBe(403);

    // Demoting the writer to reader takes effect immediately.
    const demote = await request.patch(`${bookURL}/members/${writer.id}`, {
        headers: authed(owner),
        data: { role: "reader" },
    });
    expect(demote.ok(), `demote: HTTP ${demote.status()}`).toBeTruthy();
    expect(
        (
            await request.patch(`${bookURL}/page-by-id/${page.id}`, {
                headers: authed(writer),
                data: { title: "Role Target", content: "demoted write" },
            })
        ).status(),
        "demoted writer must lose write access",
    ).toBe(403);
});

test("page edits accrue revisions that can be read back", async ({ stack, request }) => {
    const owner = await createTestUser();
    const slug = `rev-${Date.now().toString(36)}`;
    await createBook(request, stack, owner, slug);
    const page = await createPage(request, stack, owner, slug, "Versioned", "v1");
    const bookURL = `${stack.workspaceURL}/console/api/books/${slug}`;

    let current = page;
    for (const content of ["v2", "v3"]) {
        const resp = await request.patch(`${bookURL}/page-by-id/${page.id}`, {
            headers: { ...authed(owner), "If-Match": current.updated_at },
            data: { title: "Versioned", content },
        });
        expect(resp.ok(), `patch to ${content}: HTTP ${resp.status()}`).toBeTruthy();
        current = await resp.json();
    }

    const revs = await (
        await request.get(`${bookURL}/pages/${current.slug}/revisions`, { headers: authed(owner) })
    ).json();
    const items = revs.items ?? revs.revisions ?? revs;
    expect(Array.isArray(items)).toBeTruthy();
    expect(items.length, "two edits should leave at least two revisions").toBeGreaterThanOrEqual(2);

    // The newest revision's stored content is retrievable and matches
    // a state the page actually passed through.
    const rev = await (
        await request.get(`${bookURL}/pages/${current.slug}/revisions/${items[0].id}`, {
            headers: authed(owner),
        })
    ).json();
    expect(["v1", "v2", "v3"]).toContain(rev.content);
});

test("a soft-deleted page's slug is immediately reusable", async ({ stack, request }) => {
    const owner = await createTestUser();
    const slug = `slugreuse-${Date.now().toString(36)}`;
    await createBook(request, stack, owner, slug);
    const bookURL = `${stack.workspaceURL}/console/api/books/${slug}`;

    const first = await createPage(request, stack, owner, slug, "Phoenix Page", "first life");

    const del = await request.delete(`${bookURL}/page-by-id/${first.id}`, { headers: authed(owner) });
    expect(del.ok(), `delete: HTTP ${del.status()}`).toBeTruthy();

    // Same title → same derived slug. Under the old full-table unique
    // constraint this 23505'd; the partial index scopes uniqueness to
    // live rows only.
    const second = await createPage(request, stack, owner, slug, "Phoenix Page", "second life");
    expect(second.slug).toBe(first.slug);
    expect(second.id).not.toBe(first.id);

    // Only the reborn page is live.
    const list = await (await request.get(`${bookURL}/pages`, { headers: authed(owner) })).json();
    const matches = (list.items ?? []).filter((p: any) => p.slug === first.slug);
    expect(matches).toHaveLength(1);
    expect(matches[0].id).toBe(second.id);
});

test("a valid page id is unreachable through a book it doesn't belong to", async ({ stack, request }) => {
    const owner = await createTestUser();
    const slugA = `smug-a-${Date.now().toString(36)}`;
    const slugB = `smug-b-${Date.now().toString(36)}`;
    await createBook(request, stack, owner, slugA);
    await createBook(request, stack, owner, slugB);
    const page = await createPage(request, stack, owner, slugA, "Homed Page", "lives in A");

    // Same caller, full access to both books — the page id still must
    // not resolve through book B. (page-by-id filters by book_id+id;
    // without that, membership in ANY book would unlock every page.)
    const smuggled = await request.get(
        `${stack.workspaceURL}/console/api/books/${slugB}/page-by-id/${page.id}`,
        { headers: authed(owner) },
    );
    expect(smuggled.status()).toBe(404);

    // And mutation through the wrong book is refused the same way.
    const smuggledWrite = await request.patch(
        `${stack.workspaceURL}/console/api/books/${slugB}/page-by-id/${page.id}`,
        { headers: authed(owner), data: { title: "Homed Page", content: "rewritten via B" } },
    );
    expect(smuggledWrite.status()).toBe(404);
    const after = await (
        await request.get(`${stack.workspaceURL}/console/api/books/${slugA}/page-by-id/${page.id}`, {
            headers: authed(owner),
        })
    ).json();
    expect(after.content).toBe("lives in A");
});

test("a non-member cannot read or write another user's page by id", async ({ stack, request }) => {
    // The cross-USER counterpart to the smuggling test above: an
    // outsider who has somehow learned the book slug + page UUID (both
    // are guessable-ish identifiers, not secrets) must still be refused
    // by the page-by-id endpoints. Membership is the gate, not knowledge
    // of the id.
    const owner = await createTestUser();
    const intruder = await createTestUser();
    const slug = `xuser-${Date.now().toString(36)}`;
    await createBook(request, stack, owner, slug);
    const page = await createPage(request, stack, owner, slug, "Private Page", "owner-only secret");

    const read = await request.get(
        `${stack.workspaceURL}/console/api/books/${slug}/page-by-id/${page.id}`,
        { headers: authed(intruder) },
    );
    expect([403, 404], `read status was ${read.status()}`).toContain(read.status());

    const write = await request.patch(
        `${stack.workspaceURL}/console/api/books/${slug}/page-by-id/${page.id}`,
        { headers: authed(intruder), data: { title: "Private Page", content: "tampered by intruder" } },
    );
    expect([403, 404], `write status was ${write.status()}`).toContain(write.status());

    // The owner's content is untouched.
    const after = await (
        await request.get(`${stack.workspaceURL}/console/api/books/${slug}/page-by-id/${page.id}`, {
            headers: authed(owner),
        })
    ).json();
    expect(after.content).toBe("owner-only secret");
});
