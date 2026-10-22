import { createHash, randomBytes } from "node:crypto";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import {
  ChangeFollower,
  MemoryQueueStorage,
  PDriveClient,
  PDriveError,
  UploadQueue,
  bytesSource,
  hashSource,
  parsePairing,
  uploadFile,
  type Change,
} from "../src/index.js";
import { startServer, type TestServer } from "./server.js";

let srv: TestServer;
let alice: PDriveClient;
let bob: PDriveClient;

beforeAll(async () => {
  srv = await startServer();
  alice = new PDriveClient({ baseUrl: srv.url, deviceName: "alice-laptop" });
  bob = new PDriveClient({ baseUrl: srv.url, deviceName: "bob-phone" });
  await alice.login("alice", "alice test password");
  await bob.login("bob", "bob test password");
}, 60_000);

afterAll(async () => srv?.stop());

const sha = (b: Uint8Array) => createHash("sha256").update(b).digest("hex");

describe("PersonalDrive SDK against a real server", () => {
  it("hashes like the server", async () => {
    const data = randomBytes(9 << 20);
    expect(await hashSource(bytesSource(data))).toBe(sha(data));
  });

  it("uploads in parts, downloads, and dedups across users", async () => {
    const data = randomBytes(20 << 20); // 3 parts at the default 8 MiB
    const phases = new Set<string>();
    const res = await uploadFile(alice, bytesSource(data, new Date("2024-05-01T10:00:00Z")), {
      path: "Photos/big.raw",
      onProgress: (p) => phases.add(p.phase),
    });
    expect(res.file.size).toBe(data.byteLength);
    expect(res.file.sha256).toBe(sha(data));
    expect(res.file.mtime).toBe("2024-05-01T10:00:00Z");
    expect([...phases]).toEqual(["hashing", "uploading", "finishing"]);

    const body = new Uint8Array(await (await alice.download(res.file.id!)).arrayBuffer());
    expect(sha(body)).toBe(sha(data));

    const dup = await uploadFile(bob, bytesSource(data), { path: "Camera/IMG_0001.raw" });
    expect(dup.deduplicated).toBe(true);
    expect((await bob.have([sha(data), "0".repeat(64)])).has(sha(data))).toBe(true);
  });

  it("resumes an interrupted upload without resending parts", async () => {
    const data = randomBytes(17 << 20);
    const ctrl = new AbortController();
    let session: string | undefined;
    let partsSent = 0;
    await expect(
      uploadFile(alice, bytesSource(data), {
        path: "Videos/clip.mp4",
        concurrency: 1,
        signal: ctrl.signal,
        onSession: (s) => (session = s.uploadId),
        onProgress: (p) => {
          if (p.phase === "uploading" && p.bytesDone > 0 && ++partsSent === 1) ctrl.abort();
        },
      }),
    ).rejects.toThrow();
    expect(session).toBeDefined();

    let firstReported = -1;
    const res = await uploadFile(alice, bytesSource(data), {
      path: "Videos/clip.mp4",
      uploadId: session,
      onProgress: (p) => {
        if (p.phase === "uploading" && firstReported < 0) firstReported = p.bytesDone;
      },
    });
    expect(firstReported).toBeGreaterThan(0); // the part sent before the abort counted as done
    expect(res.file.sha256).toBe(sha(data));
  });

  it("keeps both versions on conflict", async () => {
    const v1 = await uploadFile(alice, bytesSource(new TextEncoder().encode("v1")), { path: "notes.txt" });
    const v2 = await uploadFile(alice, bytesSource(new TextEncoder().encode("v2 laptop")), {
      path: "notes.txt",
      baseVersion: v1.file.version,
    });
    expect(v2.conflict).toBe(false);
    expect(v2.file.version).toBe(2);
    const stale = await uploadFile(alice, bytesSource(new TextEncoder().encode("v2 phone")), {
      path: "notes.txt",
      baseVersion: v1.file.version,
    });
    expect(stale.conflict).toBe(true);
    expect(stale.file.path).toMatch(/^notes \(conflict from alice-laptop .*\)\.txt$/);
  });

  it("follows the change feed", async () => {
    const start = await alice.cursor();
    const seen: Change[] = [];
    const follower = new ChangeFollower(alice, { cursor: start, wait: 5, onChanges: (c) => void seen.push(...c) }).start();
    await alice.mkdir("Shared Docs");
    await uploadFile(alice, bytesSource(new TextEncoder().encode("hello")), { path: "Shared Docs/hello.txt" });
    for (let i = 0; i < 50 && seen.length < 2; i++) await new Promise((r) => setTimeout(r, 100));
    await follower.stop();
    expect(seen.map((c) => `${c.op} ${c.path}`)).toEqual(["create Shared Docs", "create Shared Docs/hello.txt"]);
    expect(follower.cursor).toBeGreaterThan(start);
  });

  it("isolates users and enforces space permissions", async () => {
    const mine = await alice.listAll("Photos");
    await expect(bob.file(mine[0]!.id!)).rejects.toMatchObject({ status: 404 });

    const space = await alice.admin.createSpace("Family");
    const users = await alice.admin.users();
    const bobId = users.find((u) => u.username === "bob")!.id;
    await alice.admin.setMember(space.id!, bobId!, "read");
    await uploadFile(alice, bytesSource(new TextEncoder().encode("trip")), { path: "trip.jpg", spaceId: space.id! });
    expect((await bob.listAll("", space.id!)).map((f) => f.name)).toEqual(["trip.jpg"]);
    await expect(
      uploadFile(bob, bytesSource(new TextEncoder().encode("nope")), { path: "x.jpg", spaceId: space.id! }),
    ).rejects.toMatchObject({ code: "forbidden" });
    await expect(bob.admin.users()).rejects.toBeInstanceOf(PDriveError);
  });

  it("trash and restore", async () => {
    const f = await uploadFile(alice, bytesSource(new TextEncoder().encode("bill")), { path: "Bills/power.pdf" });
    await alice.trash(f.file.id!);
    const trash = await alice.listTrash();
    expect(trash.items.some((t) => t.id === f.file.id)).toBe(true);
    const back = await alice.restoreFromTrash(f.file.id!);
    expect(back.path).toBe("Bills/power.pdf");
  });

  it("refreshes an expired access token transparently", async () => {
    // Corrupt the in-memory access token: the client must refresh and retry once.
    const c = new PDriveClient({ baseUrl: srv.url });
    const t = await c.login("bob", "bob test password");
    const store = (c as unknown as { tokens: typeof t }).tokens;
    store.access_token = "v1.bogus.bogus";
    const me = await c.me();
    expect(me.user.username).toBe("bob");
    expect((c as unknown as { tokens: typeof t }).tokens.refresh_token).not.toBe(t.refresh_token);
  });

  it("pairs a new device from a QR payload", async () => {
    const pt = await alice.createPairingToken();
    const payload = parsePairing(pt.payload);
    const phone = new PDriveClient({ baseUrl: srv.url, deviceName: "Pixel 9" });
    await phone.pair(payload.token);
    expect((await phone.me()).user.username).toBe("alice");
    await expect(new PDriveClient({ baseUrl: srv.url }).pair(payload.token)).rejects.toMatchObject({ status: 401 });
  });

  it("runs a persistent upload queue that survives a restart", async () => {
    const files = new Map([
      ["a", randomBytes(1 << 20)],
      ["b", randomBytes(2 << 20)],
      ["gone", randomBytes(10)],
    ]);
    const storage = new MemoryQueueStorage();
    const open = async (it: { ref: string }) => (it.ref === "gone" ? null : bytesSource(files.get(it.ref)!));
    const q1 = new UploadQueue(bob, { storage, open });
    for (const ref of files.keys()) await q1.add({ ref, path: `Queue/${ref}.bin` });

    // "Restart": a new queue instance over the same storage.
    const done: string[] = [];
    const q2 = new UploadQueue(bob, { storage, open, concurrency: 2, onItem: (it) => it.state === "done" && done.push(it.ref) });
    await q2.run();
    expect(done.sort()).toEqual(["a", "b"]);
    expect(await q2.stats()).toEqual({ pending: 0, uploading: 0, done: 2, failed: 0 });
    expect((await bob.listAll("Queue")).map((f) => f.name)).toEqual(["a.bin", "b.bin"]);
  });
});
