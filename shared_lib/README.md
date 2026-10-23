# @pdrive/client — shared client library

The OpenAPI contract and the TypeScript SDK used by every PersonalDrive client
(web, mobile, and anything else).

- `api/openapi.yaml`: the server API. **Edit this first** when the API changes,
  then run `pnpm generate` to refresh `src/schema.d.ts`.
- `src/client.ts`: `PDriveClient`. Handles login, setup, QR pairing, token refresh
  with refresh-token rotation, and typed helpers for every endpoint.
- `src/uploads.ts`: `uploadFile()`. The resumable upload protocol: hash, start
  (completes instantly when deduplicated), send missing parts in parallel, complete.
  Retries and expired sessions are handled. `FileSource` adapters cover browser
  Files and in-memory bytes; platforms add their own (disk paths, photo assets).
- `src/queue.ts`: `UploadQueue`. A persistent queue that survives app restarts,
  resumes half-sent uploads, backs off on errors, and pauses when `canRun()` says no
  (e.g. not on Wi-Fi).
- `src/sync.ts`: `ChangeFollower`. Long-polls the change feed from a saved cursor.
- `src/pairing.ts`: parses pairing QR codes and finds the reachable server URL.

Hashing defaults to pure-JS SHA-256 (`@noble/hashes`), which works in browsers,
Node and React Native/Hermes. Pass a faster `HasherFactory` where available.

```ts
import { PDriveClient, uploadFile, blobSource } from "@pdrive/client";

const client = new PDriveClient({ baseUrl: "https://home.local:8443", deviceName: "My laptop" });
await client.login("alice", "…");
const res = await uploadFile(client, blobSource(file), { path: `Uploads/${file.name}` });
console.log(res.deduplicated ? "already on the server" : "uploaded", res.file.path);
```

## Tests

`pnpm test` runs unit tests plus integration tests against a real `pdrive` server
started from `server/bin/pdrive` (built automatically if missing).
