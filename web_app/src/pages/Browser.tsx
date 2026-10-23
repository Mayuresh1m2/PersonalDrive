import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { ChangeFollower, type FileEntry, type Space } from "@pdrive/client";

import { client, mediaUrl } from "../api";
import { Preview } from "../components/Preview";
import { Thumb } from "../components/Thumb";
import { useUploads } from "../components/Uploads";
import { IconPlus, IconUpload } from "../components/icons";
import { bytes, date, errorMessage, kind } from "../format";

interface DroppedFile {
  file: File;
  relPath: string;
}

/** Walks dropped folders (webkitGetAsEntry) into a flat file list. */
async function collectDropped(items: DataTransferItemList): Promise<DroppedFile[]> {
  const out: DroppedFile[] = [];
  const walk = async (entry: FileSystemEntry, prefix: string): Promise<void> => {
    if (entry.isFile) {
      const file = await new Promise<File>((res, rej) => (entry as FileSystemFileEntry).file(res, rej));
      out.push({ file, relPath: prefix + file.name });
    } else if (entry.isDirectory) {
      const reader = (entry as FileSystemDirectoryEntry).createReader();
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((res, rej) => reader.readEntries(res, rej));
        if (batch.length === 0) break;
        for (const e of batch) await walk(e, `${prefix}${entry.name}/`);
      }
    }
  };
  const entries = Array.from(items)
    .map((i) => i.webkitGetAsEntry?.())
    .filter((e): e is FileSystemEntry => !!e);
  for (const e of entries) await walk(e, "");
  return out;
}

export function Browser({ spaces }: { spaces: Space[] }) {
  const params = useParams();
  const spaceId = params.spaceId ? Number(params.spaceId) : undefined;
  const space = spaces.find((s) => s.id === spaceId);
  const canWrite = !spaceId || space?.permission === "write";
  const [search, setSearch] = useSearchParams();
  const dir = search.get("path") ?? "";
  const navigate = useNavigate();
  const { enqueue, onUploaded } = useUploads();

  const [items, setItems] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [preview, setPreview] = useState<number | null>(null);
  const [selected, setSelected] = useState<number | null>(null);
  const [dragging, setDragging] = useState(false);
  const [view, setView] = useState<"grid" | "list">(() => (localStorage.getItem("pdrive.view") as "grid" | "list") ?? "grid");
  const touch = useMemo(() => window.matchMedia?.("(hover: none)").matches ?? false, []);
  const fileInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement>(null);

  const load = useCallback(async () => {
    try {
      setItems(await client.listAll(dir, spaceId));
      setError("");
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setLoading(false);
    }
  }, [dir, spaceId]);

  useEffect(() => {
    setLoading(true);
    setSelected(null);
    void load();
  }, [load]);

  // Live updates: reload when something changes in this folder.
  useEffect(() => {
    let follower: ChangeFollower | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    void client.cursor().then((cursor) => {
      follower = new ChangeFollower(client, {
        cursor,
        onChanges: (changes) => {
          const touches = changes.some((c) => {
            if ((c.space_id ?? undefined) !== spaceId) return false;
            const parents = [c.path, c.old_path].map((p) => (p ?? "").split("/").slice(0, -1).join("/"));
            return parents.includes(dir) || (c.path ?? "").startsWith(dir);
          });
          if (touches) {
            clearTimeout(timer);
            timer = setTimeout(() => void load(), 300);
          }
        },
      }).start();
    });
    const off = onUploaded(() => void load());
    return () => {
      void follower?.stop();
      clearTimeout(timer);
      off();
    };
  }, [dir, spaceId, load, onUploaded]);

  // A space we're no longer a member of: back to My files.
  useEffect(() => {
    if (spaceId && spaces.length > 0 && !space) navigate("/");
  }, [spaceId, spaces, space, navigate]);

  const folders = useMemo(() => items.filter((i) => i.is_dir), [items]);
  const files = useMemo(() => items.filter((i) => !i.is_dir), [items]);
  const media = useMemo(() => files.filter((f) => ["image", "video"].includes(kind(f.mime))), [files]);
  const crumbs = dir ? dir.split("/") : [];
  const go = (path: string) => setSearch(path ? { path } : {});
  const sel = items.find((i) => i.id === selected);

  const upload = (list: DroppedFile[]) => list.length && enqueue(list, dir, spaceId);

  const onDrop = async (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    if (!canWrite) return;
    upload(await collectDropped(e.dataTransfer.items));
  };

  const open = (f: FileEntry) => {
    if (f.is_dir) return go(f.path!);
    const i = media.findIndex((m) => m.id === f.id);
    if (i >= 0) return setPreview(i);
    void mediaUrl(client.contentPath(f.id!)).then((u) => window.open(u, "_blank", "noopener"));
  };

  const act = async (fn: () => Promise<unknown>) => {
    try {
      await fn();
      await load();
    } catch (e) {
      alert(errorMessage(e));
    }
  };

  const newFolder = () => {
    const name = prompt("Folder name");
    if (name) void act(() => client.mkdir(dir ? `${dir}/${name}` : name, spaceId));
  };
  const rename = (f: FileEntry) => {
    const name = prompt("New name", f.name);
    if (name && name !== f.name) {
      const parent = f.path!.split("/").slice(0, -1).join("/");
      void act(() => client.move(f.id!, parent ? `${parent}/${name}` : name));
    }
  };
  const remove = (f: FileEntry) => {
    if (confirm(`Move "${f.name}" to the trash?`)) void act(() => client.trash(f.id!));
  };
  const downloadSel = async (f: FileEntry) => {
    const a = document.createElement("a");
    a.href = await mediaUrl(client.contentPath(f.id!), { download: 1 });
    a.click();
  };

  return (
    <div
      onDragOver={(e) => {
        e.preventDefault();
        if (canWrite) setDragging(true);
      }}
      onDragLeave={(e) => e.currentTarget === e.target && setDragging(false)}
      onDrop={onDrop}
      style={{ minHeight: "100%" }}
    >
      <div className="toolbar">
        <div className="crumbs grow">
          <a href="#" onClick={(e) => (e.preventDefault(), go(""))}>{space ? space.name : "My files"}</a>
          {crumbs.map((c, i) => (
            <span key={i}>
              <span className="muted"> / </span>
              <a href="#" onClick={(e) => (e.preventDefault(), go(crumbs.slice(0, i + 1).join("/")))}>{c}</a>
            </span>
          ))}
        </div>
        {sel && !sel.is_dir && <button onClick={() => downloadSel(sel)}>Download</button>}
        {sel && canWrite && <button onClick={() => rename(sel)}>Rename</button>}
        {sel && canWrite && <button className="danger" onClick={() => remove(sel)}>Delete</button>}
        <button className="ghost hide-mobile" onClick={() => {
          const v = view === "grid" ? "list" : "grid";
          setView(v);
          localStorage.setItem("pdrive.view", v);
        }}>{view === "grid" ? "List" : "Grid"}</button>
        {canWrite && <button onClick={newFolder}><IconPlus size={14} /> Folder</button>}
        {canWrite && <button className="hide-mobile" onClick={() => folderInput.current?.click()}>Upload folder</button>}
        {canWrite && <button className="primary" onClick={() => fileInput.current?.click()}><IconUpload size={14} /> Upload</button>}
        <input ref={fileInput} type="file" multiple hidden onChange={(e) => {
          upload(Array.from(e.target.files ?? []).map((file) => ({ file, relPath: file.name })));
          e.target.value = "";
        }} />
        <input ref={folderInput} type="file" multiple hidden {...{ webkitdirectory: "" }} onChange={(e) => {
          upload(Array.from(e.target.files ?? []).map((file) => ({ file, relPath: file.webkitRelativePath || file.name })));
          e.target.value = "";
        }} />
      </div>

      {!canWrite && space && <div className="banner warn">You have read-only access to {space.name}.</div>}
      {error && <div className="banner error">{error}</div>}
      {!loading && items.length === 0 && !error && (
        <div className="card muted" style={{ textAlign: "center", padding: 40 }}>
          {canWrite ? "Empty folder. Drop files or folders here, or use Upload." : "Empty folder."}
        </div>
      )}

      {view === "grid" ? (
        <div className="grid">
          {[...folders, ...files].map((f) => (
            <div
              key={f.id}
              className={`tile${selected === f.id ? " selected" : ""}`}
              onClick={() => (touch ? open(f) : setSelected(f.id!))}
              onDoubleClick={() => open(f)}
              onContextMenu={(e) => {
                // Long-press on phones selects (for rename/delete).
                if (touch) {
                  e.preventDefault();
                  setSelected(f.id!);
                }
              }}
              onKeyDown={(e) => e.key === "Enter" && open(f)}
              tabIndex={0}
              title={f.name}
            >
              <Thumb file={f} />
              {kind(f.mime) === "video" && <span className="badge">video</span>}
              <div className="meta">
                <div className="name">{f.name}</div>
                <div className="sub">{f.is_dir ? "Folder" : `${bytes(f.size)} · ${date(f.media?.taken_at ?? f.mtime)}`}</div>
              </div>
            </div>
          ))}
        </div>
      ) : (
        items.length > 0 && (
          <div className="card" style={{ padding: 0 }}>
            <table>
              <thead><tr><th>Name</th><th>Size</th><th className="hide-mobile">Modified</th></tr></thead>
              <tbody>
                {[...folders, ...files].map((f) => (
                  <tr key={f.id} onClick={() => setSelected(f.id!)} onDoubleClick={() => open(f)} style={{ cursor: "pointer", background: selected === f.id ? "var(--panel-2)" : undefined }}>
                    <td>{f.is_dir ? "📁 " : ""}{f.name}</td>
                    <td>{f.is_dir ? "—" : bytes(f.size)}</td>
                    <td className="hide-mobile">{date(f.mtime)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )
      )}
      <p className="muted small" style={{ marginTop: 16 }}>
        {touch ? "Tap to open, long-press to select." : "Double-click to open."} {canWrite && "Drag files or whole folders anywhere to upload. "}
        Deleted files go to the <Link to="/trash">Trash</Link>, where you can restore them.
      </p>

      {dragging && <div className="dropzone">Drop to upload to {dir || (space?.name ?? "My files")}</div>}
      {preview !== null && media[preview] && (
        <Preview files={media} index={preview} onIndex={setPreview} onClose={() => setPreview(null)} />
      )}
    </div>
  );
}
