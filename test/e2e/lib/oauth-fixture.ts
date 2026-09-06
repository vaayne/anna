import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { type IncomingMessage, type ServerResponse } from "node:http";
import { URL } from "node:url";
import { startFixtureServer } from "./fixture-server.ts";

export interface OAuthFixture {
  url: string;
  counters: Map<string, number>;
  issuedAccessTokens: Set<string>;
  revokedAccessTokens: Set<string>;
  tokenStatus: number;
  tokenBody: string;
  expiresIn: number;
  refreshExpiresIn: number;
  resource: string;
  close(): Promise<void>;
}

function json(res: ServerResponse, value: unknown, status = 200): void {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(value));
}

function count(fixture: OAuthFixture, endpoint: string): void {
  fixture.counters.set(endpoint, (fixture.counters.get(endpoint) ?? 0) + 1);
}

function pkceChallenge(verifier: string): string {
  return createHash("sha256").update(verifier).digest("base64url");
}

async function body(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks).toString("utf8");
}

async function form(req: IncomingMessage): Promise<URLSearchParams> {
  return new URLSearchParams(await body(req));
}

function basicCredentials(req: IncomingMessage): { clientID: string; clientSecret: string; } | undefined {
  const header = req.headers.authorization ?? "";
  if (!header.startsWith("Basic ")) return undefined;
  const decoded = Buffer.from(header.slice("Basic ".length), "base64").toString("utf8");
  const separator = decoded.indexOf(":");
  if (separator < 0) return undefined;
  return { clientID: decoded.slice(0, separator), clientSecret: decoded.slice(separator + 1) };
}

// Loopback OAuth 2.1 authorization server. It auto-approves /authorize, which
// keeps the browser test deterministic while still exercising redirect + PKCE.
export async function startOAuthFixture(): Promise<OAuthFixture> {
  const fixture: OAuthFixture = {
    url: "",
    counters: new Map(),
    issuedAccessTokens: new Set(),
    revokedAccessTokens: new Set(),
    tokenStatus: 0,
    tokenBody: "",
    expiresIn: 3600,
    refreshExpiresIn: 3600,
    resource: "",
    close: async () => {},
  };
  const codes = new Map<string, { challenge: string; redirectURI: string; clientID: string; resource: string; }>();
  const fixtureServer = await startFixtureServer(async (req, res) => {
    try {
      const path = new URL(req.url ?? "/", "http://127.0.0.1").pathname;
      if (req.method === "GET" && path === "/.well-known/oauth-authorization-server") {
        count(fixture, "metadata");
        return json(res, {
          issuer: fixture.url,
          authorization_endpoint: `${fixture.url}/authorize`,
          token_endpoint: `${fixture.url}/token`,
          registration_endpoint: `${fixture.url}/register`,
          code_challenge_methods_supported: ["S256"],
        });
      }
      if (req.method === "GET" && path === "/.well-known/oauth-protected-resource") {
        count(fixture, "protected-resource");
        return json(res, {
          resource: fixture.resource,
          authorization_servers: [fixture.url],
          scopes_supported: ["mcp:read"],
        });
      }
      if (req.method === "POST" && path === "/register") {
        count(fixture, "register");
        const raw = await body(req);
        const metadata = raw ? JSON.parse(raw) as { redirect_uris?: string[]; } : {};
        return json(res, {
          client_id: "e2e-client",
          client_secret: "e2e-secret",
          redirect_uris: metadata.redirect_uris ?? [],
          grant_types: ["authorization_code", "refresh_token"],
        });
      }
      if (req.method === "GET" && path === "/authorize") {
        count(fixture, "authorize");
        const query = new URL(req.url ?? "/", fixture.url).searchParams;
        const code = `code-${randomBytes(8).toString("hex")}`;
        codes.set(code, {
          challenge: query.get("code_challenge") ?? "",
          redirectURI: query.get("redirect_uri") ?? "",
          clientID: query.get("client_id") ?? "",
          resource: query.get("resource") ?? "",
        });
        const redirect = new URL(query.get("redirect_uri") ?? "http://127.0.0.1/");
        redirect.searchParams.set("code", code);
        redirect.searchParams.set("state", query.get("state") ?? "");
        res.writeHead(302, { Location: redirect.toString() });
        return res.end();
      }
      if (req.method === "POST" && path === "/token") {
        count(fixture, "token");
        const body = await form(req);
        const credentials = basicCredentials(req);
        const clientID = body.get("client_id") ?? credentials?.clientID ?? "";
        const clientSecret = body.get("client_secret") ?? credentials?.clientSecret ?? "";
        if (clientID !== "e2e-client" || clientSecret !== "e2e-secret") {
          return json(res, { error: "invalid_client" }, 401);
        }
        if (fixture.tokenStatus) return json(res, fixture.tokenBody ? JSON.parse(fixture.tokenBody) : {}, fixture.tokenStatus);
        if (body.get("grant_type") === "authorization_code") {
          const code = codes.get(body.get("code") ?? "");
          if (
            !code || code.clientID !== clientID
            || code.challenge !== pkceChallenge(body.get("code_verifier") ?? "") || !body.get("resource")
          ) {
            return json(res, { error: "invalid_grant" }, 400);
          }
          codes.delete(body.get("code")!);
        } else if (body.get("grant_type") !== "refresh_token" || body.get("refresh_token") !== "e2e-refresh") {
          return json(res, { error: "invalid_grant" }, 400);
        }
        fixture.issuedAccessTokens.add("e2e-access");
        fixture.revokedAccessTokens.delete("e2e-access");
        return json(res, {
          access_token: "e2e-access",
          refresh_token: "e2e-refresh",
          token_type: "Bearer",
          expires_in: body.get("grant_type") === "refresh_token" ? fixture.refreshExpiresIn : fixture.expiresIn,
          scope: "mcp:read",
        });
      }
      return json(res, { error: "not found" }, 404);
    } catch (error) {
      return json(res, { error: String(error) }, 500);
    }
  });
  fixture.url = fixtureServer.state.url;
  fixture.close = fixtureServer.close;
  return fixture;
}

export function bearerAccepted(fixture: OAuthFixture, token: string): boolean {
  return fixture.issuedAccessTokens.has(token) && !fixture.revokedAccessTokens.has(token);
}

export function expireAccessToken(fixture: OAuthFixture): void {
  fixture.revokedAccessTokens.add("e2e-access");
}

export function setTokenFailure(fixture: OAuthFixture, status: number, body: string): void {
  fixture.tokenStatus = status;
  fixture.tokenBody = body;
}

export function tokenHits(fixture: OAuthFixture): number {
  return fixture.counters.get("token") ?? 0;
}

export function safeEqual(a: string, b: string): boolean {
  const aa = Buffer.from(a);
  const bb = Buffer.from(b);
  return aa.length === bb.length && timingSafeEqual(aa, bb);
}
