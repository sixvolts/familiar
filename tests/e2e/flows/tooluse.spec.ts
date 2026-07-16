// tooluse.spec.ts — multi-step tool use against a real model: the
// TESTING-MAINFRAME.md headline scenario ("read this recipe and put
// the ingredients on my grocery list"). This is the deepest E2E in
// the suite — one chat turn has to drive the whole agentic stack:
//
//   classifier fallback (deep_reasoning → tools injected)
//     → OpenAI-style tool schemas on /v1/chat/completions
//     → model emits tool_calls (llama-server --jinja)
//     → pipeline tool loop (≤5 iterations) executes wiki skills
//       scoped to the session user
//     → list_books / read_page → append_to_page on a DIFFERENT page
//     → __TOOL_EFFECT__:note_changed status frame
//     → final assistant summary streams back
//
// The assertions check OUTCOMES (the grocery page gained the
// ingredients; the recipe survived untouched), not the exact tool
// sequence — models vary their route (search_pages vs list_pages),
// and pinning the path would make the spec flake on model updates.
//
// Skips without a live inference server, same as chat.spec.ts.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

const MODEL_URL = process.env.FAMILIAR_TEST_CHAT_MODEL_URL || "http://127.0.0.1:8090";
// A 5-iteration tool loop with several model round-trips: give it
// room. Real runs on mainframe finish in well under a minute.
const TURN_TIMEOUT = 240_000;

const INGREDIENTS = ["flour", "milk", "eggs", "butter", "baking powder"];

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
    test.skip(!(await modelIsUp()), `no inference server at ${MODEL_URL} — tool-use specs need a live model`);
});

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

interface Seeded {
    user: TestUser;
    recipeID: string;
    groceryID: string;
}

// seedKitchen creates the two personal-book pages the scenario needs:
// a recipe holding the ingredients, and a grocery list with one
// pre-existing item (so "append" vs "overwrite" is distinguishable).
async function seedKitchen(request: any, stack: GatewayStack): Promise<Seeded> {
    const user = await createTestUser();
    const mk = async (title: string, content: string) => {
        const resp = await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
            headers: authed(user),
            data: { title, content },
        });
        expect(resp.ok(), `seed "${title}": HTTP ${resp.status()}`).toBeTruthy();
        return (await resp.json()).id as string;
    };
    const recipeID = await mk(
        "Pancake Recipe",
        [
            "# Pancake Recipe",
            "",
            "Ingredients:",
            "- 2 cups flour",
            "- 1.5 cups milk",
            "- 2 eggs",
            "- 4 tbsp butter",
            "- 1 tbsp baking powder",
            "",
            "Mix and fry.",
        ].join("\n"),
    );
    const groceryID = await mk("Grocery List", "# Grocery List\n\n- candles\n");
    return { user, recipeID, groceryID };
}

async function pageContent(request: any, stack: GatewayStack, user: TestUser, id: string): Promise<string> {
    const resp = await request.get(
        `${stack.workspaceURL}/console/api/books/personal/page-by-id/${id}`,
        { headers: authed(user) },
    );
    expect(resp.ok()).toBeTruthy();
    return (await resp.json()).content as string;
}

const PROMPT =
    'Read my note titled "Pancake Recipe" and add every ingredient from it to my note titled ' +
    '"Grocery List". Add the ingredients now using your tools, then tell me what you added.';

test("one chat turn reads the recipe and writes the grocery list (API)", async ({ stack, request }) => {
    const { user, recipeID, groceryID } = await seedKitchen(request, stack);

    const resp = await request.post(`${stack.workspaceURL}/api/chat`, {
        headers: { ...authed(user), Accept: "text/event-stream" },
        data: { message: PROMPT },
        timeout: TURN_TIMEOUT,
    });
    expect(resp.ok(), `chat: HTTP ${resp.status()}`).toBeTruthy();
    const body = await resp.text();
    expect(body).toContain("event: done");
    expect(body).not.toContain("event: error");

    // The pipeline announced a successful page mutation mid-turn —
    // this is the frame the notes panel uses to live-refresh.
    expect(body, "expected a note_changed tool effect in the stream").toContain(
        "__TOOL_EFFECT__:note_changed",
    );

    // Outcome 1: the grocery list gained the ingredients (wording is
    // the model's; the ingredient nouns are not negotiable) and kept
    // its pre-existing item.
    const grocery = (await pageContent(request, stack, user, groceryID)).toLowerCase();
    for (const ing of INGREDIENTS) {
        expect(grocery, `grocery list should mention ${ing}`).toContain(ing);
    }
    expect(grocery, "append must not clobber existing items").toContain("candles");

    // Outcome 2: the recipe was read, not rewritten.
    const recipe = await pageContent(request, stack, user, recipeID);
    expect(recipe).toContain("Mix and fry.");
    expect(recipe).toContain("2 cups flour");
});

test("the same flow through the chat surface, with the notes panel live-refreshing", async ({
    stack,
    browser,
    request,
}) => {
    const { user, groceryID } = await seedKitchen(request, stack);

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Open the grocery list in a notes tab FIRST — after the chat
        // turn appends to it, the __TOOL_EFFECT__ → familiar:notesChanged
        // path must refresh this very editor without a reload.
        await page.locator(".sidebar-cat-notes").click();
        const notesShell = page.locator(".notes-shell").first();
        await expect(notesShell).toBeVisible({ timeout: 10_000 });
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id } }));
        }, groceryID);
        const editor = notesShell.locator(".toastui-editor-ww-container .ProseMirror").first();
        await expect(editor).toContainText("candles", { timeout: 10_000 });

        // Now run the chat turn in a second tab of the same session.
        const chatPage = await ctx.newPage();
        await chatPage.goto(stack.workspaceURL);
        await expect(chatPage.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await chatPage.locator(".sidebar-cat-chat").click();
        const chatShell = chatPage.locator(".chat-shell").first();
        await expect(chatShell).toBeVisible({ timeout: 10_000 });
        await chatPage.evaluate(() => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "chat" } }));
        });
        const input = chatShell.locator(".chat-input");
        await expect(input).toBeVisible({ timeout: 10_000 });
        await input.fill(PROMPT);
        await chatShell.locator(".chat-send-btn").click();

        // The assistant's final summary lands after the tool loop.
        await expect(chatShell.locator(".chat-msg-assistant").last()).toContainText(/flour|ingredient/i, {
            timeout: TURN_TIMEOUT,
        });

        // The grocery list editor in the FIRST tab caught the change
        // via SSE — no reload, no manual refresh.
        await expect(editor).toContainText("flour", { timeout: 30_000 });
        await expect(editor).toContainText("candles");

        // And the server row agrees.
        const grocery = (await pageContent(request, stack, user, groceryID)).toLowerCase();
        for (const ing of INGREDIENTS) {
            expect(grocery, `grocery list should mention ${ing}`).toContain(ing);
        }
    } finally {
        await ctx.close();
    }
});
