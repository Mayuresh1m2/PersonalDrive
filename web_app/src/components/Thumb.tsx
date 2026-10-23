import { useEffect, useRef, useState } from "react";
import type { FileEntry } from "@pdrive/client";

import { mediaUrl } from "../api";
import { kind } from "../format";
import { IconFile, IconFolder, IconImage, IconVideo } from "./icons";

/** Lazily loads a thumbnail when the tile scrolls into view; falls back to an icon. */
export function Thumb({ file }: { file: FileEntry }) {
  const ref = useRef<HTMLDivElement>(null);
  const [src, setSrc] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const k = kind(file.mime);
  const wantsThumb = !file.is_dir && (k === "image" || k === "video");

  useEffect(() => {
    if (!wantsThumb || !ref.current) return;
    let alive = true;
    const io = new IntersectionObserver(
      async ([e]) => {
        if (!e?.isIntersecting) return;
        io.disconnect();
        const u = await mediaUrl(`/api/v1/files/${file.id}/thumbnail`, { size: 256, v: file.sha256?.slice(0, 8) ?? "" });
        if (alive) setSrc(u);
      },
      { rootMargin: "300px" },
    );
    io.observe(ref.current);
    return () => {
      alive = false;
      io.disconnect();
    };
  }, [file.id, file.sha256, wantsThumb]);

  const Icon = file.is_dir ? IconFolder : k === "image" ? IconImage : k === "video" ? IconVideo : IconFile;
  return (
    <div className="thumb" ref={ref}>
      {src && !failed ? <img src={src} alt="" loading="lazy" onError={() => setFailed(true)} /> : <Icon />}
    </div>
  );
}
