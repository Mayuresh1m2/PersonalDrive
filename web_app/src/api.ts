import { PDriveClient, jsSha256, type HasherFactory, type Tokens, type TokenStore } from "@pdrive/client";
import { createSHA256 } from "hash-wasm";

const KEY = "pdrive.tokens";

/** Tokens live in localStorage; the strict CSP set by the server limits XSS exposure. */
const store: TokenStore = {
  load() {
    try {
      const v = localStorage.getItem(KEY);
      return v ? (JSON.parse(v) as Tokens) : null;
    } catch {
      return null;
    }
  },
  save(t) {
    try {
      if (t) localStorage.setItem(KEY, JSON.stringify(t));
      else localStorage.removeItem(KEY);
    } catch {
      /* private mode */
    }
  },
};

const signedOut = new Set<() => void>();
export const onSignedOut = (fn: () => void) => {
  signedOut.add(fn);
  return () => void signedOut.delete(fn);
};

function browserName() {
  const ua = navigator.userAgent;
  const b = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Browser";
  const os = /Android/.test(ua) ? "Android" : /iPhone|iPad/.test(ua) ? "iOS" : /Mac/.test(ua) ? "macOS" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
  return `${b}${os ? " on " + os : ""}`;
}

/** Same-origin: the server serves this app. */
export const client = new PDriveClient({
  baseUrl: "",
  tokens: store,
  deviceName: browserName(),
  platform: "web",
  onSignedOut: () => signedOut.forEach((fn) => fn()),
});

/** WASM SHA-256: several times faster than pure JS for big videos. Falls
 * back to pure JS where WebAssembly is unavailable or blocked. */
export const wasmSha256: HasherFactory = async () => {
  try {
    const h = await createSHA256();
    h.init();
    return { update: (d) => void h.update(d), digestHex: () => h.digest("hex") };
  } catch {
    return jsSha256();
  }
};

/**
 * URL usable in <img>/<video>/<a download>. Browsers can't add headers to
 * those requests, so the (15-minute) access token goes in the query string;
 * the server only accepts that on content and thumbnail endpoints.
 */
export async function mediaUrl(path: string, query: Record<string, string | number> = {}) {
  const token = await client.accessToken();
  return client.url(path, { ...query, access_token: token ?? "" });
}
