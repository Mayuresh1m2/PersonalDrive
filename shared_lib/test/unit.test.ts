import { describe, expect, it, vi } from "vitest";

import { MemoryQueueStorage, PDriveClient, PDriveError, UploadQueue, bytesSource, findReachableUrl, parsePairing } from "../src/index.js";

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const tokens = (n: number) => ({
  access_token: `access-${n}`,
  access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  refresh_token: `refresh-${n}`,
  refresh_expires_at: new Date(Date.now() + 86_400_000).toISOString(),
  device_id: "d",
  user_id: 1,
});

describe("PDriveClient", () => {
  it("refreshes once for concurrent 401s and retries", async () => {
    let refreshes = 0;
    const f = vi.fn(async (url: string | URL | Request, init?: RequestInit) => {
      const u = String(url);
      const auth = (init?.headers as Record<string, string>)?.["Authorization"];
      if (u.endsWith("/auth/login")) return json(200, tokens(1));
      if (u.endsWith("/auth/refresh")) {
        refreshes++;
        return json(200, tokens(2));
      }
      if (auth === "Bearer access-1") return json(401, { error: { code: "unauthorized", message: "expired" } });
      return json(200, { user: { username: "x" } });
    });
    const c = new PDriveClient({ baseUrl: "http://s", fetch: f as unknown as typeof fetch });
    await c.login("x", "y");
    await Promise.all([c.me(), c.me(), c.me()]);
    expect(refreshes).toBe(1);
  });

  it("signs out when the refresh token is rejected", async () => {
    const onSignedOut = vi.fn();
    const f = async (url: string | URL | Request) =>
      String(url).endsWith("/auth/login") ? json(200, tokens(1)) : json(401, { error: { code: "unauthorized", message: "no" } });
    const c = new PDriveClient({ baseUrl: "http://s", fetch: f as typeof fetch, onSignedOut });
    await c.login("x", "y");
    await expect(c.me()).rejects.toMatchObject({ status: 401 });
    expect(onSignedOut).toHaveBeenCalled();
    expect(await c.isSignedIn()).toBe(false);
  });

  it("maps network failures to retryable errors", async () => {
    const c = new PDriveClient({ baseUrl: "http://s", fetch: (async () => { throw new TypeError("ECONNREFUSED"); }) as typeof fetch });
    const err = await c.serverInfo().catch((e) => e);
    expect(err).toBeInstanceOf(PDriveError);
    expect(err.retryable).toBe(true);
    expect(err.code).toBe("network");
  });
});

describe("pairing", () => {
  it("parses and validates QR payloads", () => {
    const p = parsePairing(JSON.stringify({ v: 1, name: "home", urls: ["https://a"], token: "t", ca_sha256: "ab" }));
    expect(p.token).toBe("t");
    expect(() => parsePairing("hello")).toThrow();
    expect(() => parsePairing(JSON.stringify({ v: 2 }))).toThrow();
  });

  it("picks the reachable URL with the right CA", async () => {
    const f = (async (url: string | URL | Request) => {
      const u = String(url);
      if (u.startsWith("https://down")) throw new TypeError("unreachable");
      if (u.startsWith("https://other")) return json(200, { ca_sha256: "zz" });
      return json(200, { ca_sha256: "ab" });
    }) as typeof fetch;
    expect(await findReachableUrl(["https://down", "https://other", "https://home.local"], { fetch: f, caSha256: "ab" })).toBe(
      "https://home.local",
    );
    expect(await findReachableUrl(["https://down"], { fetch: f })).toBeNull();
  });
});

describe("UploadQueue", () => {
  it("backs off on retryable errors and fails permanently on others", async () => {
    let calls = 0;
    const f = (async (url: string | URL | Request, init?: RequestInit) => {
      const u = String(url);
      if (u.endsWith("/auth/login")) return json(200, tokens(1));
      calls++;
      if (String(init?.body).includes("server-error")) return json(503, { error: { code: "library_unavailable", message: "disk unplugged" } });
      return json(400, { error: { code: "invalid_path", message: "bad" } });
    }) as typeof fetch;
    const c = new PDriveClient({ baseUrl: "http://s", fetch: f });
    await c.login("x", "y");
    const storage = new MemoryQueueStorage();
    const q = new UploadQueue(c, { storage, open: async () => bytesSource(new Uint8Array([1, 2, 3])) });
    await q.add({ ref: "1", path: "server-error.txt" });
    await q.add({ ref: "2", path: "bad\u0000path" });
    vi.useFakeTimers({ toFake: ["setTimeout"] });
    const run = q.run();
    await vi.runAllTimersAsync();
    await run;
    vi.useRealTimers();
    const items = await q.list();
    expect(items[0]!.state).toBe("pending"); // will retry later
    expect(items[0]!.nextAttemptAt).toBeGreaterThan(Date.now());
    expect(items[1]!.state).toBe("failed");
    expect(calls).toBeGreaterThan(2);
  });
});
