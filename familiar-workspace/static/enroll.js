    (function () {
        "use strict";

        const card = document.getElementById("card");
        const btn = document.getElementById("register-btn");
        const status = document.getElementById("status");
        const domainCell = document.getElementById("domain-cell");
        const accountCell = document.getElementById("account-cell");
        const signInLink = document.getElementById("sign-in-link");

        const params = new URLSearchParams(location.search);
        const token = params.get("token") || "";

        function setStatus(kind, message) {
            status.className = "status is-" + kind;
            status.textContent = message;
        }

        function b64urlToBytes(b64) {
            // SubtleCrypto WebAuthn endpoints receive URL-safe base64
            // from the gateway; convert to a Uint8Array for the
            // browser's CredentialCreationOptions shape.
            const pad = "=".repeat((4 - (b64.length % 4)) % 4);
            const norm = (b64 + pad).replace(/-/g, "+").replace(/_/g, "/");
            const bin = atob(norm);
            const out = new Uint8Array(bin.length);
            for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
            return out;
        }
        function bytesToB64url(buf) {
            const bytes = new Uint8Array(buf);
            let s = "";
            for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
            return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
        }

        if (!token) {
            setStatus("err", "Missing enrollment token — this page expects a ?token=... query parameter.");
            return;
        }
        if (!window.PublicKeyCredential) {
            setStatus("err", "This browser does not support passkeys.");
            return;
        }

        domainCell.textContent = location.host;
        // canonical_id surfaces from the begin response — until then,
        // show a placeholder so the layout doesn't jump.
        accountCell.textContent = "(verifying…)";

        // The flow:
        //   1. POST /console/api/auth/enroll/begin {token}
        //      → server validates token, returns CredentialCreationOptions
        //   2. navigator.credentials.create(options)
        //      → browser runs the authenticator ceremony
        //   3. POST /console/api/auth/enroll/finish {token, attestation}
        //      → server records the credential and consumes the token
        async function beginCeremony() {
            const resp = await fetch("/console/api/auth/enroll/begin", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ token: token }),
            });
            if (!resp.ok) {
                const t = await resp.text();
                const err = new Error("begin: " + resp.status + " — " + (t || "no body"));
                err.phase = "begin"; // a begin failure means the link itself is bad
                throw err;
            }
            return resp.json();
        }

        async function finishCeremony(attestation) {
            const resp = await fetch("/console/api/auth/enroll/finish", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ token: token, attestation: attestation }),
            });
            if (!resp.ok) {
                const t = await resp.text();
                throw new Error("finish: " + resp.status + " — " + (t || "no body"));
            }
            return resp.json();
        }

        async function register() {
            btn.disabled = true;
            setStatus("info", "Follow your authenticator's prompt to create a passkey.");
            try {
                const opts = await beginCeremony();
                if (!opts || !opts.publicKey) {
                    throw new Error("server returned no publicKey options");
                }
                const pk = opts.publicKey;

                // The browser WebAuthn API takes ArrayBuffers, but the
                // gateway hands back base64url strings. Convert every
                // binary field on the way in.
                pk.challenge = b64urlToBytes(pk.challenge);
                // user.id is a plain string (EncodeUserIDAsString=true
                // in the Go config). UTF-8 encode, don't base64-decode.
                if (pk.user && pk.user.id) pk.user.id = new TextEncoder().encode(pk.user.id).buffer;
                if (Array.isArray(pk.excludeCredentials)) {
                    pk.excludeCredentials = pk.excludeCredentials.map(function (c) {
                        return Object.assign({}, c, { id: b64urlToBytes(c.id) });
                    });
                }
                accountCell.textContent = (pk.user && pk.user.name) || "(unknown)";

                const cred = await navigator.credentials.create({ publicKey: pk });
                if (!cred) throw new Error("authenticator returned no credential");

                // Re-serialise the credential into the shape the gateway
                // can parse via protocol.ParseCredentialCreationResponseBody.
                const attestation = {
                    id: cred.id,
                    rawId: bytesToB64url(cred.rawId),
                    type: cred.type,
                    response: {
                        attestationObject: bytesToB64url(cred.response.attestationObject),
                        clientDataJSON:    bytesToB64url(cred.response.clientDataJSON),
                    },
                    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
                };

                const result = await finishCeremony(attestation);
                setStatus("ok", "Passkey registered. You can now sign in on this device.");
                signInLink.hidden = false;
                btn.disabled = true;
                btn.textContent = "Registered";
                if (result && result.canonical_id) accountCell.textContent = result.canonical_id;
            } catch (e) {
                console.warn("enroll failed:", e);
                // A begin failure (or no options) means the LINK is the
                // problem — expired/used/wrong-domain — not the passkey.
                // The gateway collapses all of those into one generic
                // error, so give the user the actionable next step
                // instead of a raw status code.
                if (e && (e.phase === "begin" || /no publicKey options/.test(e.message || ""))) {
                    setStatus("err", "This enrollment link is invalid or has expired (they're good for 48 hours and one use). Ask your admin for a fresh link.");
                } else {
                    setStatus("err", "Couldn't register the passkey: " + (e.message || e));
                }
                btn.disabled = false;
            }
        }

        btn.disabled = false;
        btn.addEventListener("click", register);
    })();
