// authenticator.ts — a virtual WebAuthn authenticator via Chrome
// DevTools Protocol, so the register/login ceremonies run end to end
// without a hardware key. TESTING-PLAN.md §"WebAuthn".
//
// Must be enabled on the page BEFORE any navigator.credentials.create
// / .get call — i.e. before the registration/login click. The
// authenticator lives on the page's CDP session and is discarded with
// the page (each test owns its own context, so credentials never leak
// between tests).

import type { CDPSession, Page } from "@playwright/test";

export interface VirtualAuthenticator {
    client: CDPSession;
    authenticatorId: string;
}

// enableVirtualAuthenticator installs a CTAP2 platform authenticator
// with resident keys + auto-verified user presence. Resident keys are
// required because the login path uses a *discoverable* assertion
// (the gateway's loginBegin doesn't send an allowlist — the
// authenticator must surface the credential itself).
export async function enableVirtualAuthenticator(page: Page): Promise<VirtualAuthenticator> {
    const client = await page.context().newCDPSession(page);
    await client.send("WebAuthn.enable");
    const { authenticatorId } = await client.send("WebAuthn.addVirtualAuthenticator", {
        options: {
            protocol: "ctap2",
            transport: "internal",
            hasResidentKey: true,
            hasUserVerification: true,
            isUserVerified: true,
            // Auto-confirm the user-presence/verification gesture so the
            // ceremony doesn't block waiting for a (nonexistent) touch.
            automaticPresenceSimulation: true,
        },
    });
    return { client, authenticatorId };
}
