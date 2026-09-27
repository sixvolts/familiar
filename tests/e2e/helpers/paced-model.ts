// paced-model.ts — a stand-in chat model that streams a long reply fast:
// a token every few milliseconds, the rate at which a reader's scroll and
// a token routinely land in the same frame. For the scroll-follow specs,
// which need that timing every run; a real model only produces it
// sometimes.

import * as http from "node:http";
import { AddressInfo } from "node:net";

export interface PacedModel {
    url: string;
    close: () => Promise<void>;
}

export async function startPacedModel(opts: { tokens?: number; everyMs?: number } = {}): Promise<PacedModel> {
    const tokens = opts.tokens ?? 3000;
    const everyMs = opts.everyMs ?? 3;
    const chunk = (delta: object, finish: string | null = null) =>
        `data: ${JSON.stringify({ choices: [{ index: 0, delta, finish_reason: finish }] })}\n\n`;
    const server = http.createServer(async (req, res) => {
        const url = req.url ?? "";
        if (url.startsWith("/v1/models") || url === "/health") {
            res.writeHead(200, { "content-type": "application/json" });
            res.end('{"object":"list","data":[{"id":"paced","object":"model"}]}');
            return;
        }
        if (!url.startsWith("/v1/chat/completions")) {
            res.writeHead(404);
            res.end();
            return;
        }
        let body = "";
        for await (const part of req) body += part;
        if (!/"stream"\s*:\s*true/.test(body)) {
            res.writeHead(200, { "content-type": "application/json" });
            res.end(JSON.stringify({ choices: [{ index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" }] }));
            return;
        }
        res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
        for (let i = 0; i < tokens && !res.destroyed; i++) {
            res.write(chunk({ content: i % 12 === 11 ? "word.\n\n" : "word " }));
            await new Promise((r) => setTimeout(r, everyMs));
        }
        res.write(chunk({}, "stop"));
        res.end("data: [DONE]\n\n");
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const port = (server.address() as AddressInfo).port;
    return {
        url: `http://127.0.0.1:${port}`,
        close: () =>
            new Promise<void>((resolve) => {
                server.closeAllConnections();
                server.close(() => resolve());
            }),
    };
}
