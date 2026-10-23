import { describe, expect, it } from "vitest";
import { bytes, kind } from "../src/format";

describe("format", () => {
  it("formats sizes", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(1536)).toBe("1.5 KB");
    expect(bytes(3 * 2 ** 30)).toBe("3.0 GB");
    expect(bytes(500 * 2 ** 20)).toBe("500 MB");
  });
  it("classifies mime types", () => {
    expect(kind("image/heic")).toBe("image");
    expect(kind("video/quicktime")).toBe("video");
    expect(kind("application/pdf")).toBe("pdf");
    expect(kind("application/zip")).toBe("other");
  });
});
