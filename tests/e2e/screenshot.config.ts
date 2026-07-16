import { defineConfig, devices } from "@playwright/test";

// Config for the README screenshot generator (tests/e2e/screenshot/*).
// Kept separate from playwright.config.ts (testDir ./flows) so the
// generator NEVER runs in the normal test suite. It's driven by CI
// (.github/workflows/screenshot.yml) whenever the frontend changes, so
// docs/screenshot.png stays current with no Node/npm in local builds.
// Manual run (needs Node): `npm run screenshot` from tests/e2e — wants
// FAMILIAR_TEST_DSN (a throwaway Postgres); on hosts without a
// Playwright-bundled Chromium set PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH.
export default defineConfig({
    testDir: "./screenshot",
    workers: 1,
    timeout: 180_000,
    expect: { timeout: 10_000 },
    reporter: "list",
    projects: [
        {
            name: "chromium",
            use: {
                ...devices["Desktop Chrome"],
                launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
                    ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
                    : {},
            },
        },
    ],
});
