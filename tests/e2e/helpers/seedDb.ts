// seedDb.ts — direct-to-Postgres helpers for per-test isolation
// (TESTING-PLAN.md §"Why the fixtures matter"). The gateway runs its
// own migrations on boot, so these only reset rows, never schema.

import { Client } from "pg";

// Auth tables the bootstrap registration path touches. Order doesn't
// matter — TRUNCATE ... CASCADE clears dependents. The pipeline
// `sessions` table (rolling summaries) is deliberately NOT here; it's
// unrelated to auth.
const AUTH_TABLES = ["webauthn_credentials", "admin_sessions", "users", "identity_map"];

// resetAuth clears the WebAuthn/auth state so the next boot sees an
// empty credentials table and offers first-run setup. Re-runnable: a
// dev running the auth spec twice against the same DB needs this, and
// it makes the spec independent of whatever state a prior run left.
//
// dsn defaults to FAMILIAR_TEST_DSN — the same database the gateway
// fixture points the gateway at.
export async function resetAuth(dsn = process.env.FAMILIAR_TEST_DSN): Promise<void> {
    if (!dsn) throw new Error("resetAuth: FAMILIAR_TEST_DSN is required");
    const client = new Client({ connectionString: dsn });
    await client.connect();
    try {
        await client.query(
            `TRUNCATE TABLE ${AUTH_TABLES.join(", ")} RESTART IDENTITY CASCADE`,
        );
        // Verify the table really is empty afterward. This catches the
        // most confusing failure mode — the bootstrap setup view not
        // appearing because some credential survived (wrong DSN, a
        // different schema's search_path, a permissions no-op). Far
        // better to fail here, loudly, than 10 lines later on an opaque
        // "#view-setup is hidden".
        const { rows } = await client.query<{ n: string }>(
            `SELECT COUNT(*)::text AS n FROM webauthn_credentials`,
        );
        const remaining = Number(rows[0]?.n ?? "0");
        if (remaining !== 0) {
            throw new Error(
                `resetAuth: webauthn_credentials still has ${remaining} row(s) after TRUNCATE — ` +
                    `is FAMILIAR_TEST_DSN (${redactDsn(dsn)}) the same database the gateway uses?`,
            );
        }
    } catch (e) {
        // A missing table on a brand-new DB is fine — the gateway
        // creates it (empty) on first boot, which is as empty as the
        // bootstrap path needs.
        const msg = (e as Error).message || "";
        if (!/does not exist/i.test(msg)) throw e;
    } finally {
        await client.end();
    }
}

// redactDsn strips the password from a connection string for safe
// inclusion in an error message.
function redactDsn(dsn: string): string {
    return dsn.replace(/(:)([^:@/]+)(@)/, "$1***$3");
}
