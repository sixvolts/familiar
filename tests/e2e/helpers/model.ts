// model.ts — the gate every model-backed spec shares.
//
// A model-backed spec skips when no inference server answers, so the
// suite still runs without one. Three things live here rather than in
// each spec:
//
//   - Time. Playwright's per-test timeout (60s, playwright.config.ts)
//     caps every wait inside a test, so a 120s reply deadline, or the
//     360s FAMILIAR_E2E_RUN_TIMEOUT that CI sets, never got past 60s.
//     The gate raises each gated test's timeout to cover the model waits
//     its file declares, plus a margin for everything else.
//   - Hollow skips. run-tiers.sh sets FAMILIAR_E2E_REQUIRE_MODEL=1 in the
//     tier that has a model. There a failed probe fails the test instead
//     of skipping it, or a server that dies mid-tier would turn the rest
//     of the model tests into skips and the tier would still pass. The
//     probe retries for 30s first, so a busy server that answers /health
//     slowly is not mistaken for a dead one.
//   - Tests that don't call the model. Tagged NO_MODEL, they run with or
//     without a server: authz checks that happen to live in a
//     model-backed file shouldn't disappear when the model does.

import type { TestType } from "@playwright/test";

export const MODEL_URL = process.env.FAMILIAR_TEST_CHAT_MODEL_URL || "http://127.0.0.1:8090";

// Deadline for a scheduled/research run to finish. Configurable because the
// right value depends entirely on the backend: production furnace answers far
// faster than icecube's local MLX model, where a single run measured ~230s
// against this 120s default.
//
// Default stays tight so a real slowdown still shows up locally; CI raises it
// explicitly in e2e.yml, where the value is visible rather than buried.
export const RUN_TIMEOUT = Number(process.env.FAMILIAR_E2E_RUN_TIMEOUT ?? 120_000);

// Tag for a test in a model-backed file that never calls the model.
export const NO_MODEL = "@no-model";

// Room in a gated test for everything that isn't a model wait: the
// fixture's users and pages, browser steps, assertions after the reply.
const MARGIN_MS = 60_000;

const REQUIRE_MODEL = process.env.FAMILIAR_E2E_REQUIRE_MODEL === "1";
const REQUIRE_PROBE_MS = 30_000;

async function probe(): Promise<boolean> {
    try {
        const resp = await fetch(`${MODEL_URL}/health`, { signal: AbortSignal.timeout(2_000) });
        return resp.ok;
    } catch {
        return false;
    }
}

export async function modelIsUp(): Promise<boolean> {
    const deadline = Date.now() + (REQUIRE_MODEL ? REQUIRE_PROBE_MS : 0);
    for (;;) {
        if (await probe()) return true;
        if (Date.now() >= deadline) return false;
        await new Promise((r) => setTimeout(r, 2_000));
    }
}

// gateOnModel gates every test in the calling spec file on the model.
// modelWaitMs is the longest any one test in the file may spend waiting
// on the model (the sum of its reply/run deadlines); what names the
// specs in the skip message.
export function gateOnModel(test: TestType<any, any>, what: string, modelWaitMs: number): void {
    test.beforeEach(async ({}, testInfo) => {
        if (testInfo.tags.includes(NO_MODEL)) return;
        testInfo.setTimeout(Math.max(testInfo.timeout, modelWaitMs + MARGIN_MS));
        if (await modelIsUp()) return;
        const why = `no inference server at ${MODEL_URL} — ${what} need a live model`;
        if (REQUIRE_MODEL) {
            throw new Error(`${why}. FAMILIAR_E2E_REQUIRE_MODEL=1: this tier runs against a model, so skipping would hide it`);
        }
        test.skip(true, why);
    });
}
