import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { MemoryQueueStorage, UploadQueue, blobSource, type QueueItem } from "@pdrive/client";

import { client, wasmSha256 } from "../api";
import { bytes } from "../format";
import { IconX } from "./icons";

interface Entry {
  id: string;
  name: string;
  path: string;
  size: number;
  done: number;
  state: QueueItem["state"];
  note?: string;
  error?: string;
}

interface Ctx {
  /** Queue files for upload into dir (in a space when spaceId is set). */
  enqueue: (files: { file: File; relPath: string }[], dir: string, spaceId?: number) => void;
  onUploaded: (fn: () => void) => () => void;
}

const UploadsContext = createContext<Ctx | null>(null);
export const useUploads = () => useContext(UploadsContext)!;

/**
 * Upload manager for the session. Browsers can't reopen files after a reload,
 * so the queue lives in memory; the server still resumes half-sent uploads
 * within a session after network drops.
 */
export function UploadsProvider({ children }: { children: ReactNode }) {
  const files = useRef(new Map<string, File>());
  const listeners = useRef(new Set<() => void>());
  const [entries, setEntries] = useState<Entry[]>([]);
  const [open, setOpen] = useState(true);

  const update = useCallback((id: string, patch: Partial<Entry>) => {
    setEntries((es) => es.map((e) => (e.id === id ? { ...e, ...patch } : e)));
  }, []);

  const queue = useMemo(
    () =>
      new UploadQueue(client, {
        storage: new MemoryQueueStorage(),
        open: async (it) => {
          const f = files.current.get(it.ref);
          return f ? blobSource(f) : null;
        },
        hasher: wasmSha256,
        concurrency: 2,
        onProgress: (it, done) => update(it.id, { done, state: "uploading" }),
        onItem: (it, res) => {
          update(it.id, {
            state: it.state,
            error: it.lastError,
            note: res?.deduplicated ? "already on server" : res?.conflict ? "saved as conflict copy" : it.state === "pending" ? "will retry" : undefined,
            ...(it.state === "done" ? { done: it.size ?? 0 } : {}),
          });
          if (it.state === "done") {
            files.current.delete(it.ref);
            listeners.current.forEach((fn) => fn());
          }
        },
      }),
    [update],
  );

  const enqueue = useCallback<Ctx["enqueue"]>(
    async (list, dir, spaceId) => {
      setOpen(true);
      for (const { file, relPath } of list) {
        const ref = `${Date.now()}-${Math.random()}`;
        files.current.set(ref, file);
        const path = dir ? `${dir}/${relPath}` : relPath;
        const it = await queue.add({ ref, path, spaceId });
        setEntries((es) => [...es, { id: it.id, name: file.name, path, size: file.size, done: 0, state: "pending" }]);
      }
      void queue.run();
    },
    [queue],
  );

  // Retry pending items (after backoff) while the page is open.
  useEffect(() => {
    const t = setInterval(() => void queue.run(), 5000);
    return () => clearInterval(t);
  }, [queue]);

  const onUploaded = useCallback((fn: () => void) => {
    listeners.current.add(fn);
    return () => void listeners.current.delete(fn);
  }, []);

  const active = entries.filter((e) => e.state === "pending" || e.state === "uploading").length;
  const total = entries.reduce((n, e) => n + e.size, 0);
  const done = entries.reduce((n, e) => n + (e.state === "done" ? e.size : e.done), 0);

  return (
    <UploadsContext.Provider value={{ enqueue, onUploaded }}>
      {children}
      {entries.length > 0 && (
        <div className="uploads card" role="status">
          <div className="row" style={{ marginBottom: 8 }}>
            <strong className="grow" style={{ flex: 1 }}>
              {active > 0 ? `Uploading ${active} of ${entries.length}` : `${entries.length} uploads finished`}
            </strong>
            <span className="muted small">
              {bytes(done)} / {bytes(total)}
            </span>
            <button className="ghost" onClick={() => setOpen(!open)}>{open ? "Hide" : "Show"}</button>
            {active === 0 && (
              <button className="ghost" aria-label="Close" onClick={() => setEntries([])}>
                <IconX size={14} />
              </button>
            )}
          </div>
          <div className="bar"><div style={{ width: `${total ? (100 * done) / total : 0}%` }} /></div>
          {open && (
            <div className="list">
              {entries.slice(-200).map((e) => (
                <div className="item" key={e.id}>
                  <div className="row">
                    <span style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={e.path}>{e.name}</span>
                    <span className={e.state === "failed" ? "error" : e.state === "done" ? "ok" : "muted"}>
                      {e.state === "done" ? e.note ?? "done" : e.state === "failed" ? "failed" : e.state === "uploading" ? `${Math.round((100 * e.done) / (e.size || 1))}%` : e.note ?? "waiting"}
                    </span>
                  </div>
                  {e.error && e.state !== "done" && <div className="error">{e.error}</div>}
                </div>
              ))}
            </div>
          )}
          {entries.some((e) => e.state === "failed") && (
            <button style={{ marginTop: 8 }} onClick={() => queue.retryFailed().then(() => queue.run())}>Retry failed</button>
          )}
        </div>
      )}
    </UploadsContext.Provider>
  );
}
