// README screenshot generator. NOT part of the test suite — it runs via
// its own config (screenshot.config.ts) and is driven by CI
// (.github/workflows/screenshot.yml) on frontend changes, which commits
// the refreshed docs/screenshot.png back. Local builds stay pure Go.
// Manual run (needs Node): `npm run screenshot` from tests/e2e.
//
// It boots the real gateway+workspace stack, seeds believable demo data
// (no model needed), opens the split workspace (chat + a live note), and
// captures docs/screenshot.png at 2×. Edit the demo data below to change
// what the hero shows.

import { test as base, expect } from "@playwright/test";
import * as path from "node:path";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

// Output path: cwd is tests/e2e when run via screenshot.config.ts, so
// this resolves to <repo>/docs/screenshot.png.
const OUT = path.resolve("..", "..", "docs", "screenshot.png");

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

function authed(u: TestUser) {
    return { Cookie: u.cookieHeader, "Content-Type": "application/json" };
}

const KYOTO_REPLY = `Here's a 5-day Kyoto plan balancing temples, gardens, and food:

**Day 1 — Higashiyama (East)**
- Kiyomizu-dera right at opening to beat the crowds
- Wander Ninenzaka → Sannenzaka for matcha and sweets
- Dinner in Gion (kaiseki if you can book ahead)

**Day 2 — Arashiyama**
- Bamboo grove early, then the Tenryu-ji garden
- Lunch: *yudofu* (tofu hot pot) by the river

**Day 3 — Northern temples**
- Kinkaku-ji (Golden Pavilion), then Ryoan-ji's rock garden

Want me to save this to a note so you can check things off as you go?`;

const KYOTO_NOTE = `## Kyoto — 5 days

### Day 1 · Higashiyama
- [x] Kiyomizu-dera (opens 6:00)
- [ ] Tea stop on Ninenzaka
- [ ] Gion dinner reservation

### Day 2 · Arashiyama
- [ ] Bamboo grove (go early)
- [ ] Tenryu-ji garden
- [ ] Yudofu lunch

### Packing
- [ ] JR Pass + IC card
- [ ] Comfortable walking shoes
- [ ] Pocket wifi

> Pulled from the trip-planning chat — see the **Kyoto trip planning** thread.`;

const RUNBOOK = `# Home Lab runbook

A quick reference for the cluster.

## Restart order
1. Postgres
2. Embedder + sidecar
3. Gateway, then workspace

Watch \`journalctl -u familiar-gateway -f\` during a deploy.`;

test("@screenshot workspace README image", async ({ stack, browser, request }) => {
    const user = await createTestUser({ role: "admin", displayName: "Sam Rivera" });
    const H = authed(user);
    const post = (p: string, data: any) => request.post(`${stack.workspaceURL}${p}`, { headers: H, data });

    // Notes (personal book pages)
    const note = await (await post("/console/api/books/personal/pages", { title: "Kyoto itinerary", content: KYOTO_NOTE })).json();
    await post("/console/api/books/personal/pages", { title: "Reading list", content: "- Project Hail Mary\n- The Pragmatic Programmer" });
    await post("/console/api/books/personal/pages", { title: "Garden planting plan", content: "## Spring\n- Tomatoes\n- Basil" });

    // Wiki
    const book = await (await post("/console/api/books", { name: "Home Lab" })).json();
    await post(`/console/api/books/${book.slug}/pages`, { title: "Runbook", content: RUNBOOK });
    await post(`/console/api/books/${book.slug}/pages`, { title: "Network map", content: "## Subnets\n- mgmt 10.0.0.0/24" });

    // Conversations — the first is opened in the left pane; the rest fill the rail.
    const conv = await (await post("/console/api/conversations", { title: "Kyoto trip planning", model: "familiar" })).json();
    await post(`/console/api/conversations/${conv.id}/messages`, { role: "user", content: "Plan a 5-day Kyoto trip focused on temples, gardens, and food." });
    await post(`/console/api/conversations/${conv.id}/messages`, { role: "assistant", content: KYOTO_REPLY });
    await post("/console/api/conversations", { title: "Refactor the auth flow", model: "familiar" });
    await post("/console/api/conversations", { title: "Weekly review", model: "familiar" });

    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 });
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });

        // Split workspace: chat conversation on the left, the note on the right.
        await page.evaluate(({ convId, noteId }) => {
            const ws: any = (window as any).FamiliarWorkspace;
            if ((window as any).appSwitchPanel) (window as any).appSwitchPanel("workspace");
            ws.openDoc("chat", convId, "Kyoto trip planning");
            ws.openDoc("notes", noteId, "Kyoto itinerary");
        }, { convId: conv.id, noteId: note.id });

        // Expand the rail so the nav tree is populated.
        for (const cat of ["chat", "notes", "wiki"]) {
            const chev = page.locator(`.sidebar-cat-${cat} .sidebar-row-chevron`).first();
            if (await chev.count()) await chev.click().catch(() => {});
        }

        // Both panes filled (and markdown rendered, not raw) before capture.
        await expect(page.locator(".chat-messages").first()).toContainText("Kyoto", { timeout: 15_000 });
        await expect(page.locator(".chat-msg-assistant strong").first()).toBeVisible({ timeout: 10_000 });
        await page.waitForTimeout(1000);
        await page.screenshot({ path: OUT });
        console.log("[screenshot] wrote " + OUT);
    } finally {
        await ctx.close();
    }
});
