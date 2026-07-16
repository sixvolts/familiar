// user.ts — seeded users + sessions, no WebAuthn ceremony
// (TESTING-PLAN.md §"Phase 1" step 3, landed with Phase 2).
//
// The auth spec proves the real ceremony works end to end; every
// other spec just needs "an approved user with a live session".
// Seeding users/admin_sessions directly and dropping the cookie on
// the BrowserContext gets there in ~10ms instead of a multi-second
// ceremony per test, and gives each test its own identity (rule #1
// of "Why the fixtures matter": each test owns its data).
//
// The session token mirrors the gateway exactly: 32 random bytes,
// base64url, stored raw in admin_sessions.token and sent raw as the
// cookie value (internal/admin/sessions.go). principal_type/'user'
// matches the shard_auth_phase1 schema so the authz middleware reads
// the row as a plain user session. The middleware re-checks user
// status on every request, so tests can flip `users.status` mid-test
// and watch a live session die.

import type { BrowserContext } from "@playwright/test";
import { Client } from "pg";
import * as crypto from "node:crypto";

// SESSION_COOKIE is the gateway's session cookie name
// (internal/admin/handler.go sessionCookieName).
export const SESSION_COOKIE = "familiar_admin_session";

export interface TestUser {
    id: string;
    displayName: string;
    email: string;
    role: "admin" | "user";
    // Raw session token — also the cookie value.
    sessionToken: string;
    // Cookie header value for APIRequestContext callers.
    cookieHeader: string;
}

export interface CreateUserOpts {
    role?: "admin" | "user";
    status?: string; // default "approved"
    idPrefix?: string; // default "e2e"
    displayName?: string;
}

// createTestUser inserts an approved user + a live session row and
// returns both. IDs are random-suffixed so concurrent specs (and
// repeated runs against the same DB) never collide; rows are left
// behind deliberately — they're inert, and deleting a user would
// trip FK references from any content the test created.
export async function createTestUser(opts: CreateUserOpts = {}): Promise<TestUser> {
    const dsn = process.env.FAMILIAR_TEST_DSN;
    if (!dsn) throw new Error("createTestUser: FAMILIAR_TEST_DSN is required");

    const role = opts.role ?? "user";
    const suffix = crypto.randomBytes(4).toString("hex");
    const id = `${opts.idPrefix ?? "e2e"}-${role}-${suffix}`;
    const displayName = opts.displayName ?? `E2E ${role} ${suffix}`;
    const email = `${id}@e2e.test`;
    const sessionToken = crypto.randomBytes(32).toString("base64url");

    const client = new Client({ connectionString: dsn });
    await client.connect();
    try {
        await client.query(
            `INSERT INTO users (id, display_name, status, role, email, approved_at)
             VALUES ($1, $2, $3, $4, $5, NOW())`,
            [id, displayName, opts.status ?? "approved", role, email],
        );
        await client.query(
            `INSERT INTO admin_sessions (token, user_id, principal_type, principal_id, expires_at)
             VALUES ($1, $2, 'user', $2, NOW() + INTERVAL '2 hours')`,
            [sessionToken, id],
        );
    } finally {
        await client.end();
    }

    return {
        id,
        displayName,
        email,
        role,
        sessionToken,
        cookieHeader: `${SESSION_COOKIE}=${sessionToken}`,
    };
}

// attachSession drops the user's session cookie on a BrowserContext
// so every page in it is authenticated from the first navigation.
export async function attachSession(
    context: BrowserContext,
    workspaceURL: string,
    user: TestUser,
): Promise<void> {
    await context.addCookies([
        {
            name: SESSION_COOKIE,
            value: user.sessionToken,
            url: workspaceURL,
            httpOnly: true,
            sameSite: "Lax",
        },
    ]);
}

// setUserStatus flips a user's status (approved / disabled / ...) so
// specs can exercise the authz middleware's per-request status check.
export async function setUserStatus(userID: string, status: string): Promise<void> {
    const dsn = process.env.FAMILIAR_TEST_DSN;
    if (!dsn) throw new Error("setUserStatus: FAMILIAR_TEST_DSN is required");
    const client = new Client({ connectionString: dsn });
    await client.connect();
    try {
        await client.query(`UPDATE users SET status = $2 WHERE id = $1`, [userID, status]);
    } finally {
        await client.end();
    }
}
