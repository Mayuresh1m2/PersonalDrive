import { useEffect, useState } from "react";
import type { FileEntry } from "@pdrive/client";

import { mediaUrl } from "../api";
import { bytes, date, kind } from "../format";
import { IconDownload, IconX } from "./icons";

/** Full-screen viewer for images and videos, with arrow-key navigation. */
export function Preview({ files, index, onClose, onIndex }: { files: FileEntry[]; index: number; onClose: () => void; onIndex: (i: number) => void }) {
  const file = files[index]!;
  const [src, setSrc] = useState<string>();
  const [download, setDownload] = useState<string>();

  useEffect(() => {
    let alive = true;
    const k = kind(file.mime);
    void (async () => {
      const full = await mediaUrl(`/api/v1/files/${file.id}/content`);
      const dl = await mediaUrl(`/api/v1/files/${file.id}/content`, { download: 1 });
      // Big images: show the 1024px preview first (originals can be 20+ MB).
      const shown = k === "image" && (file.size ?? 0) > 4 << 20 ? await mediaUrl(`/api/v1/files/${file.id}/thumbnail`, { size: 1024 }) : full;
      if (alive) {
        setSrc(shown);
        setDownload(dl);
      }
    })();
    return () => {
      alive = false;
    };
  }, [file]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
      if (e.key === "ArrowRight" && index < files.length - 1) onIndex(index + 1);
      if (e.key === "ArrowLeft" && index > 0) onIndex(index - 1);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [index, files.length, onClose, onIndex]);

  const k = kind(file.mime);
  return (
    <div className="overlay" role="dialog" aria-label={file.name}>
      <header>
        <div className="grow">
          <strong>{file.name}</strong> <span style={{ opacity: 0.7 }}>· {bytes(file.size)} · {date(file.media?.taken_at ?? file.mtime)}</span>
        </div>
        <button disabled={index === 0} onClick={() => onIndex(index - 1)}>‹</button>
        <button disabled={index >= files.length - 1} onClick={() => onIndex(index + 1)}>›</button>
        {download && (
          <a className="btn" href={download} style={{ color: "white", background: "rgb(255 255 255 / .1)", border: 0 }}>
            <IconDownload size={16} />
          </a>
        )}
        <button aria-label="Close" onClick={onClose}><IconX size={16} /></button>
      </header>
      <div className="body" onClick={(e) => e.target === e.currentTarget && onClose()}>
        {src && k === "image" && <img src={src} alt={file.name} />}
        {src && k === "video" && <video src={src} controls autoPlay />}
        {src && k === "audio" && <audio src={src} controls autoPlay />}
        {src && (k === "pdf" || k === "text") && <iframe src={src} title={file.name} style={{ width: "min(1000px, 100%)", height: "100%", border: 0, background: "white", borderRadius: 8 }} sandbox="" />}
        {k === "other" && (
          <div style={{ textAlign: "center" }}>
            <p>No preview for this file type.</p>
            {download && <a className="btn" href={download}>Download</a>}
          </div>
        )}
      </div>
    </div>
  );
}
