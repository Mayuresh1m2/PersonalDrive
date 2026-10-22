import { PDriveClient, PDriveError } from "./client.js";
import { uploadFile, type FileSource, type HasherFactory, type UploadResult } from "./uploads.js";

/** A queued upload. `ref` is whatever the platform needs to reopen the file (path, asset id…). */
export interface QueueItem {
  id: string;
  ref: string;
  path: string;
  spaceId?: number;
  baseVersion?: number;
  size?: number;
  sha256?: string;
  uploadId?: string;
  state: "pending" | "uploading" | "done" | "failed";
  attempts: number;
  lastError?: string;
  nextAttemptAt?: number;
  addedAt: number;
}

export interface QueueStorage {
  load(): Promise<QueueItem[]>;
  save(items: QueueItem[]): Promise<void>;
}

export class MemoryQueueStorage implements QueueStorage {
  items: QueueItem[] = [];
  async load() {
    return structuredClone(this.items);
  }
  async save(items: QueueItem[]) {
    this.items = structuredClone(items);
  }
}

export interface QueueOptions {
  storage: QueueStorage;
  /** Reopens a queued item's file. Return null if it no longer exists (item is dropped). */
  open: (item: QueueItem) => Promise<FileSource | null>;
  hasher?: HasherFactory;
  concurrency?: number;
  /** Checked before each upload, e.g. "on Wi-Fi and charging". */
  canRun?: () => Promise<boolean> | boolean;
  onItem?: (item: QueueItem, result?: UploadResult) => void;
  onProgress?: (item: QueueItem, bytesDone: number, bytesTotal: number) => void;
  maxAttempts?: number;
}

/**
 * A persistent upload queue: survives restarts (resuming half-finished
 * uploads), retries with backoff, and pauses when conditions aren't met.
 */
export class UploadQueue {
  private items: QueueItem[] = [];
  private loaded = false;
  private running = false;
  private ctrl = new AbortController();

  constructor(
    private readonly client: PDriveClient,
    private readonly o: QueueOptions,
  ) {}

  private async load() {
    if (this.loaded) return;
    this.items = await this.o.storage.load();
    // Anything "uploading" when the app died is retried.
    for (const it of this.items) if (it.state === "uploading") it.state = "pending";
    this.loaded = true;
  }

  private persist() {
    return this.o.storage.save(this.items);
  }

  async add(item: Omit<QueueItem, "id" | "state" | "attempts" | "addedAt"> & { id?: string }): Promise<QueueItem> {
    await this.load();
    const it: QueueItem = {
      ...item,
      id: item.id ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`,
      state: "pending",
      attempts: 0,
      addedAt: Date.now(),
    };
    this.items.push(it);
    await this.persist();
    return it;
  }

  async list(): Promise<QueueItem[]> {
    await this.load();
    return structuredClone(this.items);
  }

  async stats() {
    await this.load();
    const s = { pending: 0, uploading: 0, done: 0, failed: 0 };
    for (const it of this.items) s[it.state]++;
    return s;
  }

  /** Drops finished items. */
  async clearDone() {
    await this.load();
    this.items = this.items.filter((i) => i.state !== "done");
    await this.persist();
  }

  async retryFailed() {
    await this.load();
    for (const it of this.items) {
      if (it.state === "failed") Object.assign(it, { state: "pending", attempts: 0, nextAttemptAt: undefined });
    }
    await this.persist();
  }

  stop() {
    this.ctrl.abort();
    this.ctrl = new AbortController();
  }

  /** Processes pending items until none are ready. Safe to call repeatedly. */
  async run(): Promise<void> {
    if (this.running) return;
    this.running = true;
    try {
      await this.load();
      const signal = this.ctrl.signal;
      const next = () =>
        this.items.find((i) => i.state === "pending" && (i.nextAttemptAt ?? 0) <= Date.now());
      const worker = async () => {
        for (let it = next(); it && !signal.aborted; it = next()) {
          if (this.o.canRun && !(await this.o.canRun())) return;
          it.state = "uploading";
          await this.process(it, signal);
        }
      };
      await Promise.all(Array.from({ length: Math.max(1, this.o.concurrency ?? 1) }, worker));
    } finally {
      this.running = false;
    }
  }

  private async process(it: QueueItem, signal: AbortSignal) {
    try {
      const src = await this.o.open(it);
      if (!src) {
        this.items = this.items.filter((i) => i !== it);
        await this.persist();
        return;
      }
      it.size = src.size;
      const res = await uploadFile(this.client, src, {
        path: it.path,
        spaceId: it.spaceId,
        baseVersion: it.baseVersion,
        sha256: it.sha256,
        uploadId: it.uploadId,
        hasher: this.o.hasher,
        signal,
        onSession: async ({ uploadId, sha256 }) => {
          Object.assign(it, { uploadId, sha256 });
          await this.persist();
        },
        onProgress: (p) => this.o.onProgress?.(it, p.bytesDone, p.bytesTotal),
      });
      Object.assign(it, { state: "done", lastError: undefined, uploadId: undefined });
      await this.persist();
      this.o.onItem?.(it, res);
    } catch (e) {
      if (signal.aborted) {
        it.state = "pending";
        await this.persist();
        return;
      }
      it.attempts++;
      it.lastError = (e as Error).message;
      const permanent = e instanceof PDriveError && !e.retryable && e.status !== 409;
      if (permanent || it.attempts >= (this.o.maxAttempts ?? 8)) {
        it.state = "failed";
      } else {
        it.state = "pending";
        it.nextAttemptAt = Date.now() + Math.min(2 ** it.attempts * 1000, 30 * 60_000);
      }
      await this.persist();
      this.o.onItem?.(it);
    }
  }
}
