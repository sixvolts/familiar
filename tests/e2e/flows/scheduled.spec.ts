// scheduled.spec.ts — SCHEDULED-ACTIONS-SPEC Phase 1, end to end
// against the real model: create an action whose report target is a
// note, trigger it manually, and watch the report land — in the DB,
// and live in an open editor via the same page-events path
// interactive saves use. Cron *timing* is owned by the Go runner
// tests (injected seams); E2E owns the run-now → deliver → observe
// loop and the console API contract.
//
// Skips without a live inference server, like chat/tooluse.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession, TestUser } from "../fixtures/user";

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
    test.skip(!(await modelIsUp()), `no inference server at ${MODEL_URL} — scheduled specs need a live model`);
});

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

async function createNote(request: any, stack: GatewayStack, user: TestUser, title: string) {
    const resp = await request.post(`${stack.workspaceURL}/console/api/books/personal/pages`, {
        headers: authed(user),
        data: { title, content: `# ${title}\n` },
    });
    expect(resp.ok(), `create note: HTTP ${resp.status()}`).toBeTruthy();
    return resp.json();
}

function actionBody(pageID: string, overrides: Record<string, unknown> = {}) {
    return {
        name: "e2e report",
        prompt: "Reply with exactly: SCHEDULED_OK",
        cron: "0 7 * * *",
        report_targets: [{ kind: "page", book_slug: "personal", page_id: pageID }],
        ...overrides,
    };
}

// pollRun polls the ledger until the run leaves "running".
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

test("run-now executes through the pipeline and appends the report to the note", async ({
    stack,
    request,
}) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Action Log");

    const created = await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers: authed(user),
        data: actionBody(note.id),
    });
    expect(created.status(), await created.text()).toBe(201);
    const action = await created.json();
    expect(action.enabled).toBe(true);
    expect(action.timeout_seconds).toBe(600);

    const fired = await request.post(`${stack.workspaceURL}/console/api/actions/${action.id}/run`, {
        headers: authed(user),
    });
    expect(fired.status()).toBe(202);
    const { run_id } = await fired.json();

    const run = await pollRun(request, stack, user, action.id, run_id);
    expect(run.status, `run failed: ${run.error}`).toBe("ok");
    expect(run.trigger).toBe("manual");
    expect(run.model_id).toBe("test/gemma");
    expect(run.output.toUpperCase()).toContain("SCHEDULED_OK");
    expect(run.deliveries?.[0]?.ok).toBe(true);

    // The note gained a timestamped section attributed to the action.
    const page = await (
        await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers: authed(user),
        })
    ).json();
    expect(page.content).toContain("— e2e report");
    expect(page.content.toUpperCase()).toContain("SCHEDULED_OK");

    // The action row carries the outcome for the panel list.
    const after = await (
        await request.get(`${stack.workspaceURL}/console/api/actions/${action.id}`, {
            headers: authed(user),
        })
    ).json();
    expect(after.last_status).toBe("ok");
    expect(after.last_run_at).toBeTruthy();
});

test("a run's report live-refreshes an open editor, and the panel shows the history", async ({
    stack,
    browser,
    request,
}) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Live Target");
    const action = await (
        await request.post(`${stack.workspaceURL}/console/api/actions`, {
            headers: authed(user),
            data: actionBody(note.id, { name: "live report" }),
        })
    ).json();

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, user);
    const page = await ctx.newPage();
    try {
        // Hold the target note open in a notes tab.
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        await page.locator(".sidebar-cat-notes").click();
        const shell = page.locator(".notes-shell").first();
        await expect(shell).toBeVisible({ timeout: 10_000 });
        await page.evaluate((id) => {
            window.dispatchEvent(new CustomEvent("familiar:openDoc", { detail: { surface: "notes", id } }));
        }, note.id);
        const editor = shell.locator(".toastui-editor-ww-container .ProseMirror").first();
        await expect(editor).toContainText("Live Target", { timeout: 10_000 });

        // Fire the action from outside the browser (API). The append
        // goes through the hooked wiki store → page-events SSE → the
        // open editor refreshes itself.
        const fired = await request.post(
            `${stack.workspaceURL}/console/api/actions/${action.id}/run`,
            { headers: authed(user) },
        );
        expect(fired.status()).toBe(202);
        await expect(editor).toContainText(/SCHEDULED_OK/i, { timeout: RUN_TIMEOUT });
        await expect(editor).toContainText("live report");

        // The panel: list row → detail → run history.
        await page.evaluate(() => (window as any).appSwitchPanel("scheduled"));
        await expect(page.locator("#panel-scheduled")).toBeVisible();
        const row = page.locator("#actions-rows tr", { hasText: "live report" });
        await expect(row).toBeVisible({ timeout: 10_000 });
        await expect(row).toContainText(/ok/i);
        await row.click();
        await expect(page.locator("#action-detail")).toBeVisible();
        await expect(page.locator("#action-runs-rows tr").first()).toContainText("manual", {
            timeout: 10_000,
        });
    } finally {
        await ctx.close();
    }
});

test("on_content stays quiet when the model has nothing to say", async ({ stack, request }) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Quiet Target");
    const before = await (
        await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers: authed(user),
        })
    ).json();

    const action = await (
        await request.post(`${stack.workspaceURL}/console/api/actions`, {
            headers: authed(user),
            data: actionBody(note.id, {
                name: "quiet check",
                delivery_policy: "on_content",
                // The runner appends the NOTHING_TO_REPORT contract;
                // the prompt makes "nothing" the only honest answer.
                prompt: "You are checking for urgent alerts. There are no alerts and nothing has changed.",
            }),
        })
    ).json();

    const fired = await request.post(`${stack.workspaceURL}/console/api/actions/${action.id}/run`, {
        headers: authed(user),
    });
    expect(fired.status()).toBe(202);
    const { run_id } = await fired.json();
    const run = await pollRun(request, stack, user, action.id, run_id);

    // The model answered the sentinel → run succeeded quietly: the
    // reply is ledgered, no deliveries ran, the note is untouched.
    expect(run.status, `run = ${JSON.stringify(run)}`).toBe("skipped_quiet");
    expect(run.output.toUpperCase()).toContain("NOTHING_TO_REPORT");
    expect(run.deliveries ?? null).toBeNull();
    const after = await (
        await request.get(`${stack.workspaceURL}/console/api/books/personal/page-by-id/${note.id}`, {
            headers: authed(user),
        })
    ).json();
    expect(after.content).toBe(before.content);
    expect(after.updated_at).toBe(before.updated_at);
});

test("a conversation target auto-creates its thread and receives the report", async ({
    stack,
    request,
}) => {
    const user = await createTestUser();

    // Submit a conversation target WITHOUT an id — the backend mints
    // the dedicated thread and fills it in.
    const created = await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers: authed(user),
        data: {
            name: "thread report",
            prompt: "Reply with exactly: SCHEDULED_OK",
            cron: "0 7 * * *",
            report_targets: [{ kind: "conversation" }],
        },
    });
    expect(created.status(), await created.text()).toBe(201);
    const action = await created.json();
    const convID = action.report_targets[0].conversation_id;
    expect(convID, "backend must fill conversation_id").toBeTruthy();

    // The thread exists, titled after the action, owned by the user.
    const convs = await (
        await request.get(`${stack.workspaceURL}/console/api/conversations?limit=20`, {
            headers: authed(user),
        })
    ).json();
    const thread = (convs.items ?? []).find((c: any) => c.id === convID);
    expect(thread?.title).toBe("Scheduled: thread report");

    // Run → the report lands as an assistant message in that thread.
    const fired = await request.post(`${stack.workspaceURL}/console/api/actions/${action.id}/run`, {
        headers: authed(user),
    });
    expect(fired.status()).toBe(202);
    const { run_id } = await fired.json();
    const run = await pollRun(request, stack, user, action.id, run_id);
    expect(run.status, `run failed: ${run.error}`).toBe("ok");

    const msgs = await (
        await request.get(`${stack.workspaceURL}/console/api/conversations/${convID}/messages`, {
            headers: authed(user),
        })
    ).json();
    const items = msgs.items ?? msgs.messages ?? [];
    const report = items.find((m: any) => m.role === "assistant");
    expect(report, "assistant report message must exist").toBeTruthy();
    expect(report.content.toUpperCase()).toContain("SCHEDULED_OK");
    expect(report.model).toBe("scheduled:thread report");
});

test("a page_saved trigger fires when the watched book changes", async ({ stack, request }) => {
    const user = await createTestUser();

    // Report target lives in a SEPARATE book — an action reporting
    // into its own watched book would be loop-prevention territory.
    const bookSlug = `reports-${Date.now().toString(36)}`;
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/books`, {
                headers: authed(user),
                data: { name: "Reports", slug: bookSlug },
            })
        ).ok(),
    ).toBeTruthy();
    const report = await (
        await request.post(`${stack.workspaceURL}/console/api/books/${bookSlug}/pages`, {
            headers: authed(user),
            data: { title: "Event Log", content: "# Event Log\n" },
        })
    ).json();

    const created = await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers: authed(user),
        data: {
            name: "watcher",
            prompt: "A page changed. Reply with exactly: EVENT_OK",
            trigger_kind: "page_saved",
            watch_book_slug: "personal",
            report_targets: [{ kind: "page", book_slug: bookSlug, page_id: report.id }],
        },
    });
    expect(created.status(), await created.text()).toBe(201);
    const action = await created.json();
    expect(action.trigger_kind).toBe("page_saved");
    expect(action.watch_book_id).toBeTruthy();

    // Touch the watched book: creating a personal note fires the
    // page-saved hook → bus → runner.
    await createNote(request, stack, user, "Trigger Note");

    // The action fires on its own — no run-now anywhere.
    const deadline = Date.now() + RUN_TIMEOUT;
    let run: any = null;
    while (Date.now() < deadline) {
        const runs = await (
            await request.get(`${stack.workspaceURL}/console/api/actions/${action.id}/runs`, {
                headers: authed(user),
            })
        ).json();
        run = (runs.items ?? []).find((r: any) => r.trigger === "page_saved" && r.status !== "running");
        if (run) break;
        await new Promise((r) => setTimeout(r, 500));
    }
    expect(run, "page_saved run never appeared").toBeTruthy();
    expect(run.status, `run failed: ${run.error}`).toBe("ok");

    const after = await (
        await request.get(`${stack.workspaceURL}/console/api/books/${bookSlug}/page-by-id/${report.id}`, {
            headers: authed(user),
        })
    ).json();
    expect(after.content.toUpperCase()).toContain("EVENT_OK");
    expect(after.content).toContain("— watcher");
});

test("a webhook trigger fires from an unauthenticated POST with the secret URL", async ({
    stack,
    request,
}) => {
    const user = await createTestUser();
    const created = await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers: authed(user),
        data: {
            name: "hook report",
            prompt: "Acknowledge the webhook. Reply with exactly: HOOK_OK",
            trigger_kind: "webhook",
            report_targets: [{ kind: "conversation" }],
        },
    });
    expect(created.status(), await created.text()).toBe(201);
    const action = await created.json();
    expect(action.webhook_token).toMatch(/^whk_/);
    const convID = action.report_targets[0].conversation_id;

    // Fire it with NO session — the URL token is the credential.
    const fired = await request.post(
        `${stack.workspaceURL}/console/api/actions/hooks/${action.webhook_token}`,
        { headers: { "Content-Type": "application/json" }, data: { alert: "disk almost full" } },
    );
    expect(fired.status(), await fired.text()).toBe(202);
    const { run_id } = await fired.json();
    const run = await pollRun(request, stack, user, action.id, run_id);
    expect(run.status, `run failed: ${run.error}`).toBe("ok");
    expect(run.trigger).toBe("webhook");

    // The report landed in the auto-created thread.
    const msgs = await (
        await request.get(`${stack.workspaceURL}/console/api/conversations/${convID}/messages`, {
            headers: authed(user),
        })
    ).json();
    const reportMsg = (msgs.items ?? msgs.messages ?? []).find((m: any) => m.role === "assistant");
    expect(reportMsg?.content.toUpperCase()).toContain("HOOK_OK");

    // Garbage token → uniform 404; an immediate second fire → 429
    // (default min_interval 60s).
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/actions/hooks/whk_garbage`, {
                data: {},
            })
        ).status(),
    ).toBe(404);
    expect(
        (
            await request.post(
                `${stack.workspaceURL}/console/api/actions/hooks/${action.webhook_token}`,
                { data: {} },
            )
        ).status(),
    ).toBe(429);
});

test("an ephemeral-envelope action runs bare: prompt only, nothing else", async ({
    stack,
    request,
}) => {
    const user = await createTestUser();
    const note = await createNote(request, stack, user, "Ephemeral Log");

    const created = await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers: authed(user),
        data: actionBody(note.id, {
            name: "e2e ephemeral",
            envelope: "ephemeral",
            prompt: "Reply with exactly: EPHEMERAL_OK",
        }),
    });
    expect(created.status(), await created.text()).toBe(201);
    const action = await created.json();
    expect(action.envelope).toBe("ephemeral");
    expect(action.shard_id ?? "").toBe("");

    const fired = await request.post(`${stack.workspaceURL}/console/api/actions/${action.id}/run`, {
        headers: authed(user),
    });
    expect(fired.status()).toBe(202);
    const { run_id } = await fired.json();
    const run = await pollRun(request, stack, user, action.id, run_id);
    expect(run.status, `run failed: ${run.error}`).toBe("ok");
    expect(run.output.toUpperCase()).toContain("EPHEMERAL_OK");
    expect(run.deliveries?.[0]?.ok).toBe(true);

    // Envelope coherence is enforced at the API: ephemeral cannot
    // carry a shard binding.
    const bad = await request.post(`${stack.workspaceURL}/console/api/actions`, {
        headers: authed(user),
        data: actionBody(note.id, { envelope: "ephemeral", shard_id: "some-shard" }),
    });
    expect(bad.status()).toBe(400);
});

test("the console API validates shapes and enforces ownership", async ({ stack, request }) => {
    const owner = await createTestUser();
    const intruder = await createTestUser();
    const note = await createNote(request, stack, owner, "Validation Target");
    const base = `${stack.workspaceURL}/console/api/actions`;

    // Shape rejections.
    for (const [label, body] of [
        ["bad cron", actionBody(note.id, { cron: "every full moon" })],
        ["no schedule", actionBody(note.id, { cron: "" })],
        ["no targets", actionBody(note.id, { report_targets: [] })],
        ["unknown target kind", actionBody(note.id, { report_targets: [{ kind: "pigeon" }] })],
        ["timeout out of range", actionBody(note.id, { timeout_seconds: 5 })],
    ] as const) {
        const resp = await request.post(base, { headers: authed(owner), data: body });
        expect(resp.status(), `${label} should 400`).toBe(400);
    }

    // A page target pointing at someone else's note is refused at
    // create time — the intruder can't aim an action at the owner's
    // notebook.
    const aimed = await request.post(base, { headers: authed(intruder), data: actionBody(note.id) });
    expect(aimed.status(), "cross-user page target must be refused").toBe(400);

    // Ownership: the intruder can't see, run, or delete the owner's
    // action; ids read as 404.
    const action = await (
        await request.post(base, { headers: authed(owner), data: actionBody(note.id) })
    ).json();
    for (const probe of [
        request.get(`${base}/${action.id}`, { headers: authed(intruder) }),
        request.post(`${base}/${action.id}/run`, { headers: authed(intruder) }),
        request.delete(`${base}/${action.id}`, { headers: authed(intruder) }),
        request.get(`${base}/${action.id}/runs`, { headers: authed(intruder) }),
    ]) {
        expect((await probe).status()).toBe(404);
    }
    const list = await (await request.get(base, { headers: authed(intruder) })).json();
    expect((list.items ?? []).map((a: any) => a.id)).not.toContain(action.id);

    // Disable / enable round-trip.
    const off = await (
        await request.post(`${base}/${action.id}/disable`, { headers: authed(owner) })
    ).json();
    expect(off.enabled).toBe(false);
    const on = await (
        await request.post(`${base}/${action.id}/enable`, { headers: authed(owner) })
    ).json();
    expect(on.enabled).toBe(true);
});
