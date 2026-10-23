import { describe, expect, it } from "vitest";
import { allowed, freeable, pending, remotePathFor, safeName, type AssetMeta } from "../src/backupPlan";

const may5 = Date.UTC(2024, 4, 5, 12);
const a = (id: string, created: number, modified = created, filename: string | null = `IMG_${id}.HEIC`): AssetMeta => ({
  id, filename, mediaType: "image", creationTime: created, modificationTime: modified,
});

describe("camera roll backup plan", () => {
  it("files photos by capture year/month", () => {
    expect(remotePathFor(a("1", may5), "Camera Roll", "UTC")).toBe("Camera Roll/2024/05/IMG_1.HEIC");
    expect(remotePathFor({ ...a("2", may5, may5, null), mediaType: "video" }, "Phone", "UTC")).toBe("Phone/2024/05/2.mp4");
  });

  it("makes filenames safe for the server", () => {
    expect(safeName("a/b\\c\u0001.jpg")).toBe("a_b_c_.jpg");
    expect(safeName(".pdrive-volume")).toBe("_.pdrive-volume");
    expect(safeName("  ")).toBe("unnamed");
  });

  it("finds new and edited assets only", () => {
    const assets = [a("new", may5), a("same", may5), a("edited", may5, may5 + 5000), a("old", may5 - 1e10)];
    const done = { same: { path: "x", modified: may5 + 1, at: 0 }, edited: { path: "y", modified: may5 + 1, at: 0 } };
    expect(pending(assets, done, may5 - 1e9).map((x) => x.id)).toEqual(["new", "edited"]);
  });

  it("only frees space for verified, fully backed-up, old, unedited photos", () => {
    const now = may5 + 60 * 86_400_000;
    const backup = may5 + 2 * 86_400_000;
    const assets = [a("ok", may5), a("recent", now - 86_400_000), a("unverified", may5), a("edited", may5, now), a("not-in-backup-yet", may5)];
    const done = {
      ok: { path: "", sha256: "h1", modified: may5, at: may5 + 1000 },
      recent: { path: "", sha256: "h2", modified: now, at: may5 + 1000 },
      unverified: { path: "", sha256: "h3", modified: may5, at: may5 + 1000 },
      edited: { path: "", sha256: "h4", modified: may5, at: may5 + 1000 },
      "not-in-backup-yet": { path: "", sha256: "h5", modified: may5, at: backup + 1000 },
    };
    const onServer = new Set(["h1", "h2", "h4", "h5"]);
    expect(freeable(assets, done, onServer, 30, backup, now).map((x) => x.id)).toEqual(["ok"]);
    // No complete backup yet: nothing is ever freeable.
    expect(freeable(assets, done, onServer, 30, null, now)).toEqual([]);
  });

  it("respects Wi-Fi and charging rules", () => {
    const c = { wifiOnly: true, chargingOnly: true };
    expect(allowed(c, { connected: false, wifi: false }, true)).toBe("offline");
    expect(allowed(c, { connected: true, wifi: false }, true)).toBe("waiting for Wi-Fi");
    expect(allowed(c, { connected: true, wifi: true }, false)).toBe("waiting for the charger");
    expect(allowed(c, { connected: true, wifi: true }, true)).toBeNull();
    expect(allowed({ wifiOnly: false, chargingOnly: false }, { connected: true, wifi: false }, false)).toBeNull();
  });
});
