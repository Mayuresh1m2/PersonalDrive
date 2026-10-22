import type { components } from "./schema.js";

export type FileEntry = components["schemas"]["File"];
export type Tokens = components["schemas"]["Tokens"];
export type User = components["schemas"]["User"];
export type Device = components["schemas"]["Device"];
export type Space = components["schemas"]["Space"];
export type Change = components["schemas"]["Change"];
export type UploadState = components["schemas"]["Upload"];
export type ServerInfo = components["schemas"]["ServerInfo"];
export type Health = components["schemas"]["Health"];
export type PairingPayload = components["schemas"]["PairingPayload"];
export type BackupTargetStatus = components["schemas"]["BackupTarget"];
export type SnapshotStats = components["schemas"]["SnapshotStats"];

/** An API error with the server's machine-readable code. */
export class PDriveError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "PDriveError";
  }

  /** Errors worth retrying later (network, server busy, disk unplugged). */
  get retryable(): boolean {
    return this.status === 0 || this.status === 429 || this.status >= 500;
  }
}

/** Persists credentials between app launches (keychain, localStorage, file…). */
export interface TokenStore {
  load(): Promise<Tokens | null> | Tokens | null;
  save(tokens: Tokens | null): Promise<void> | void;
}

export class MemoryTokenStore implements TokenStore {
  private tokens: Tokens | null = null;
  load() {
    return this.tokens;
  }
  save(t: Tokens | null) {
    this.tokens = t;
  }
}

export interface ClientOptions {
  /** Server base URL, e.g. https://home.local:8443. "" for same-origin (web app). */
  baseUrl: string;
  tokens?: TokenStore;
  /** fetch implementation (inject one that pins the server CA on native platforms). */
  fetch?: typeof fetch;
  deviceName?: string;
  platform?: string;
  /** Called when the session can't be refreshed and the user must sign in again. */
  onSignedOut?: () => void;
}

type Query = Record<string, string | number | boolean | undefined>;

interface RequestOptions {
  query?: Query;
  body?: unknown;
  raw?: BodyInit;
  headers?: Record<string, string>;
  auth?: boolean;
  signal?: AbortSignal;
}

/**
 * PDriveClient talks to a PersonalDrive server: it signs in, keeps access
 * tokens fresh (refreshing once on 401 and rotating the refresh token), and
 * exposes typed helpers for every endpoint.
 */
export class PDriveClient {
  readonly baseUrl: string;
  private readonly store: TokenStore;
  private readonly fetchImpl: typeof fetch;
  private tokens: Tokens | null = null;
  private loaded = false;
  private refreshing: Promise<boolean> | null = null;

  constructor(private readonly opts: ClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, "");
    this.store = opts.tokens ?? new MemoryTokenStore();
    this.fetchImpl = opts.fetch ?? globalThis.fetch.bind(globalThis);
  }

  // ---- auth ------------------------------------------------------------------

  private async ensureLoaded() {
    if (!this.loaded) {
      this.tokens = (await this.store.load()) ?? null;
      this.loaded = true;
    }
  }

  async isSignedIn(): Promise<boolean> {
    await this.ensureLoaded();
    return this.tokens !== null;
  }

  async accessToken(): Promise<string | null> {
    await this.ensureLoaded();
    if (this.tokens && Date.parse(this.tokens.access_expires_at ?? "") - Date.now() < 30_000) {
      await this.refresh();
    }
    return this.tokens?.access_token ?? null;
  }

  private async setTokens(t: Tokens | null) {
    this.tokens = t;
    this.loaded = true;
    await this.store.save(t);
  }

  private device() {
    return { device_name: this.opts.deviceName ?? "PersonalDrive client", platform: this.opts.platform ?? "" };
  }

  async login(username: string, password: string): Promise<Tokens> {
    const t = await this.request<Tokens>("POST", "/api/v1/auth/login", {
      body: { username, password, ...this.device() },
      auth: false,
    });
    await this.setTokens(t);
    return t;
  }

  /** First-run: create the admin account with the setup code from the server log. */
  async setup(setupCode: string, username: string, password: string, displayName = ""): Promise<Tokens> {
    const t = await this.request<Tokens>("POST", "/api/v1/setup", {
      body: { setup_code: setupCode, username, password, display_name: displayName, ...this.device() },
      auth: false,
    });
    await this.setTokens(t);
    return t;
  }

  /** Sign in by scanning a pairing QR code (see parsePairing). */
  async pair(token: string): Promise<Tokens> {
    const t = await this.request<Tokens>("POST", "/api/v1/devices/pair", {
      body: { token, ...this.device() },
      auth: false,
    });
    await this.setTokens(t);
    return t;
  }

  async logout(): Promise<void> {
    try {
      await this.request("POST", "/api/v1/auth/logout");
    } finally {
      await this.setTokens(null);
    }
  }

  /** Refreshes tokens; concurrent callers share one refresh. */
  private refresh(): Promise<boolean> {
    if (!this.refreshing) {
      this.refreshing = (async () => {
        const rt = this.tokens?.refresh_token;
        if (!rt) return false;
        try {
          const t = await this.request<Tokens>("POST", "/api/v1/auth/refresh", {
            body: { refresh_token: rt },
            auth: false,
          });
          await this.setTokens(t);
          return true;
        } catch (e) {
          if (e instanceof PDriveError && e.status === 401) {
            await this.setTokens(null);
            this.opts.onSignedOut?.();
          }
          return false;
        } finally {
          this.refreshing = null;
        }
      })();
    }
    return this.refreshing;
  }

  // ---- transport ---------------------------------------------------------------

  url(path: string, query?: Query): string {
    let u = this.baseUrl + path;
    if (query) {
      const q = Object.entries(query)
        .filter(([, v]) => v !== undefined && v !== "")
        .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`)
        .join("&");
      if (q) u += "?" + q;
    }
    return u;
  }

  /** Low-level request returning the raw Response (for downloads). */
  async fetchRaw(method: string, path: string, o: RequestOptions = {}, retried = false): Promise<Response> {
    const headers: Record<string, string> = { ...o.headers };
    if (o.auth !== false) {
      const tok = await this.accessToken();
      if (tok) headers["Authorization"] = `Bearer ${tok}`;
    }
    let body: BodyInit | undefined = o.raw;
    if (o.body !== undefined) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(o.body);
    }
    let res: Response;
    try {
      res = await this.fetchImpl(this.url(path, o.query), { method, headers, body, signal: o.signal });
    } catch (e) {
      if ((e as Error).name === "AbortError") throw e;
      throw new PDriveError(0, "network", `can't reach the server: ${(e as Error).message}`);
    }
    if (res.status === 401 && o.auth !== false && !retried && (await this.refresh())) {
      return this.fetchRaw(method, path, o, true);
    }
    if (!res.ok) {
      let code = "http_" + res.status;
      let message = res.statusText;
      try {
        const j = (await res.json()) as { error?: { code: string; message: string } };
        if (j.error) ({ code, message } = j.error);
      } catch {
        /* not JSON */
      }
      throw new PDriveError(res.status, code, message);
    }
    return res;
  }

  async request<T = unknown>(method: string, path: string, o: RequestOptions = {}): Promise<T> {
    const res = await this.fetchRaw(method, path, o);
    if (res.status === 204) return undefined as T;
    const text = await res.text();
    return (text ? JSON.parse(text) : undefined) as T;
  }

  // ---- endpoints -----------------------------------------------------------------

  serverInfo() {
    return this.request<ServerInfo>("GET", "/api/v1/server", { auth: false });
  }

  me() {
    return this.request<{ user: User; device: Device; used_bytes: number; spaces: Space[] }>("GET", "/api/v1/me");
  }

  spaces() {
    return this.request<{ items: Space[] }>("GET", "/api/v1/spaces").then((r) => r.items);
  }

  /** Lists one folder page. Use listAll for everything. */
  list(path = "", opts: { spaceId?: number; after?: string; limit?: number } = {}) {
    return this.request<{ items: FileEntry[]; next_after?: string }>("GET", "/api/v1/files", {
      query: { path, space: opts.spaceId, after: opts.after, limit: opts.limit },
    });
  }

  async listAll(path = "", spaceId?: number): Promise<FileEntry[]> {
    const out: FileEntry[] = [];
    let after: string | undefined;
    do {
      const page = await this.list(path, { spaceId, after });
      out.push(...page.items);
      after = page.next_after;
    } while (after);
    return out;
  }

  file(id: number) {
    return this.request<FileEntry>("GET", `/api/v1/files/${id}`);
  }

  /** URL for <img>/<video> or downloads; the browser needs a token-bearing fetch, see download(). */
  contentPath(id: number) {
    return `/api/v1/files/${id}/content`;
  }

  async download(id: number, signal?: AbortSignal): Promise<Response> {
    return this.fetchRaw("GET", this.contentPath(id), { signal });
  }

  async thumbnail(id: number, size = 256, signal?: AbortSignal): Promise<Blob | null> {
    try {
      const res = await this.fetchRaw("GET", `/api/v1/files/${id}/thumbnail`, { query: { size }, signal });
      return await res.blob();
    } catch (e) {
      if (e instanceof PDriveError && e.status === 404) return null;
      throw e;
    }
  }

  move(id: number, path: string) {
    return this.request<FileEntry>("PATCH", `/api/v1/files/${id}`, { body: { path } });
  }

  trash(id: number) {
    return this.request<void>("DELETE", `/api/v1/files/${id}`);
  }

  mkdir(path: string, spaceId?: number) {
    return this.request<FileEntry>("POST", "/api/v1/folders", { body: { path, space_id: spaceId ?? 0 } });
  }

  listTrash(spaceId?: number) {
    return this.request<{ items: FileEntry[]; retention_days: number }>("GET", "/api/v1/trash", {
      query: { space: spaceId },
    });
  }

  restoreFromTrash(id: number) {
    return this.request<FileEntry>("POST", `/api/v1/trash/${id}/restore`);
  }

  purge(id: number) {
    return this.request<void>("DELETE", `/api/v1/trash/${id}`);
  }

  devices() {
    return this.request<{ items: Device[] }>("GET", "/api/v1/devices").then((r) => r.items);
  }

  revokeDevice(id: string) {
    return this.request<void>("DELETE", `/api/v1/devices/${id}`);
  }

  createPairingToken() {
    return this.request<{ token: string; expires_at: string; payload: string; qr_png: string }>(
      "POST",
      "/api/v1/devices/pairing-tokens",
    );
  }

  have(sha256: string[]) {
    return this.request<{ have: string[] }>("POST", "/api/v1/sync/have", { body: { sha256 } }).then(
      (r) => new Set(r.have),
    );
  }

  cursor() {
    return this.request<{ cursor: number }>("GET", "/api/v1/sync/cursor").then((r) => r.cursor);
  }

  changes(cursor: number, opts: { wait?: number; limit?: number; signal?: AbortSignal } = {}) {
    return this.request<{ changes: Change[]; cursor: number; has_more: boolean }>("GET", "/api/v1/sync/changes", {
      query: { cursor, wait: opts.wait, limit: opts.limit },
      signal: opts.signal,
    });
  }

  // ---- admin ------------------------------------------------------------------------

  admin = {
    users: () => this.request<{ items: (User & { used_bytes: number })[] }>("GET", "/api/v1/admin/users").then((r) => r.items),
    createUser: (u: { username: string; password: string; display_name?: string; role?: "admin" | "member"; quota_bytes?: number }) =>
      this.request<User>("POST", "/api/v1/admin/users", { body: u }),
    updateUser: (
      id: number,
      u: Partial<{ display_name: string; role: "admin" | "member"; quota_bytes: number; disabled: boolean; password: string }>,
    ) => this.request<User>("PATCH", `/api/v1/admin/users/${id}`, { body: u }),
    spaces: () => this.request<{ items: Space[] }>("GET", "/api/v1/admin/spaces").then((r) => r.items),
    createSpace: (name: string) => this.request<Space>("POST", "/api/v1/admin/spaces", { body: { name } }),
    spaceMembers: (id: number) =>
      this.request<{ items: { user_id: number; username: string; permission: string }[] }>(
        "GET",
        `/api/v1/admin/spaces/${id}/members`,
      ).then((r) => r.items),
    setMember: (spaceId: number, userId: number, permission: "read" | "write") =>
      this.request<void>("PUT", `/api/v1/admin/spaces/${spaceId}/members/${userId}`, { body: { permission } }),
    removeMember: (spaceId: number, userId: number) =>
      this.request<void>("DELETE", `/api/v1/admin/spaces/${spaceId}/members/${userId}`),
    health: () => this.request<Health>("GET", "/api/v1/admin/health"),
    backupStatus: () => this.request<{ targets: BackupTargetStatus[]; warnings: string[] | null }>("GET", "/api/v1/admin/backup/status"),
    runBackup: (target: string) => this.request<void>("POST", "/api/v1/admin/backup/run", { body: { target } }),
    snapshots: (target: string) =>
      this.request<{ items: { id: string; time: string; host: string; stats: SnapshotStats }[] }>(
        "GET",
        "/api/v1/admin/backup/snapshots",
        { query: { target } },
      ).then((r) => r.items),
  };
}
