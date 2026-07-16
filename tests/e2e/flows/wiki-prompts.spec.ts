// wiki-prompts.spec.ts — the multi-step wiki flow from the real Slack
// incident (2026-06-13: "add everything from the recipe in the grey
// hawk wiki to our grocery list"), run against the REPO's actual
// prompts (realPrompts) rather than the minimal test prompt. This is
// the regression guard for wiki tool-calling under the production
// prompt content, and for the tool-tag leak fix.
//
// Model-gated like the other chat specs: skips without a live model.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, TestUser } from "../fixtures/user";

const MODEL_URL = process.env.FAMILIAR_TEST_CHAT_MODEL_URL || "http://127.0.0.1:8090";
const TURN_TIMEOUT = 120_000;

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
            // realPrompts: load prompts/system_prompt.md + prompts/tiers/*.
            const stack = await start({ admin: true, chatModelURL: MODEL_URL, realPrompts: true });
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

test.describe.configure({ mode: "serial" });
test.beforeEach(async () => {
    test.skip(!(await modelIsUp()), `no inference server at ${MODEL_URL} — wiki-prompt specs need a live model`);
});

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

const INGREDIENTS = ["flour", "buttermilk", "sausage", "butter"];
const RECIPE =
    "# Biscuits and Gravy\n\n## Ingredients\n" +
    "- 2 cups flour\n- 1 cup buttermilk\n- 1 lb sausage\n- 4 tbsp butter\n\n" +
    "## Steps\nMake biscuits, make gravy, combine.\n";

async function pageContent(request: any, stack: GatewayStack, user: TestUser, slug: string, pageID: string) {
    const resp = await request.get(
        `${stack.workspaceURL}/console/api/books/${slug}/page-by-id/${pageID}`,
        { headers: authed(user) },
    );
    expect(resp.ok()).toBeTruthy();
    return ((await resp.json()).content as string);
}

test("the recipe → grocery list wiki flow works under the real prompts", async ({
    stack,
    request,
}) => {
    // Personal book (stable "personal" slug per user) — the shared
    // test DB has a global UNIQUE slug namespace and each run is a
    // fresh user, so a named shared book ("family-wiki") can't get
    // a deterministic slug. The personal book is per-user and the
    // real tool_policy names it; page slugs derive from titles.
    const user = await createTestUser();
    const headers = authed(user);

    const recipe = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Biscuits and Gravy", content: RECIPE },
        })
    ).json();
    const grocery = await (
        await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers,
            data: { title: "Grocery List", content: "# Grocery List\n\n- paper towels\n" },
        })
    ).json();

    const resp = await request.post(`${stack.workspaceURL}/api/chat`, {
        headers: { ...headers, Accept: "text/event-stream" },
        data: {
            message:
                "I want to make biscuits and gravy tonight. Add every ingredient from my " +
                "Biscuits and Gravy note to my Grocery List note. " +
                "Do it now with your tools, then tell me what you added.",
        },
        timeout: TURN_TIMEOUT,
    });
    expect(resp.ok(), `chat: HTTP ${resp.status()}`).toBeTruthy();
    const body = await resp.text();
    expect(body).toContain("event: done");
    expect(body).not.toContain("event: error");
    // The tool-tag leak fix: no protocol markup in the reply stream.
    expect(body).not.toContain("</tool_call>");
    expect(body).not.toContain("<tool_call>");

    // Outcome: the grocery list gained the ingredients, kept its
    // existing item, and the recipe was read (not rewritten).
    const groceryNow = (await pageContent(request, stack, user, "personal", grocery.id)).toLowerCase();
    for (const ing of INGREDIENTS) {
        expect(groceryNow, `grocery list should mention ${ing}`).toContain(ing);
    }
    expect(groceryNow, "append must not clobber existing items").toContain("paper towels");
    const recipeNow = await pageContent(request, stack, user, "personal", recipe.id);
    expect(recipeNow).toContain("Make biscuits, make gravy, combine.");
});
