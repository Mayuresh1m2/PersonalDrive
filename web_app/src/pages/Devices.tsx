import { useCallback, useEffect, useState } from "react";
import type { Device } from "@pdrive/client";

import { client } from "../api";
import { ago, errorMessage } from "../format";

export function Devices({ currentDevice }: { currentDevice?: string }) {
  const [devices, setDevices] = useState<Device[]>([]);
  const [qr, setQr] = useState<{ png: string; expires: string } | null>(null);
  const [error, setError] = useState("");

  const load = useCallback(() => client.devices().then(setDevices, (e) => setError(errorMessage(e))), []);
  useEffect(() => void load(), [load]);

  const pair = async () => {
    try {
      const t = await client.createPairingToken();
      setQr({ png: t.qr_png, expires: t.expires_at });
    } catch (e) {
      setError(errorMessage(e));
    }
  };

  const active = devices.filter((d) => !d.revoked_at);
  return (
    <div className="stack">
      <h1>Devices</h1>
      {error && <div className="banner error">{error}</div>}
      <div className="card stack">
        <h2>Add a phone or computer</h2>
        <p className="muted" style={{ margin: 0 }}>
          Open the PersonalDrive app on the new device and scan this code. It signs in as you,
          and the device learns how to trust this server's certificate.
        </p>
        {qr ? (
          <div className="row" style={{ alignItems: "flex-start" }}>
            <img className="qr" src={`data:image/png;base64,${qr.png}`} alt="Pairing QR code" />
            <div className="muted small">Single use. Expires at {new Date(qr.expires).toLocaleTimeString()}.<br /><button style={{ marginTop: 8 }} onClick={pair}>New code</button></div>
          </div>
        ) : (
          <div><button className="primary" onClick={pair}>Show pairing code</button></div>
        )}
      </div>
      <div className="card" style={{ padding: 0 }}>
        <table>
          <thead><tr><th>Device</th><th className="hide-mobile">Platform</th><th>Last seen</th><th /></tr></thead>
          <tbody>
            {active.map((d) => (
              <tr key={d.id}>
                <td>{d.name}{d.id === currentDevice && <span className="pill" style={{ marginLeft: 6 }}>this browser</span>}</td>
                <td className="hide-mobile">{d.platform || "—"}</td>
                <td>{ago(d.last_seen_at)}</td>
                <td style={{ textAlign: "right" }}>
                  {d.id !== currentDevice && (
                    <button className="danger" onClick={async () => {
                      if (!confirm(`Sign out "${d.name}"? It will need to be paired again.`)) return;
                      await client.revokeDevice(d.id!).catch((e) => alert(errorMessage(e)));
                      void load();
                    }}>Sign out</button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
