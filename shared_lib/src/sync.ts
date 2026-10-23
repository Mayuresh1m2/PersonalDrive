import { PDriveClient, PDriveError, type Change } from "./client.js";

export interface FollowOptions {
  /** Start after this cursor. Get one with client.cursor() after a full listing. */
  cursor: number;
  /** Receives each batch in order; the cursor only advances after it resolves. */
  onChanges: (changes: Change[], cursor: number) => void | Promise<void>;
  /** Persist the cursor so the next app launch resumes from it. */
  onCursor?: (cursor: number) => void | Promise<void>;
  onError?: (e: unknown) => void;
  /** Long-poll wait in seconds (server max 60). */
  wait?: number;
}

/**
 * Follows the server's change feed with long-polling until stop() is called.
 * Network errors back off exponentially (1s → 60s) and never lose position.
 */
export class ChangeFollower {
  private ctrl = new AbortController();
  private running: Promise<void> | null = null;
  cursor: number;

  constructor(
    private readonly client: PDriveClient,
    private readonly o: FollowOptions,
  ) {
    this.cursor = o.cursor;
  }

  start(): this {
    if (!this.running) this.running = this.loop();
    return this;
  }

  async stop(): Promise<void> {
    this.ctrl.abort();
    await this.running?.catch(() => {});
    this.running = null;
    this.ctrl = new AbortController();
  }

  private async loop() {
    let backoff = 1000;
    const signal = this.ctrl.signal;
    while (!signal.aborted) {
      try {
        const res = await this.client.changes(this.cursor, { wait: this.o.wait ?? 30, signal });
        if (res.changes.length > 0) {
          await this.o.onChanges(res.changes, res.cursor);
          this.cursor = res.cursor;
          await this.o.onCursor?.(this.cursor);
        }
        backoff = 1000;
        if (res.has_more) continue;
      } catch (e) {
        if (signal.aborted) return;
        this.o.onError?.(e);
        if (e instanceof PDriveError && e.status === 401) return; // signed out
        await sleep(backoff, signal);
        backoff = Math.min(backoff * 2, 60_000);
      }
    }
  }
}

function sleep(ms: number, signal: AbortSignal) {
  return new Promise<void>((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(t);
      resolve();
    });
  });
}
