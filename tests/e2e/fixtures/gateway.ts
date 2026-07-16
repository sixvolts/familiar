// gateway.ts — boots the gateway + workspace as subprocesses and
// returns their base URLs. This is the scaffolding every E2E spec
// pulls in.
//
// Lifecycle:
//   1. Build the gateway + workspace binaries into tests/e2e/.bin
//      (Go's build cache makes the second run near-instant).
//   2. Pick two free TCP ports.
//   3. Write a minimal gateway.toml + workspace.toml into a temp
//      directory; point them at FAMILIAR_TEST_DSN and the bare
//      minimum config that lets the gateway boot cleanly without
//      LLMs, sidecars, or admin auth.
//   4. Spawn both binaries, wait for the gateway's /api/health to
//      answer 200 and for the workspace root to serve.
//   5. On teardown, SIGTERM both, force-kill after a grace period.
//
// See ../../TESTING-PLAN.md §"Phase 1" for what this is the seed of.

import { spawn, spawnSync, ChildProcess } from "node:child_process";
import * as net from "node:net";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import * as os from "node:os";

const REPO_ROOT = path.resolve(__dirname, "../../..");
const GATEWAY_DIR = path.join(REPO_ROOT, "familiar-gateway");
const WORKSPACE_DIR = path.join(REPO_ROOT, "familiar-workspace");
const BIN_DIR = path.resolve(__dirname, "../.bin");

// FIRST_USER_ID is the canonical id the bootstrap WebAuthn registration
// assigns to the first credential (gateway.toml admin.first_user_id).
// Exposed so auth specs can assert the registered admin's identity.
export const FIRST_USER_ID = "e2e-admin";

export interface GatewayStack {
    gatewayURL: string;
    workspaceURL: string;
    firstUserID: string; // admin.first_user_id for the bootstrap registration
    instanceDir: string; // temp dir where toml configs and logs live
    stop: () => Promise<void>;
}

export interface StartOpts {
    // Override the test DSN. Defaults to process.env.FAMILIAR_TEST_DSN.
    dsn?: string;
    // Total ms to wait for both processes to come up. Default 30s.
    bootTimeoutMs?: number;
    // Enable the admin console + WebAuthn auth. Off by default so the
    // smoke fixture stays minimal; auth specs pass admin:true, which
    // adds the relying-party config bound to the workspace origin.
    admin?: boolean;
    // Enable public-link sharing with this host allowlist. The share
    // spec sends spoofed Host headers to exercise the host gate, so
    // the entries don't need DNS — they just have to match (or not
    // match) the inbound Host.
    publicHosts?: string[];
    // Base URL of a live OpenAI-compatible inference server (e.g. a
    // local llama-server). When set, a role-less chat model is added
    // pointing at it, which the disabled router's rule-based fallback
    // then picks for /api/chat. The registry health-checks it
    // immediately on boot, so it's online by the time specs run.
    chatModelURL?: string;
    // Load the REPO's real prompts (prompts/system_prompt.md +
    // prompts/tiers/*) instead of the minimal deterministic test
    // prompt. Use this to exercise the production prompt content —
    // e.g. the wiki-tool guidance the real instance relies on.
    realPrompts?: boolean;
    // Enable Web Push with a fixed test VAPID keypair so the push
    // subscribe/key endpoints + the `push` delivery target are live.
    push?: boolean;
}

// A throwaway VAPID keypair for tests (generated via `gateway --gen-vapid`).
// Not used for any real push service — just makes [push] "configured".
export const TEST_VAPID_PUBLIC =
    "BJORr4S4nLXH9s2-VWvhtgCd4n8Pyw0c2fxyleJtKJxgfF9FInFiiXYk3ao0a972b5U2YC8q5BB4K7FSdeTJstM";
const TEST_VAPID_PRIVATE = "poibXqmMhNxH1o0sBfCKzU7W2vSQesqrm_G1pao7N6Q";

/**
 * Build + spawn the gateway and workspace. Returns once both are
 * answering health probes. Callers are responsible for calling
 * stop() on teardown (the worker fixture in flows/*.spec.ts does
 * this).
 */
export async function start(opts: StartOpts = {}): Promise<GatewayStack> {
    const dsn = opts.dsn ?? process.env.FAMILIAR_TEST_DSN;
    if (!dsn) {
        throw new Error(
            "FAMILIAR_TEST_DSN is required. See tests/e2e/MAKE_TESTS.md.",
        );
    }
    const bootTimeout = opts.bootTimeoutMs ?? 30_000;

    await buildBinaries();
    const [gatewayPort, workspacePort] = await Promise.all([freePort(), freePort()]);

    const instanceDir = await fs.mkdtemp(path.join(os.tmpdir(), "familiar-e2e-"));
    // Servers bind 127.0.0.1; the browser/tests reach them via
    // `localhost` (which resolves to 127.0.0.1). localhost — NOT
    // 127.0.0.1 — is required for WebAuthn: it's a "secure context"
    // over plain http, and an IP literal can't be a WebAuthn rp_id.
    const gatewayURL = `http://localhost:${gatewayPort}`;
    const workspaceURL = `http://localhost:${workspacePort}`;
    const staticDir = path.join(WORKSPACE_DIR, "static");

    const gatewayToml = path.join(instanceDir, "gateway.toml");
    const workspaceToml = path.join(instanceDir, "workspace.toml");
    if (opts.chatModelURL && !opts.realPrompts) {
        // Deterministic test system prompt. Tool guidance keeps the
        // multi-step specs reliable: the model is told where notes
        // live and to act without confirmation round-trips.
        await fs.writeFile(
            path.join(instanceDir, "system_prompt.md"),
            [
                "You are Familiar, a personal assistant with tool access.",
                "The user's notes are pages in their personal wiki book — find it with list_books (include_personal=true).",
                "When the user asks you to read or change notes, use the wiki tools and perform the action immediately.",
                "Never ask for confirmation; complete the requested action, then reply with a short summary.",
            ].join("\n"),
        );
    }
    await fs.writeFile(
        gatewayToml,
        renderGatewayToml({
            port: gatewayPort,
            dsn,
            instanceDir,
            admin: opts.admin ?? false,
            workspaceURL,
            publicHosts: opts.publicHosts ?? [],
            chatModelURL: opts.chatModelURL ?? "",
            realPrompts: opts.realPrompts ?? false,
            push: opts.push ?? false,
        }),
    );
    await fs.writeFile(workspaceToml, renderWorkspaceToml({ port: workspacePort, gatewayURL, staticDir }));

    const gatewayBin = path.join(BIN_DIR, "gateway");
    const workspaceBin = path.join(BIN_DIR, "workspace");

    const gatewayLog = await fs.open(path.join(instanceDir, "gateway.log"), "w");
    const workspaceLog = await fs.open(path.join(instanceDir, "workspace.log"), "w");

    const gatewayProc = spawn(gatewayBin, ["-http", "-config", gatewayToml], {
        stdio: ["ignore", "pipe", "pipe"],
        env: { ...process.env, FAMILIAR_HOME: instanceDir },
    });
    const workspaceProc = spawn(workspaceBin, ["-config", workspaceToml], {
        stdio: ["ignore", "pipe", "pipe"],
    });

    // Tee stdout/stderr to per-process log files so failures have a
    // breadcrumb trail. We don't echo to the test runner stream by
    // default — too noisy when 30+ specs boot the stack.
    tee(gatewayProc, gatewayLog);
    tee(workspaceProc, workspaceLog);

    const stop = async () => {
        await Promise.all([
            stopChild(gatewayProc),
            stopChild(workspaceProc),
        ]);
        await gatewayLog.close().catch(() => {});
        await workspaceLog.close().catch(() => {});
    };

    try {
        await Promise.all([
            waitForUrl(`${gatewayURL}/api/health`, bootTimeout, gatewayProc, "gateway"),
            waitForUrl(`${workspaceURL}/`, bootTimeout, workspaceProc, "workspace"),
        ]);
    } catch (e) {
        await stop();
        const gw = await readTail(path.join(instanceDir, "gateway.log"));
        const ws = await readTail(path.join(instanceDir, "workspace.log"));
        throw new Error(
            `${(e as Error).message}\n\n--- gateway.log (tail) ---\n${gw}\n--- workspace.log (tail) ---\n${ws}`,
        );
    }

    return { gatewayURL, workspaceURL, firstUserID: FIRST_USER_ID, instanceDir, stop };
}

// ── binary build ──────────────────────────────────────────────────

let buildOnce: Promise<void> | null = null;

function buildBinaries(): Promise<void> {
    if (buildOnce) return buildOnce;
    buildOnce = (async () => {
        await fs.mkdir(BIN_DIR, { recursive: true });
        runGoBuild(GATEWAY_DIR, "./cmd/gateway", path.join(BIN_DIR, "gateway"));
        runGoBuild(WORKSPACE_DIR, "./cmd/workspace", path.join(BIN_DIR, "workspace"));
    })();
    return buildOnce;
}

function runGoBuild(cwd: string, pkg: string, out: string) {
    const result = spawnSync("go", ["build", "-o", out, pkg], { cwd, encoding: "utf8" });
    if (result.status !== 0) {
        throw new Error(
            `go build ${pkg} (cwd=${cwd}) failed:\n${result.stderr || result.stdout}`,
        );
    }
}

// ── tomls ─────────────────────────────────────────────────────────

// Minimal gateway.toml for tests: in-process memengine, no sidecar,
// no models that actually resolve. The dummy model entry is required
// by Config.Validate (at least one model). Chat tests will need a
// real model wired in a future phase.
//
// When admin is true, the [admin] block is enabled with a relying
// party bound to the workspace origin so the WebAuthn ceremony
// validates. rp_id is "localhost" (the host, port stripped by the
// gateway's RP matcher); rp_origins is the exact browser origin
// (WebAuthn checks the full origin incl. port from the signed client
// data). cookie_secure stays off — the test stack is plain http.
function renderGatewayToml({
    port,
    dsn,
    instanceDir,
    admin,
    workspaceURL,
    publicHosts,
    chatModelURL,
    realPrompts,
    push,
}: {
    port: number;
    dsn: string;
    instanceDir: string;
    admin: boolean;
    workspaceURL: string;
    publicHosts: string[];
    chatModelURL: string;
    realPrompts: boolean;
    push: boolean;
}): string {
    const socketPath = path.join(instanceDir, "engine.sock");
    const sharingBlock = publicHosts.length
        ? `
[sharing]
public_hosts = [${publicHosts.map((h) => JSON.stringify(h)).join(", ")}]
public_base_url = "http://${publicHosts[0]}"
`
        : "";
    const pushBlock = push
        ? `
[push]
vapid_public_key = ${JSON.stringify(TEST_VAPID_PUBLIC)}
vapid_private_key = ${JSON.stringify(TEST_VAPID_PRIVATE)}
subject = "mailto:test@example.com"
`
        : "";
    // No role tag — that's what makes it the chat model. The dummy
    // role="small" entry below stays either way: it satisfies config
    // validation when no chat model is wired, and exercises the
    // "skip offline models" path when one is.
    //
    // capabilities=["tools"] is load-bearing: without it the pipeline
    // never attaches tool schemas to the request, so tool-use specs
    // silently degrade to plain completions.
    //
    // The [system_prompt] block pins OUR prompt file: the config
    // default is ~/.familiar/system_prompt.md, which exists on dev
    // boxes and would otherwise leak the production prompt into the
    // test stack (and vary between machines).
    // realPrompts: point at the repo's actual prompts so specs
    // exercise production prompt content (tiered base + tier + tool
    // policy, with system_prompt.md as the monolithic fallback).
    const promptFile = realPrompts
        ? path.join(REPO_ROOT, "prompts", "system_prompt.md")
        : path.join(instanceDir, "system_prompt.md");
    const promptDirLine = realPrompts
        ? `\nprompt_dir = "${path.join(REPO_ROOT, "prompts", "tiers")}"`
        : "";
    const chatModelBlock = chatModelURL
        ? `
[system_prompt]
file = "${promptFile}"${promptDirLine}

[[models]]
id = "test/gemma"
provider = "llama-server"
endpoint = "${chatModelURL}"
context_window = 32768
latency_profile = "local"
capabilities = ["tools"]
`
        : "";
    const adminBlock = admin
        ? `
[admin]
enabled = true
first_user_id = "${FIRST_USER_ID}"
rp_id = "localhost"
rp_display_name = "Familiar E2E"
rp_origins = ["${workspaceURL}"]
session_max_age = 3600
cookie_secure = false
`
        : `
[admin]
enabled = false
`;
    return `
[engine]
socket_path = "${socketPath}"

[adapter.http]
listen_addr = "127.0.0.1:${port}"

[context]
window_size = 32768
output_reservation = 4096
system_prompt_ratio = 0.10
memory_ratio = 0.12
tool_result_ratio = 0.12
max_tool_result_tokens = 2000

[skills]
dir = "${instanceDir}/skills"

[media]
dir = "${instanceDir}/media"

[memory]
local_dsn = "${dsn}"
relevance_threshold = 0.55
dedup_threshold = 0.95
max_injected_memories = 5
${adminBlock}${sharingBlock}${pushBlock}
[sidecar]
enabled = false

[router]
enabled = false

[[models]]
id = "test/dummy"
endpoint = "http://127.0.0.1:1"
role = "small"
${chatModelBlock}`;
}

function renderWorkspaceToml({
    port,
    gatewayURL,
    staticDir,
}: {
    port: number;
    gatewayURL: string;
    staticDir: string;
}): string {
    return `
listen_addr = "127.0.0.1:${port}"
gateway_url = "${gatewayURL}"
static_dir = "${staticDir}"
`;
}

// ── helpers ───────────────────────────────────────────────────────

async function freePort(): Promise<number> {
    return new Promise((resolve, reject) => {
        const srv = net.createServer();
        srv.unref();
        srv.once("error", reject);
        srv.listen(0, "127.0.0.1", () => {
            const addr = srv.address();
            if (typeof addr !== "object" || !addr) {
                reject(new Error("could not get bound port"));
                return;
            }
            const port = addr.port;
            srv.close((err) => (err ? reject(err) : resolve(port)));
        });
    });
}

async function waitForUrl(
    url: string,
    timeoutMs: number,
    child: ChildProcess,
    label: string,
): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    let lastErr: unknown = null;
    while (Date.now() < deadline) {
        if (child.exitCode !== null) {
            throw new Error(`${label} exited with code ${child.exitCode} before responding at ${url}`);
        }
        try {
            const resp = await fetch(url, { signal: AbortSignal.timeout(2_000) });
            if (resp.ok) return;
            lastErr = new Error(`HTTP ${resp.status}`);
        } catch (e) {
            lastErr = e;
        }
        await sleep(150);
    }
    throw new Error(`${label} never came up at ${url} (last error: ${lastErr})`);
}

async function stopChild(child: ChildProcess): Promise<void> {
    if (child.exitCode !== null) return;
    child.kill("SIGTERM");
    const settled = await Promise.race([
        new Promise<"exit">((resolve) => child.once("exit", () => resolve("exit"))),
        sleep(3_000).then(() => "timeout" as const),
    ]);
    if (settled === "timeout") {
        child.kill("SIGKILL");
        await new Promise<void>((resolve) => child.once("exit", () => resolve()));
    }
}

function tee(child: ChildProcess, fh: fs.FileHandle): void {
    child.stdout?.on("data", (chunk) => fh.write(chunk).catch(() => {}));
    child.stderr?.on("data", (chunk) => fh.write(chunk).catch(() => {}));
}

async function readTail(file: string, bytes = 4_000): Promise<string> {
    try {
        const stat = await fs.stat(file);
        const start = Math.max(0, stat.size - bytes);
        const fh = await fs.open(file, "r");
        try {
            const buf = Buffer.alloc(stat.size - start);
            await fh.read(buf, 0, buf.length, start);
            return buf.toString("utf8");
        } finally {
            await fh.close();
        }
    } catch {
        return "(no log file)";
    }
}

function sleep(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
}
