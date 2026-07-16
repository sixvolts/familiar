// skillpacks.spec.ts — SKILL-PACKAGES-SPEC Phase 2: a STANDARD
// agentskills.io skill (SKILL.md + references/, unmodified format)
// imported into the library, bound to a shard, and exercised through
// chat with a real model. This is the compatibility milestone: the
// progressive-disclosure loop — metadata in the prompt → use_skill
// returns the body → read_skill_file serves references — end to end.
//
// The skill is written straight into the stack's library directory
// and admitted via the admin rescan (the cp-r-then-rescan operator
// path). Zip import, path jails, and authorization edges are
// Go-tested in internal/skillpkg.

import { test as base, expect } from "@playwright/test";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, TestUser } from "../fixtures/user";

const MODEL_URL = process.env.FAMILIAR_TEST_CHAT_MODEL_URL || "http://127.0.0.1:8090";
const REPLY_TIMEOUT = 120_000;

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
    test.skip(!(await modelIsUp()), `no inference server at ${MODEL_URL} — skillpacks specs need a live model`);
});

function authed(user: TestUser) {
    return { Cookie: user.cookieHeader, "Content-Type": "application/json" };
}

// A spec-conformant skill. The body and reference carry marker words
// so assertions prove WHICH disclosure layer the model actually hit.
const SKILL_MD = `---
name: dice-oracle
description: Answers questions about dice, dice rolls, and dice odds. Use whenever the user mentions dice.
license: MIT
metadata:
  version: "1.0"
allowed-tools: read_page Bash(jq:*)
---

# Dice Oracle

When asked to roll dice or talk about dice, ALWAYS include the word
DICEWORD in your reply.

When asked about loaded-dice odds, first call read_skill_file with
path references/odds.md and repeat the odds code you find there.
`;

const ODDS_MD = `# Loaded dice odds\n\nThe odds code is ODDSCODE42.\n`;

async function installSkill(stack: GatewayStack) {
    const dir = path.join(stack.instanceDir, "skills", "dice-oracle");
    await fs.mkdir(path.join(dir, "references"), { recursive: true });
    await fs.writeFile(path.join(dir, "SKILL.md"), SKILL_MD);
    await fs.writeFile(path.join(dir, "references", "odds.md"), ODDS_MD);
}

async function chatOnce(request: any, stack: GatewayStack, user: TestUser, convID: string, message: string) {
    const resp = await request.post(`${stack.workspaceURL}/api/chat`, {
        headers: { ...authed(user), Accept: "text/event-stream" },
        data: { message, conversation_id: convID },
        timeout: REPLY_TIMEOUT,
    });
    expect(resp.ok(), `chat: HTTP ${resp.status()}`).toBeTruthy();
    const body = await resp.text();
    expect(body).not.toContain("event: error");
    return body
        .split("\n")
        .filter((l: string) => l.startsWith("data: "))
        .map((l: string) => {
            try {
                return JSON.parse(l.slice(6))?.chunk ?? "";
            } catch {
                return "";
            }
        })
        .join("");
}

test("an unmodified Agent Skill works inside a shard via progressive disclosure", async ({
    stack,
    request,
}) => {
    const admin = await createTestUser({ role: "admin" });

    // The DB outlives stack instances — clear any dice-oracle row left
    // by a previous run so the rescan-admits-it assertion is real.
    // (Delete also removes the library dir, so this runs BEFORE install.)
    const pre = await (
        await request.get(`${stack.workspaceURL}/console/api/skillpacks`, { headers: authed(admin) })
    ).json();
    for (const p of pre.items ?? []) {
        if (p.name === "dice-oracle") {
            await request.delete(`${stack.workspaceURL}/console/api/skillpacks/${p.id}`, {
                headers: authed(admin),
            });
        }
    }

    await installSkill(stack);

    // Rescan admits the dropped-in skill (the operator path; the
    // click IS the approval).
    const rescan = await request.post(`${stack.workspaceURL}/console/api/skillpacks/rescan`, {
        headers: authed(admin),
    });
    expect(rescan.ok(), await rescan.text()).toBeTruthy();
    expect((await rescan.json()).added).toBe(1);

    // The catalog shows it: unsigned badge, scripts-free, and the
    // allowed-tools mapping per the spec decision (read_page matches;
    // the Claude-style Bash pattern is noted as not applicable).
    const list = await (
        await request.get(`${stack.workspaceURL}/console/api/skillpacks`, { headers: authed(admin) })
    ).json();
    const pkg = (list.items ?? []).find((p: any) => p.name === "dice-oracle");
    expect(pkg, "package must be listed").toBeTruthy();
    expect(pkg.signature_status).toBe("unsigned");
    expect(pkg.has_scripts).toBe(false);
    expect(pkg.tools_matched).toContain("read_page");
    expect(pkg.tools_unmatched).toContain("Bash(jq:*)");

    // Shard + binding + bound conversation.
    const shardID = `e2e-skillshard-${Date.now().toString(36)}`;
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/shards`, {
                headers: authed(admin),
                data: {
                    id: shardID,
                    name: "Dice Shard",
                    persistence: "persistent",
                    visibility: "isolated",
                    scope_tag: `shard:${shardID}`,
                    system_prompt: "You are a helpful assistant.",
                    tool_allowlist: [],
                },
            })
        ).ok(),
    ).toBeTruthy();
    const bind = await request.put(`${stack.workspaceURL}/console/api/shards/${shardID}/skills`, {
        headers: authed(admin),
        data: { skill_ids: [pkg.id] },
    });
    expect(bind.ok(), await bind.text()).toBeTruthy();
    const conv = await (
        await request.post(`${stack.workspaceURL}/console/api/conversations`, {
            headers: authed(admin),
            data: { model: `shard:${shardID}` },
        })
    ).json();

    // Layer 2 (activation): the model sees the skill in its prompt,
    // calls use_skill, and follows the body's instruction.
    const rollReply = await chatOnce(request, stack, admin, conv.id, "Please roll some dice for me.");
    expect(rollReply.toUpperCase()).toContain("DICEWORD");

    // Layer 3 (resources): the body directs it to read the reference
    // file; the odds code only exists there.
    const oddsReply = await chatOnce(
        request, stack, admin, conv.id,
        "What is the odds code for loaded dice, according to your skill's reference table?",
    );
    expect(oddsReply.toUpperCase()).toContain("ODDSCODE42");

    // Deletion reconcile: remove the directory from the library and
    // rescan — the catalog must stop advertising it (disabled +
    // reported missing), not keep serving a dead row.
    await fs.rm(path.join(stack.instanceDir, "skills", "dice-oracle"), { recursive: true });
    const rescan2 = await request.post(`${stack.workspaceURL}/console/api/skillpacks/rescan`, {
        headers: authed(admin),
    });
    expect(rescan2.ok()).toBeTruthy();
    expect((await rescan2.json()).missing).toContain("dice-oracle");
    const after = await (
        await request.get(`${stack.workspaceURL}/console/api/skillpacks`, { headers: authed(admin) })
    ).json();
    const gone = (after.items ?? []).find((p: any) => p.name === "dice-oracle");
    expect(gone.disabled_at, "vanished package must be disabled").toBeTruthy();
});

test("library management is admin-only and bindings are owner-scoped", async ({ stack, request }) => {
    const admin = await createTestUser({ role: "admin" });
    const plain = await createTestUser();

    // Non-admin: can LIST (the binding catalog) but not manage.
    expect((await request.get(`${stack.workspaceURL}/console/api/skillpacks`, { headers: authed(plain) })).ok()).toBeTruthy();
    expect(
        (await request.post(`${stack.workspaceURL}/console/api/skillpacks/rescan`, { headers: authed(plain) })).status(),
    ).toBe(403);

    // Binding someone else's shard: not yours, not found.
    const shardID = `e2e-bindscope-${Date.now().toString(36)}`;
    expect(
        (
            await request.post(`${stack.workspaceURL}/console/api/shards`, {
                headers: authed(admin),
                data: {
                    id: shardID, name: "x", persistence: "persistent", visibility: "isolated",
                    scope_tag: `shard:${shardID}`, system_prompt: "x", tool_allowlist: [],
                },
            })
        ).ok(),
    ).toBeTruthy();
    expect(
        (
            await request.put(`${stack.workspaceURL}/console/api/shards/${shardID}/skills`, {
                headers: authed(plain),
                data: { skill_ids: [] },
            })
        ).status(),
    ).toBe(404);
});
