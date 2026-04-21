interface Env {
  OPENROUTER_API_KEY: string;
}

const ALLOWED_ORIGINS = [
  "https://walter-grace.github.io",
  "http://localhost",
  "http://127.0.0.1",
  "null", // local file:// opens as null origin
];

const SYSTEM_PROMPT = `You are an expert Cloudflare Worker developer who specializes in create-mcpay (pay-per-call agent gateways).

When given a handler function, rewrite it to follow the create-mcpay pattern EXACTLY:
1. Read body with bounded size check (16KB max) using req.text()
2. Parse and shape-validate the body — return error(400, ..., { note: "no charge applied" }) on bad input. NO charge happens yet.
3. Call: const auth = await authAndCharge(req, env, PRICE_MCENTS, "SCOPE");
         if (auth instanceof Response) return auth;
4. Do the actual work
5. Return json({ ok: true, ...result, balance_mcents: auth.record.balance_mcents })

Then output THREE blocks in sequence, each preceded by a comment header:

// ── 1. HANDLER ──
The rewritten async handler function.

// ── 2. ROUTER ENTRY ──
The single if-line to add to the router.

// ── 3. CONSTANTS ──
The entries to add to CallType, PRICE_MCENTS, XP_AWARD, and SCOPE_FOR tables.

Output ONLY code — no prose, no markdown fences, no explanation. Paste-ready into template.ts.`;

function corsHeaders(origin: string): Record<string, string> {
  const allowed = ALLOWED_ORIGINS.includes(origin) ? origin : ALLOWED_ORIGINS[0];
  return {
    "Access-Control-Allow-Origin": allowed,
    "Access-Control-Allow-Methods": "POST, OPTIONS",
    "Access-Control-Allow-Headers": "Content-Type",
    "Access-Control-Max-Age": "86400",
  };
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const origin = request.headers.get("Origin") ?? "null";
    const cors = corsHeaders(origin);

    if (request.method === "OPTIONS") {
      return new Response(null, { status: 204, headers: cors });
    }

    if (request.method !== "POST") {
      return new Response("Method Not Allowed", { status: 405, headers: cors });
    }

    const url = new URL(request.url);
    if (url.pathname !== "/transform") {
      return new Response("Not Found", { status: 404, headers: cors });
    }

    const raw = await request.text();
    if (raw.length > 32 * 1024) {
      return new Response(JSON.stringify({ error: "body too large" }), {
        status: 413,
        headers: { "Content-Type": "application/json", ...cors },
      });
    }

    let body: { code?: string; name?: string; price?: number; scope?: string };
    try {
      body = JSON.parse(raw);
    } catch {
      return new Response(JSON.stringify({ error: "invalid JSON" }), {
        status: 400,
        headers: { "Content-Type": "application/json", ...cors },
      });
    }

    const { code, name = "mytool", price = 100, scope = "mytool" } = body;
    if (!code || typeof code !== "string") {
      return new Response(JSON.stringify({ error: "missing code" }), {
        status: 400,
        headers: { "Content-Type": "application/json", ...cors },
      });
    }

    const userMsg = `Transform this handler. Use these values:
- Endpoint: /v1/${name}  Method: POST  Price: ${price} mcents  Scope: "${scope}"  XP: ${Math.round(price / 10)}

Handler to transform:

${code}`;

    const FREE_MODELS = [
      "qwen/qwen3-coder:free",
      "google/gemma-4-26b-a4b-it:free",
      "meta-llama/llama-3.3-70b-instruct:free",
      "nvidia/nemotron-3-super-120b-a12b:free",
      "nousresearch/hermes-3-llama-3.1-405b:free",
      "google/gemma-3-27b-it:free",
    ];

    const payload = (model: string) => JSON.stringify({
      model,
      max_tokens: 2048,
      stream: true,
      messages: [
        { role: "system", content: SYSTEM_PROMPT },
        { role: "user", content: userMsg },
      ],
    });

    let upstream: Response | null = null;
    let lastErr = "";
    for (const model of FREE_MODELS) {
      const resp = await fetch("https://openrouter.ai/api/v1/chat/completions", {
        method: "POST",
        headers: {
          "Authorization": `Bearer ${env.OPENROUTER_API_KEY}`,
          "Content-Type": "application/json",
          "HTTP-Referer": "https://walter-grace.github.io/create-mcpay",
          "X-Title": "create-mcpay transformer",
        },
        body: payload(model),
      });
      if (resp.ok) { upstream = resp; break; }
      lastErr = await resp.text();
      // only retry on rate-limit; bail on auth/bad-request errors
      if (resp.status !== 429) {
        return new Response(JSON.stringify({ error: `upstream ${resp.status}: ${lastErr}` }), {
          status: 502,
          headers: { "Content-Type": "application/json", ...cors },
        });
      }
    }

    if (!upstream) {
      return new Response(JSON.stringify({ error: `all models rate-limited: ${lastErr}` }), {
        status: 429,
        headers: { "Content-Type": "application/json", ...cors },
      });
    }

    // Pipe SSE stream straight through with CORS headers added
    const { readable, writable } = new TransformStream();
    upstream.body!.pipeTo(writable);

    return new Response(readable, {
      headers: {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-cache",
        "X-Accel-Buffering": "no",
        ...cors,
      },
    });
  },
};
