// scheduled-wiki.spec.ts — a scheduled action, running inside a shard
// envelope scoped to a single wiki book, prunes a grocery list under the
// real model. This is the end-to-end of the "household maintenance"
// pattern: a shard with a wiki-only tool allowlist and book_access fixed
// to one book, driven by the scheduled-actions runner.
//
// The prompt is the real one from the user: remove bought (checked)
// items, keep the unbought ones, and when a section empties leave a
// single blank checkbox behind — all silently, by editing the page.
//
// Split of concerns, matching scheduled.spec.ts: the HARD book-scope
// confinement guarantee is owned by the Go unit tests
// (internal/skills/wiki — resolveBook denies out-of-scope; internal/
// pipeline — BookAccess reaches the skill SessionContext). This E2E
// owns the real-model behavior: does the run actually prune the list the
// way the prompt asks, and does a scoped run stay in its own book.
//
// Model-gated like the other chat specs: skips without a live model.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, TestUser } from "../fixtures/user";

const MODEL_URL = process.env.FAMILIAR_TEST_CHAT_MODEL_URL || "http://127.0.0.1:8090";
const RUN_TIMEOUT = 120_000;

async function modelIsUp(): Promise<boolean> {
    try {
        const resp = await fetch(`${MODEL_URL}/health`, { signal: AbortSignal.timeout(2_000) });
        return resp.ok;
    } catch {
        return false;
    }
}

const test = base.extend<{}, { stack: GatewayStack }>({
    stack: [
        async ({}, use) => {
            const stack = await start({ admin: true, chatModelURL: MODEL_URL });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

test.describe.configure({ mode: "serial" });

test.beforeEach(async () => {
    test.skip(!(await modelIsUp()), `no inference server at ${MODEL_URL} — scheduled-wiki specs need a live model`);
});

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

// The shard's own system prompt is the only system message a shard run
// sees (the layered trusted prompt is replaced), so it carries all the
// tool guidance and the exact checkbox semantics the task depends on.
const SHARD_SYSTEM_PROMPT = [
    "You maintain a household wiki. Your wiki tools are scoped to a single book.",
    "Find the book with list_books, then read the relevant page with read_page before editing.",
    "Grocery items are markdown task-list lines: '- [x] name' means the item was bought; '- [ ] name' means it is still needed.",
    "When asked to tidy the grocery list:",
    "  - Remove every checked ('- [x]') line entirely.",
    "  - Keep every unchecked ('- [ ]') line exactly as written.",
    "  - Never change or drop a section heading (lines starting with '#').",
    "  - If removing checked lines leaves a section with no items, add ONE empty unchecked line '- [ ]' under that heading so items are easy to add later.",
    "Write the result back with update_page (the full new markdown body).",
    "Act immediately and silently: do not ask questions and do not post any summary, message, or extra page. Just edit the list.",
].join("\n");

// The grocery list the action operates on. Designed so the outcome is
// checkable: two checked items to remove in a section that survives,
// one checked item that empties its whole section, and unchecked items
// that must stay.
const GROCERY_LIST = [
    "# Grocery List",
    "",
    "## Produce",
    "- [x] apples",
    "- [ ] bananas",
    "- [x] carrots",
    "",
    "## Dairy",
    "- [x] whole milk",
    "",
    "## Pantry",
    "- [ ] olive oil",
    "",
].join("\n");

// The exact prompt the user runs the action with.
const ACTION_PROMPT = [
    "In the Family Wiki, Check the Grocery List page. If there are checked items in the list,",
    "that means we successfully bought that item. Remove the Item from the list. If you remove the",
    "last item for a section, make sure you put an empty item/checkbox in that section to make it",
    "easier to add items to the list.",
    "",
    "You don't need to inform me of any changes through a message or page, just edit the list.",
].join(" ");

async function createBook(request: any, stack: GatewayStack, user: TestUser, name: string, slug: string) {
    const resp = await request.post(`${stack.workspaceURL}/console/api/books`, {
        headers: authed(user),
        data: { name, slug },
    });
    expect(resp.ok(), `create book ${slug}: HTTP ${resp.status()}`).toBeTruthy();
    return resp.json();
}

async function createPage(
    request: any,
    stack: GatewayStack,
    user: TestUser,
    bookSlug: string,
    title: string,
    content: string,
) {
    const resp = await request.post(`${stack.workspaceURL}/console/api/books/${bookSlug}/pages`, {
        headers: authed(user),
        data: { title, content },
    });
    expect(resp.ok(), `create page ${title}: HTTP ${resp.status()}`).toBeTruthy();
    return resp.json();
}

async function pageContent(request: any, stack: GatewayStack, user: TestUser, bookSlug: string, pageID: string) {
    const resp = await request.get(
        `${stack.workspaceURL}/console/api/books/${bookSlug}/page-by-id/${pageID}`,
        { headers: authed(user) },
    );
    expect(resp.ok(), `read page: HTTP ${resp.status()}`).toBeTruthy();
    return resp.json();
}

// pollRun polls the run ledger until the run leaves "running".
async function pollRun(request: any, stack: GatewayStack, user: TestUser, actionID: string, runID: string) {
    const deadline = Date.now() + RUN_TIMEOUT;
    while (Date.now() < deadline) {
        const runs = await (
            await request.get(`${stack.workspaceURL}/console/api/actions/${actionID}/runs`, {
                headers: authed(user),
            })
        ).json();
        const run = (runs.items ?? []).find((r: any) => r.id === runID);
        if (run && run.status !== "running") return run;
        await new Promise((r) => setTimeout(r, 500));
    }
    throw new Error("run never finished");
}

test("a wiki-scoped shard action prunes the grocery list and leaves emptied sections a blank checkbox", async ({
    stack,
    request,
}) => {
    const user = await createTestUser();

    // Two books with a unique slug suffix (the shared test DB has a
    // global UNIQUE slug namespace). The shard is confined to the Grey
    // Hawk book; the Firehouse book is the "must not touch" control.
    const suffix = Date.now().toString(36);
    const familyWiki = await createBook(request, stack, user, "Family Wiki", `family-wiki-${suffix}`);
    const firehouse = await createBook(request, stack, user, "Firehouse Wiki", `firehouse-${suffix}`);

    const grocery = await createPage(request, stack, user, familyWiki.slug, "Grocery List", GROCERY_LIST);
    const offLimits = await createPage(
        request,
        stack,
        user,
        firehouse.slug,
        "Roster",
        "# Roster\n\n- Engine 1\n- Engine 2\n",
    );

    // The shard: wiki tools only, book_access pinned to the Family
    // book. Persistent + isolated mirrors a real household-maintenance
    // shard.
    const shardID = `grocery-${suffix}`;
    const shardResp = await request.post(`${stack.workspaceURL}/console/api/shards`, {
        headers: authed(user),
        data: {
            id: shardID,
            name: "Grocery Keeper",
            description: "Tidies the Family grocery list",
            persistence: "persistent",
            visibility: "isolated",
            scope_tag: `shard:${shardID}`,
            system_prompt: SHARD_SYSTEM_PROMPT,
            tool_allowlist: ["list_books", "list_pages", "read_page", "update_page", "patch_page"],
            book_access: [familyWiki.id],
        },
    });
    expect(shardResp.status(), await shardResp.text()).toBe(201);

    // The scheduled action: the user's real prompt, run inside the shard
    // envelope. report_targets is a log sink — the prompt forbids any
    // message/page report, so the only observable effect is the edit.
    const created = await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers: authed(user),
        data: {
            name: "tidy grocery list",
            prompt: ACTION_PROMPT,
            cron: "0 7 * * *",
            shard_id: shardID,
            report_targets: [{ kind: "log" }],
        },
    });
    expect(created.status(), await created.text()).toBe(201);
    const action = await created.json();
    expect(action.shard_id).toBe(shardID);
    expect(action.envelope).toBe("shard");

    // Fire it now and wait for the run to land.
    const fired = await request.post(`${stack.workspaceURL}/console/api/actions/${action.id}/run`, {
        headers: authed(user),
    });
    expect(fired.status()).toBe(202);
    const { run_id } = await fired.json();
    const run = await pollRun(request, stack, user, action.id, run_id);
    expect(run.status, `run failed: ${run.error}`).toBe("ok");

    // The grocery list, after the run.
    const after = (await pageContent(request, stack, user, familyWiki.slug, grocery.id)).content as string;
    const lower = after.toLowerCase();

    // Bought (checked) items are gone.
    expect(lower, "checked 'apples' should be removed").not.toContain("apples");
    expect(lower, "checked 'carrots' should be removed").not.toContain("carrots");
    expect(lower, "checked 'whole milk' should be removed").not.toContain("milk");

    // Still-needed (unchecked) items remain.
    expect(lower, "unchecked 'bananas' must stay").toContain("bananas");
    expect(lower, "unchecked 'olive oil' must stay").toContain("olive oil");

    // The headings survive — including Dairy, whose only item was bought.
    expect(after, "Produce heading must survive").toMatch(/^##\s+Produce/m);
    expect(after, "Dairy heading must survive even when emptied").toMatch(/^##\s+Dairy/m);
    expect(after, "Pantry heading must survive").toMatch(/^##\s+Pantry/m);

    // The emptied Dairy section gets a blank checkbox so it's easy to add
    // to later. Look for an empty unchecked item (no text after the box)
    // somewhere in the body — bananas/olive oil are unchecked-WITH-text,
    // so a bare "- [ ]" can only be the placeholder.
    expect(after, "emptied section should gain a blank checkbox").toMatch(/-\s+\[\s\]\s*(\n|$)/);

    // Confinement sanity: the run stayed in its own book — the Firehouse
    // page the shard had no access to is byte-for-byte unchanged. (The
    // hard "cannot reach it even if it tries" guarantee is unit-tested in
    // internal/skills/wiki.)
    const firehouseAfter = await pageContent(request, stack, user, firehouse.slug, offLimits.id);
    expect(firehouseAfter.content).toBe("# Roster\n\n- Engine 1\n- Engine 2\n");
});
