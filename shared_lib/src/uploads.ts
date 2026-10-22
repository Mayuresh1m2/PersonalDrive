import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";

import { PDriveClient, PDriveError, type FileEntry, type UploadState } from "./client.js";

/**
 * Something we can upload: a browser File, a file on disk, a photo asset…
 * slice() must return exactly end-start bytes.
 */
export interface FileSource {
  size: number;
  mtime?: Date;
  slice(start: number, end: number): Promise<Uint8Array>;
}

/** Incremental SHA-256. The default is pure JS (works everywhere, incl. Hermes). */
export interface Hasher {
  update(data: Uint8Array): void;
  digestHex(): string | Promise<string>;
}

export type HasherFactory = () => Hasher | Promise<Hasher>;

export const jsSha256: HasherFactory = () => {
  const h = sha256.create();
  return { update: (d) => void h.update(d), digestHex: () => bytesToHex(h.digest()) };
};

/** Wraps a browser File/Blob as a FileSource. */
export function blobSource(b: Blob & { lastModified?: number }): FileSource {
  return {
    size: b.size,
    mtime: b.lastModified ? new Date(b.lastModified) : undefined,
    slice: async (s, e) => new Uint8Array(await b.slice(s, e).arrayBuffer()),
  };
}

/** Wraps in-memory bytes as a FileSource (tests, small generated files). */
export function bytesSource(data: Uint8Array, mtime?: Date): FileSource {
  return { size: data.byteLength, mtime, slice: async (s, e) => data.subarray(s, e) };
}

const HASH_CHUNK = 4 << 20;

/** Hashes a source in 4 MiB slices. */
export async function hashSource(
  src: FileSource,
  hasher: HasherFactory = jsSha256,
  onProgress?: (done: number) => void,
  signal?: AbortSignal,
): Promise<string> {
  const h = await hasher();
  for (let off = 0; off < src.size; off += HASH_CHUNK) {
    signal?.throwIfAborted();
    h.update(await src.slice(off, Math.min(off + HASH_CHUNK, src.size)));
    onProgress?.(Math.min(off + HASH_CHUNK, src.size));
  }
  return h.digestHex();
}

export interface UploadOptions {
  path: string;
  spaceId?: number;
  /** Version the client last synced; omit for new files. A mismatch creates a conflict copy. */
  baseVersion?: number;
  /** Precomputed SHA-256 (hex), e.g. from a previous attempt. */
  sha256?: string;
  /** Resume this upload session if it still exists. */
  uploadId?: string;
  hasher?: HasherFactory;
  /** Parallel part uploads. */
  concurrency?: number;
  signal?: AbortSignal;
  onProgress?: (p: UploadProgress) => void;
  /** Called once the session exists, so callers can persist it for resuming after a restart. */
  onSession?: (s: { uploadId: string; sha256: string }) => void;
}

export interface UploadProgress {
  phase: "hashing" | "uploading" | "finishing";
  bytesDone: number;
  bytesTotal: number;
}

export interface UploadResult {
  file: FileEntry;
  /** The server already had the content: nothing (or little) was transferred. */
  deduplicated: boolean;
  /** Stored under a "(conflict from …)" name because the file changed on the server. */
  conflict: boolean;
  unchanged: boolean;
}

function done(s: UploadState): UploadResult {
  if (!s.file) throw new PDriveError(500, "bad_response", "server completed the upload without a file");
  return { file: s.file, deduplicated: !!s.deduplicated, conflict: !!s.conflict, unchanged: !!s.unchanged };
}

async function retry<T>(fn: () => Promise<T>, signal?: AbortSignal, attempts = 5): Promise<T> {
  let delay = 500;
  for (let i = 1; ; i++) {
    try {
      return await fn();
    } catch (e) {
      if (!(e instanceof PDriveError) || !e.retryable || i >= attempts) throw e;
      signal?.throwIfAborted();
      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 2, 15_000);
    }
  }
}

/**
 * Uploads a file with the resumable protocol:
 * hash → start (may finish instantly via dedup) → missing parts → complete.
 * Expired sessions are restarted transparently.
 */
export async function uploadFile(client: PDriveClient, src: FileSource, o: UploadOptions): Promise<UploadResult> {
  const report = (phase: UploadProgress["phase"], bytesDone: number) =>
    o.onProgress?.({ phase, bytesDone, bytesTotal: src.size });

  const sha = o.sha256 ?? (await hashSource(src, o.hasher, (d) => report("hashing", d), o.signal));

  let session: UploadState | null = null;
  if (o.uploadId) {
    try {
      session = await client.request<UploadState>("GET", `/api/v1/uploads/${o.uploadId}`, { signal: o.signal });
    } catch (e) {
      if (!(e instanceof PDriveError) || (e.status !== 404 && e.status !== 410)) throw e;
    }
  }
  if (!session) {
    session = await retry(
      () =>
        client.request<UploadState>("POST", "/api/v1/uploads", {
          body: {
            path: o.path,
            space_id: o.spaceId ?? 0,
            size: src.size,
            sha256: sha,
            mtime: (src.mtime ?? new Date()).toISOString(),
            base_version: o.baseVersion ?? null,
          },
          signal: o.signal,
        }),
      o.signal,
    );
    if (session.status === "completed") {
      report("finishing", src.size);
      return done(session);
    }
  }
  const id = session.upload_id!;
  o.onSession?.({ uploadId: id, sha256: sha });
  const partSize = session.part_size!;
  const parts = session.parts ?? 0;
  const received = new Set(session.received ?? []);
  let sent = [...received].reduce((n, i) => n + Math.min(partSize, src.size - i * partSize), 0);
  report("uploading", sent);

  const todo = Array.from({ length: parts }, (_, i) => i).filter((i) => !received.has(i));
  const worker = async () => {
    for (let n = todo.shift(); n !== undefined; n = todo.shift()) {
      const start = n * partSize;
      const data = await src.slice(start, Math.min(start + partSize, src.size));
      await retry(
        () =>
          client.request("PUT", `/api/v1/uploads/${id}/parts/${n}`, {
            raw: data as unknown as BodyInit,
            headers: { "Content-Type": "application/octet-stream" },
            signal: o.signal,
          }),
        o.signal,
      );
      sent += data.byteLength;
      report("uploading", sent);
    }
  };
  try {
    await Promise.all(Array.from({ length: Math.max(1, o.concurrency ?? 2) }, worker));
    report("finishing", src.size);
    const res = await retry(
      () => client.request<UploadState>("POST", `/api/v1/uploads/${id}/complete`, { signal: o.signal }),
      o.signal,
    );
    return done(res);
  } catch (e) {
    if (e instanceof PDriveError && e.status === 410) {
      // Session expired mid-way (e.g. phone offline for a day): start over.
      return uploadFile(client, src, { ...o, sha256: sha, uploadId: undefined });
    }
    throw e;
  }
}
