import { StrictMode, useCallback, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, NavLink, Navigate, Route, Routes } from "react-router-dom";
import type { Device, Space, User } from "@pdrive/client";

import { client, onSignedOut } from "./api";
import { UploadsProvider } from "./components/Uploads";
import { IconFolder, IconHeart, IconHome, IconPhone, IconShare, IconTrash, IconUsers, Logo } from "./components/icons";
import { bytes } from "./format";
import { Overview, Users } from "./pages/Admin";
import { Browser } from "./pages/Browser";
import { Devices } from "./pages/Devices";
import { Login } from "./pages/Login";
import { Trash } from "./pages/Trash";
import "./styles.css";

interface Me {
  user: User;
  device: Device;
  used_bytes: number;
  spaces: Space[];
}

function App() {
  const [me, setMe] = useState<Me | null>(null);
  const [state, setState] = useState<"loading" | "signed-out" | "ready">("loading");

  const load = useCallback(async () => {
    if (!(await client.isSignedIn())) return setState("signed-out");
    try {
      setMe((await client.me()) as Me);
      setState("ready");
    } catch {
      setState(await client.isSignedIn() ? "ready" : "signed-out");
    }
  }, []);

  useEffect(() => {
    void load();
    return onSignedOut(() => setState("signed-out"));
  }, [load]);

  if (state === "loading") return null;
  if (state === "signed-out" || !me) return <Login onSignedIn={load} />;

  const admin = me.user.role === "admin";
  return (
    <UploadsProvider>
      <div className="layout">
        <nav className="sidebar nav" aria-label="Main">
          <div className="brand"><Logo /><span>PersonalDrive</span></div>
          <NavLink to="/" end><IconHome /> <span>My files</span></NavLink>
          {me.spaces.length > 0 && <div className="nav-section">Shared</div>}
          {me.spaces.map((s) => (
            <NavLink key={s.id} to={`/space/${s.id}`}><IconShare /> <span>{s.name}</span></NavLink>
          ))}
          <div className="nav-section">You</div>
          <NavLink to="/trash"><IconTrash /> <span>Trash</span></NavLink>
          <NavLink to="/devices"><IconPhone /> <span>Devices</span></NavLink>
          {admin && <div className="nav-section">Admin</div>}
          {admin && <NavLink to="/admin/server"><IconHeart /> <span>Server &amp; backups</span></NavLink>}
          {admin && <NavLink to="/admin/people"><IconUsers /> <span>People &amp; spaces</span></NavLink>}
          <div className="spacer" />
          <div className="me">
            <div><IconFolder size={12} /> {me.user.display_name || me.user.username} · {bytes(me.used_bytes)}{me.user.quota_bytes ? ` of ${bytes(me.user.quota_bytes)}` : ""}</div>
            <button className="ghost small" style={{ padding: "4px 0" }} onClick={() => client.logout().finally(() => setState("signed-out"))}>Sign out</button>
          </div>
        </nav>
        <main>
          <Routes>
            <Route path="/" element={<Browser spaces={me.spaces} />} />
            <Route path="/space/:spaceId" element={<Browser spaces={me.spaces} />} />
            <Route path="/trash" element={<Trash spaces={me.spaces} />} />
            <Route path="/devices" element={<Devices currentDevice={me.device.id} />} />
            {admin && <Route path="/admin/server" element={<Overview />} />}
            {admin && <Route path="/admin/people" element={<Users me={me.user} />} />}
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </main>
      </div>
    </UploadsProvider>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
);
