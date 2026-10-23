// Pure camera-roll backup logic (no React Native imports), unit-tested.

export interface AssetMeta {
  id: string;
  filename: string | null;
  mediaType: string;
  creationTime: number | null; // ms
  modificationTime: number | null;
}

/** What we remember about each backed-up asset (keyed by asset id). */
export interface BackedUp {
  path: string;
  sha256?: string;
  modified: number | null;
  at: number;
}

/**
 * Server path for a photo: Camera Roll/2024/05/IMG_1234.HEIC. Dates come from
 * the capture time so the library reads like a timeline.
 */
export function remotePathFor(a: AssetMeta, root = "Camera Roll", tz?: string): string {
  const d = new Date(a.creationTime ?? a.modificationTime ?? Date.now());
  const parts = new Intl.DateTimeFormat("en-CA", { timeZone: tz, year: "numeric", month: "2-digit" }).formatToParts(d);
  const y = parts.find((p) => p.type === "year")!.value;
  const m = parts.find((p) => p.type === "month")!.value;
  return `${root}/${y}/${m}/${safeName(a.filename || `${a.id}${a.mediaType === "video" ? ".mp4" : ".jpg"}`)}`;
}

/** Server paths can't contain slashes or control characters. */
export function safeName(name: string): string {
  const cleaned = name.replace(/[\\/\u0000-\u001f]/g, "_").trim();
  return cleaned.startsWith(".pdrive") ? `_${cleaned}` : cleaned || "unnamed";
}

/** Assets that still need uploading: new, or edited since their backup. */
export function pending(assets: AssetMeta[], done: Record<string, BackedUp>, since = 0): AssetMeta[] {
  return assets.filter((a) => {
    if ((a.creationTime ?? 0) < since) return false;
    const b = done[a.id];
    return !b || (a.modificationTime ?? 0) > (b.modified ?? 0);
  });
}

/**
 * Assets safe to delete from the phone. All of these must hold:
 * - uploaded, and the server confirms it has that exact content (hash);
 * - uploaded before the server's last *complete* backup, so copies exist on
 *   the server, the local backup disk and offsite;
 * - not edited since the upload;
 * - older than keepDays.
 */
export function freeable(
  assets: AssetMeta[],
  done: Record<string, BackedUp>,
  onServer: Set<string>,
  keepDays: number,
  lastFullBackup: number | null,
  now = Date.now(),
): AssetMeta[] {
  if (!lastFullBackup) return [];
  const cutoff = now - keepDays * 86_400_000;
  return assets.filter((a) => {
    const b = done[a.id];
    return (
      !!b?.sha256 &&
      onServer.has(b.sha256) &&
      b.at < lastFullBackup &&
      (a.creationTime ?? now) < cutoff &&
      (a.modificationTime ?? 0) <= (b.modified ?? 0)
    );
  });
}

export interface Conditions {
  wifiOnly: boolean;
  chargingOnly: boolean;
}

export function allowed(c: Conditions, net: { wifi: boolean; connected: boolean }, charging: boolean): string | null {
  if (!net.connected) return "offline";
  if (c.wifiOnly && !net.wifi) return "waiting for Wi-Fi";
  if (c.chargingOnly && !charging) return "waiting for the charger";
  return null;
}
