// skillpacks-ui.spec.ts — the imported-skills CONSOLE UI, driven
// through the real DOM: the import modal's preview-then-approve flow
// (zip upload via the file input), the library card rendering, and
// the shard form's binding checklist with its bind-time tool warning.
//
// The API contract is covered in skillpacks.spec.ts and Go; this spec
// is regression insurance for the glue — element IDs, listeners, and
// the approve-revocation logic live only in panel JS. No model needed:
// nothing here runs inference.

import { test as base, expect } from "@playwright/test";
import { start, GatewayStack } from "../fixtures/gateway";
import { createTestUser, attachSession } from "../fixtures/user";
import { storedZip } from "../fixtures/zip";

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

function skillMD(name: string): string {
    return `---
name: ${name}
description: UI e2e skill that reads wiki pages.
license: MIT
allowed-tools: read_page Bash(jq:*)
---

# UI Skill

Body of the UI test skill.
`;
}

test("import page and shard binding checklist drive the real UI", async ({
    stack,
    browser,
    request,
}) => {
    const admin = await createTestUser({ role: "admin" });
    // Unique name per run: the test DB outlives stack instance dirs,
    // so a fixed name would collide with rows from previous runs.
    const name = `uiskill${Date.now().toString(36)}`;
    const zip = storedZip([
        { name: `${name}/SKILL.md`, data: Buffer.from(skillMD(name)) },
        { name: `${name}/references/notes.md`, data: Buffer.from("# notes\n") },
    ]);

    const ctx = await browser.newContext();
    await attachSession(ctx, stack.workspaceURL, admin);
    const page = await ctx.newPage();
    try {
        await page.goto(stack.workspaceURL);
        await expect(page.locator("#view-dashboard")).toBeVisible({ timeout: 15_000 });
        // The imported-skills library lives on the admin System skills
        // panel now (zip import is admin-only; users author markdown).
        await page.evaluate(() => (window as any).appSwitchPanel("system-skills"));
        await expect(page.locator("#panel-system-skills")).toBeVisible();

        // ── Import: preview shows what approve admits. The importer
        // is a full-page sub-view (list→detail pattern), not a modal:
        // opening it hides the home sections. ──────────────
        await page.locator("#skillpacks-import").click();
        await expect(page.locator("#skill-import-view")).toBeVisible();
        await expect(page.locator("#system-skills-home")).toBeHidden();
        await page.locator("#skillpack-import-file").setInputFiles({
            name: "skill.zip",
            mimeType: "application/zip",
            buffer: zip,
        });
        await page.locator("#skillpack-import-go").click();

        const preview = page.locator("#skillpack-import-preview");
        await expect(preview).toBeVisible();
        await expect(preview).toContainText(name.toUpperCase());
        await expect(preview).toContainText("UNSIGNED");
        await expect(preview).toContainText("read_page"); // matched
        await expect(preview).toContainText("Bash(jq:*)"); // noted, not applicable
        await expect(preview).toContainText("Body of the UI test skill");

        // Changing the source revokes the approve button.
        const approve = page.locator("#skillpack-import-approve");
        await expect(approve).toBeVisible();
        await page.locator("#skillpack-import-url").fill("https://example.com/x.zip");
        await expect(approve).toBeHidden();
        await page.locator("#skillpack-import-url").fill("");
        await page.locator("#skillpack-import-go").click();
        await expect(approve).toBeVisible();

        await approve.click();
        // Approval returns to the System skills home view.
        await expect(page.locator("#skill-import-view")).toBeHidden();
        await expect(page.locator("#system-skills-home")).toBeVisible();
        await expect(page.locator("#skillpacks-list")).toContainText(name.toUpperCase());

        // ── Bind through the shard form, with the tool warning ─────
        await page.evaluate(() => (window as any).appSwitchPanel("shards"));
        await expect(page.locator("#panel-shards")).toBeVisible();
        await page.locator("#shards-new").click();
        await expect(page.locator("#shard-detail")).toBeVisible();

        const shardID = `e2e-ui-${Date.now().toString(36)}`;
        await page.locator("#shard-id").fill(shardID);
        await page.locator("#shard-name").fill("UI Bind Shard");
        await page.locator("#shard-prompt").fill("You are a UI e2e shard.");

        const skillRow = page.locator(".shard-skillpack-row", { hasText: name });
        await skillRow.locator("input").check();

        // The skill requests read_page; the allowlist is empty, so the
        // bind-time warning shows — and clears when the tool is added.
        const warn = page.locator(".shard-skillpack-warn", { hasText: "read_page" });
        await expect(warn).toBeVisible();
        await page.locator('.shard-tool-option[data-name="read_page"] input').check();
        await expect(warn).toBeHidden();

        await page.locator('#shard-form button[type="submit"]').click();

        // Create reopens the detail view with server state: the
        // binding survived the round-trip.
        await expect(page.locator("#shard-detail-title")).toContainText("UI Bind Shard", {
            timeout: 10_000,
        });
        await expect(
            page.locator(".shard-skillpack-row", { hasText: name }).locator("input"),
        ).toBeChecked();

        // And the server agrees.
        const bound = await (
            await request.get(`${stack.workspaceURL}/console/api/shards/${shardID}/skills`, {
                headers: { Cookie: admin.cookieHeader },
            })
        ).json();
        expect((bound.items ?? []).map((p: any) => p.name)).toContain(name);
    } finally {
        await ctx.close();
    }
});
