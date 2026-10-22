import { useCallback, useEffect, useState } from "react";
import type { FileEntry, Space } from "@pdrive/client";

import { client } from "../api";
import { bytes, date, errorMessage } from "../format";

export function Trash({ spaces }: { spaces: Space[] }) {
  const [spaceId, setSpaceId] = useState<number | undefined>();
  const [items, setItems] = useState<FileEntry[]>([]);
  const [days, setDays] = useState(30);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      const r = await client.listTrash(spaceId);
      setItems(r.items);
      setDays(r.retention_days);
      setError("");
    } catch (e) {
      setError(errorMessage(e));
    }
  }, [spaceId]);
  useEffect(() => void load(), [load]);

  const act = async (fn: () => Promise<unknown>) => {
    try {
      await fn();
    } catch (e) {
      alert(errorMessage(e));
    }
    await load();
  };

  return (
    <div className="stack">
      <div className="toolbar">
        <h1 className="grow">Trash</h1>
        <select value={spaceId ?? ""} onChange={(e) => setSpaceId(e.target.value ? Number(e.target.value) : undefined)} style={{ width: "auto" }}>
          <option value="">My files</option>
          {spaces.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
        </select>
      </div>
      <p className="muted" style={{ margin: 0 }}>
        Deleted items and previous versions of edited files are kept for {days} days, then removed for good.
        They also stay in older backups.
      </p>
      {error && <div className="banner error">{error}</div>}
      <div className="card" style={{ padding: 0 }}>
        {items.length === 0 ? (
          <p className="muted" style={{ padding: 16, margin: 0 }}>Trash is empty.</p>
        ) : (
          <table>
            <thead><tr><th>Name</th><th>Why</th><th className="hide-mobile">Size</th><th className="hide-mobile">Deleted</th><th /></tr></thead>
            <tbody>
              {items.map((f) => (
                <tr key={f.id}>
                  <td title={f.path}>{f.is_dir ? "📁 " : ""}{f.path}</td>
                  <td><span className="pill">{f.trash_reason === "replaced" ? "old version" : "deleted"}</span></td>
                  <td className="hide-mobile">{f.is_dir ? "—" : bytes(f.size)}</td>
                  <td className="hide-mobile">{date(f.trashed_at)}</td>
                  <td style={{ textAlign: "right" }}>
                    <div className="row" style={{ justifyContent: "flex-end" }}>
                      <button onClick={() => act(() => client.restoreFromTrash(f.id!))}>Restore</button>
                      <button className="danger" onClick={() => confirm(`Delete "${f.name}" forever?`) && act(() => client.purge(f.id!))}>Delete forever</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
