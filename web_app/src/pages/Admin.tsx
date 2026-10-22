import { useCallback, useEffect, useState, type FormEvent } from "react";
import type { BackupTargetStatus, Health, Space, User } from "@pdrive/client";

import { client } from "../api";
import { ago, bytes, date, errorMessage } from "../format";

function useLoad<T>(fn: () => Promise<T>, every = 0) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  const load = useCallback(() => fn().then((d) => (setData(d), setError("")), (e) => setError(errorMessage(e))), [fn]);
  useEffect(() => {
    void load();
    if (!every) return;
    const t = setInterval(load, every);
    return () => clearInterval(t);
  }, [load, every]);
  return { data, error, reload: load };
}

export function Overview() {
  const health = useLoad(useCallback(() => client.admin.health(), []), 15_000);
  const backups = useLoad(useCallback(() => client.admin.backupStatus(), []), 5_000);
  const h = health.data as (Health & { volumes?: { path: string; role: string; ok: boolean; problem?: string; free_bytes: number; total_bytes: number }[] }) | null;
  const warnings = [...(h?.warnings ?? [])];
  return (
    <div className="stack">
      <h1>Server</h1>
      {(health.error || backups.error) && <div className="banner error">{health.error || backups.error}</div>}
      {h && (warnings.length ? (
        <div className="banner warn">{warnings.map((w, i) => <div key={i}>⚠ {w}</div>)}</div>
      ) : (
        <div className="banner" style={{ borderColor: "var(--ok)" }}><span className="ok">●</span> Everything looks healthy.</div>
      ))}
      <div className="card">
        <h2>Disks</h2>
        <table>
          <thead><tr><th>Role</th><th>Path</th><th>Status</th><th>Free</th></tr></thead>
          <tbody>
            {[...(h?.volumes ?? []), ...(h?.state ? [h.state as never] : [])].map((v: { path: string; role: string; ok: boolean; problem?: string; free_bytes: number; total_bytes: number }) => (
              <tr key={v.path + v.role}>
                <td>{v.role === "library" ? "Library (disk A)" : v.role === "state" ? "Database & cache (SSD)" : v.role}</td>
                <td><code>{v.path}</code></td>
                <td>{v.ok ? <span className="ok">OK</span> : <span className="error">{v.problem}</span>}</td>
                <td>{bytes(v.free_bytes)} of {bytes(v.total_bytes)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <Backups targets={backups.data?.targets ?? []} reload={backups.reload} />
    </div>
  );
}

function Backups({ targets, reload }: { targets: BackupTargetStatus[]; reload: () => void }) {
  const [snaps, setSnaps] = useState<{ target: string; items: { id: string; time: string; stats: { files?: number; bytes_scanned?: number; bytes_new?: number } }[] } | null>(null);
  return (
    <div className="card">
      <h2>Backups</h2>
      {targets.length === 0 ? (
        <p className="warn" style={{ margin: 0 }}>No backup targets configured, so the library has no backup. See docs/BACKUP.md.</p>
      ) : (
        <table>
          <thead><tr><th>Target</th><th className="hide-mobile">Where</th><th>Last success</th><th className="hide-mobile">Next</th><th /></tr></thead>
          <tbody>
            {targets.map((t) => (
              <tr key={t.name}>
                <td>
                  <strong>{t.name}</strong>
                  {t.running && <div className="small muted">{t.running}{t.progress ? `: ${t.progress.phase}, ${bytes(t.progress.bytes)} scanned, ${bytes(t.progress.uploaded)} uploaded` : "…"}</div>}
                  {t.last_error && <div className="small error">{t.last_error}</div>}
                </td>
                <td className="hide-mobile"><code>{t.location}</code>{t.storage_class && <span className="pill" style={{ marginLeft: 6 }}>{t.storage_class}</span>}</td>
                <td>
                  <span className={t.last_success ? "ok" : "error"}>{ago(t.last_success)}</span>
                  {t.last_stats && <div className="small muted">{t.last_stats.files} files, {bytes(t.last_stats.bytes_new)} new</div>}
                </td>
                <td className="hide-mobile">{date(t.next_run)}</td>
                <td style={{ textAlign: "right" }}>
                  <div className="row" style={{ justifyContent: "flex-end" }}>
                    <button onClick={async () => setSnaps({ target: t.name!, items: await client.admin.snapshots(t.name!).catch(() => []) })}>Snapshots</button>
                    <button className="primary" disabled={!!t.running} onClick={() => client.admin.runBackup(t.name!).then(reload, (e) => alert(errorMessage(e)))}>Back up now</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {snaps && (
        <div style={{ marginTop: 12 }}>
          <div className="row"><strong style={{ flex: 1 }}>Snapshots on {snaps.target}</strong><button className="ghost" onClick={() => setSnaps(null)}>Close</button></div>
          <table>
            <thead><tr><th>ID</th><th>Time</th><th>Files</th><th>Size</th><th>New</th></tr></thead>
            <tbody>
              {snaps.items.map((s) => (
                <tr key={s.id}><td><code>{s.id.slice(0, 12)}</code></td><td>{date(s.time)}</td><td>{s.stats.files}</td><td>{bytes(s.stats.bytes_scanned)}</td><td>{bytes(s.stats.bytes_new)}</td></tr>
              ))}
            </tbody>
          </table>
          <p className="muted small">Restore with <code>pdrive restore --target {snaps.target} --snapshot &lt;id&gt; …</code> (see docs/RECOVERY.md).</p>
        </div>
      )}
    </div>
  );
}

export function Users({ me }: { me: User }) {
  const users = useLoad(useCallback(() => client.admin.users(), []));
  const [form, setForm] = useState({ username: "", display_name: "", password: "", role: "member" as "member" | "admin", quota_gb: "" });
  const [error, setError] = useState("");

  const create = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await client.admin.createUser({
        username: form.username, display_name: form.display_name, password: form.password, role: form.role,
        quota_bytes: form.quota_gb ? Number(form.quota_gb) * 2 ** 30 : undefined,
      });
      setForm({ username: "", display_name: "", password: "", role: "member", quota_gb: "" });
      setError("");
      void users.reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };
  const update = (id: number, patch: Parameters<typeof client.admin.updateUser>[1]) =>
    client.admin.updateUser(id, patch).then(users.reload, (e) => alert(errorMessage(e)));

  return (
    <div className="stack">
      <h1>People</h1>
      {users.error && <div className="banner error">{users.error}</div>}
      <div className="card" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>User</th><th>Role</th><th>Using</th><th className="hide-mobile">Quota</th><th /></tr></thead>
          <tbody>
            {(users.data ?? []).map((u) => (
              <tr key={u.id} style={{ opacity: u.disabled ? 0.5 : 1 }}>
                <td><strong>{u.username}</strong> {u.display_name && <span className="muted">{u.display_name}</span>}</td>
                <td>{u.role}</td>
                <td>{bytes(u.used_bytes)}</td>
                <td className="hide-mobile">{u.quota_bytes ? bytes(u.quota_bytes) : "unlimited"}</td>
                <td style={{ textAlign: "right" }}>
                  {u.id !== me.id && (
                    <div className="row" style={{ justifyContent: "flex-end" }}>
                      <button onClick={() => {
                        const pw = prompt(`New password for ${u.username} (10+ characters)`);
                        if (pw) void update(u.id!, { password: pw });
                      }}>Reset password</button>
                      <button onClick={() => {
                        const q = prompt("Quota in GB (empty = unlimited)", u.quota_bytes ? String(Math.round(u.quota_bytes / 2 ** 30)) : "");
                        if (q !== null) void update(u.id!, { quota_bytes: q ? Number(q) * 2 ** 30 : 0 });
                      }}>Quota</button>
                      <button className={u.disabled ? "" : "danger"} onClick={() => update(u.id!, { disabled: !u.disabled })}>{u.disabled ? "Enable" : "Disable"}</button>
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <form className="card stack" onSubmit={create}>
        <h2>Add someone</h2>
        <div className="row" style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(160px, 1fr))", gap: 10 }}>
          <label>Username<input required value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} /></label>
          <label>Display name<input value={form.display_name} onChange={(e) => setForm({ ...form, display_name: e.target.value })} /></label>
          <label>Password<input required type="password" minLength={10} value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} autoComplete="new-password" /></label>
          <label>Role<select value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value as "member" | "admin" })}><option value="member">Member</option><option value="admin">Admin</option></select></label>
          <label>Quota (GB)<input type="number" min={0} placeholder="unlimited" value={form.quota_gb} onChange={(e) => setForm({ ...form, quota_gb: e.target.value })} /></label>
        </div>
        {error && <div className="error">{error}</div>}
        <div><button className="primary">Create user</button></div>
      </form>
      <SpacesAdmin users={users.data ?? []} />
    </div>
  );
}

function SpacesAdmin({ users }: { users: User[] }) {
  const spaces = useLoad(useCallback(() => client.admin.spaces(), []));
  const [members, setMembers] = useState<Record<number, { user_id: number; username: string; permission: string }[]>>({});
  const loadMembers = useCallback(async (list: Space[]) => {
    const all: typeof members = {};
    for (const s of list) all[s.id!] = await client.admin.spaceMembers(s.id!);
    setMembers(all);
  }, []);
  useEffect(() => void (spaces.data && loadMembers(spaces.data)), [spaces.data, loadMembers]);

  const add = async () => {
    const name = prompt("Name of the shared space (e.g. Family Photos)");
    if (name) await client.admin.createSpace(name).then(spaces.reload, (e) => alert(errorMessage(e)));
  };
  const setPerm = async (spaceId: number, userId: number, perm: string) => {
    await (perm ? client.admin.setMember(spaceId, userId, perm as "read" | "write") : client.admin.removeMember(spaceId, userId)).catch((e) => alert(errorMessage(e)));
    if (spaces.data) await loadMembers(spaces.data);
  };

  return (
    <div className="card stack">
      <div className="row"><h2 style={{ flex: 1, margin: 0 }}>Shared spaces</h2><button onClick={add}>New space</button></div>
      <p className="muted" style={{ margin: 0 }}>Spaces are folders shared between people. Files are stored once even if several people uploaded the same photo.</p>
      {(spaces.data ?? []).map((s) => (
        <div key={s.id}>
          <strong>{s.name}</strong>
          <table>
            <tbody>
              {users.map((u) => {
                const m = members[s.id!]?.find((x) => x.user_id === u.id);
                return (
                  <tr key={u.id}>
                    <td>{u.username}</td>
                    <td style={{ width: 180 }}>
                      <select value={m?.permission ?? ""} onChange={(e) => setPerm(s.id!, u.id!, e.target.value)}>
                        <option value="">No access</option>
                        <option value="read">Can view</option>
                        <option value="write">Can edit</option>
                      </select>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ))}
    </div>
  );
}
