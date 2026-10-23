import type { PairingPayload } from "./client.js";

/** Parses the JSON in a pairing QR code. */
export function parsePairing(text: string): PairingPayload & { token: string; urls: string[] } {
  let p: PairingPayload;
  try {
    p = JSON.parse(text) as PairingPayload;
  } catch {
    throw new Error("this QR code isn't a PersonalDrive pairing code");
  }
  if (p.v !== 1 || !p.token || !Array.isArray(p.urls) || p.urls.length === 0) {
    throw new Error("unsupported or incomplete pairing code");
  }
  return p as PairingPayload & { token: string; urls: string[] };
}

/**
 * Returns the first server URL that answers /api/v1/server (and, when a CA
 * fingerprint is expected, reports the same one), trying all in parallel.
 */
export async function findReachableUrl(
  urls: string[],
  opts: { fetch?: typeof fetch; caSha256?: string; timeoutMs?: number } = {},
): Promise<string | null> {
  const f = opts.fetch ?? globalThis.fetch.bind(globalThis);
  const probe = async (u: string) => {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), opts.timeoutMs ?? 3000);
    try {
      const res = await f(u.replace(/\/+$/, "") + "/api/v1/server", { signal: ctrl.signal });
      if (!res.ok) throw new Error(String(res.status));
      const info = (await res.json()) as { ca_sha256?: string };
      if (opts.caSha256 && info.ca_sha256 && info.ca_sha256 !== opts.caSha256) throw new Error("different server");
      return u;
    } finally {
      clearTimeout(t);
    }
  };
  try {
    return await Promise.any(urls.map(probe));
  } catch {
    return null;
  }
}
