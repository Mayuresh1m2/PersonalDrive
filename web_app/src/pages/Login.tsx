import { useEffect, useState, type FormEvent } from "react";
import type { ServerInfo } from "@pdrive/client";

import { client } from "../api";
import { Logo } from "../components/icons";
import { errorMessage } from "../format";

export function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [info, setInfo] = useState<ServerInfo | null>(null);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    client.serverInfo().then(setInfo, (e) => setError(errorMessage(e)));
  }, []);

  const setup = info?.setup_required;
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (setup) await client.setup(code, username, password);
      else await client.login(username, password);
      onSignedIn();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="center">
      <form className="auth card" onSubmit={submit}>
        <div className="brand" style={{ padding: 0 }}>
          <Logo />
          <h1>{info?.name ?? "PersonalDrive"}</h1>
        </div>
        {setup && (
          <p className="muted" style={{ margin: 0 }}>
            Welcome! Create the admin account. The setup code is printed in the server log
            (<code>journalctl --user -u pdrive</code>).
          </p>
        )}
        {setup && (
          <label>
            Setup code
            <input value={code} onChange={(e) => setCode(e.target.value)} autoComplete="off" required />
          </label>
        )}
        <label>
          Username
          <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" autoFocus required />
        </label>
        <label>
          Password
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete={setup ? "new-password" : "current-password"} minLength={setup ? 10 : undefined} required />
        </label>
        {error && <div className="error">{error}</div>}
        <button className="primary" disabled={busy}>{setup ? "Create admin account" : "Sign in"}</button>
        {info?.read_only && <div className="warn small">The server is read-only right now (library disk unavailable or a restore is running).</div>}
      </form>
    </div>
  );
}
