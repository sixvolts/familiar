// smoke.spec.ts — proves the fixture wiring. If this is red, no
// other E2E spec stands a chance. Two checks:
//
//   1. The gateway boots against FAMILIAR_TEST_DSN and answers
//      /api/health with {status:"ok"}.
//   2. The workspace boots, proxies to the gateway, and serves
//      the SPA index HTML at "/".
//
// Add real behavior-level specs in flows/auth.spec.ts etc. once
// the WebAuthn + user fixtures land (TESTING-PLAN.md §"Phase 1"
// step 4).

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";

const test = base.extend<{}, { stack: GatewayStack }>({
    // Worker-scoped: the stack boots once per Playwright worker and
    // every spec in the worker shares it. Stays cheap so long as
    // tests don't mutate cross-cutting global state (auth tests
    // will, and will move to a test-scoped fixture).
    stack: [
        async ({}, use) => {
            const stack = await start();
            await use(stack);
            await stack.stop();
        },
        { scope: "worker" },
    ],
});

test("gateway responds 200 on /api/health", async ({ stack, request }) => {
    const resp = await request.get(`${stack.gatewayURL}/api/health`);
    expect(resp.ok()).toBe(true);
    const body = await resp.json();
    expect(body).toMatchObject({ status: "ok" });
});

test("workspace serves the SPA index", async ({ stack, request }) => {
    const resp = await request.get(`${stack.workspaceURL}/`);
    expect(resp.ok()).toBe(true);
    const html = await resp.text();
    // We're not asserting the SPA's exact title — that string is
    // user-visible and the test shouldn't pin it. Asserting the doc
    // shape is what matters: it's HTML, not a 404 page or a proxy
    // error.
    expect(html.toLowerCase()).toContain("<!doctype html>");
    expect(html.toLowerCase()).toContain("</html>");
});
