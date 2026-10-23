// Small inline icon set (stroke icons, 24px grid).
const P = (d: string) => (props: { size?: number }) => (
  <svg width={props.size ?? 18} height={props.size ?? 18} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d={d} />
  </svg>
);

export const IconFolder = P("M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z");
export const IconFile = P("M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8zM14 3v5h5");
export const IconImage = P("M4 5h16v14H4zM4 16l5-5 4 4 3-3 4 4M15 9.5a1 1 0 1 0 0-.01");
export const IconVideo = P("M4 6h11v12H4zM15 10l5-3v10l-5-3");
export const IconTrash = P("M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3");
export const IconUpload = P("M12 16V4M7 9l5-5 5 5M4 20h16");
export const IconHome = P("M3 11l9-7 9 7v9a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z");
export const IconUsers = P("M16 19v-1a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v1M9 10a3 3 0 1 0 0-6 3 3 0 0 0 0 6M22 19v-1a4 4 0 0 0-3-3.9M16 4.1a3 3 0 0 1 0 5.8");
export const IconPhone = P("M8 3h8a1 1 0 0 1 1 1v16a1 1 0 0 1-1 1H8a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM11 18h2");
export const IconShield = P("M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z");
export const IconHeart = P("M3 12h4l3-8 4 16 3-8h4");
export const IconPlus = P("M12 5v14M5 12h14");
export const IconDownload = P("M12 4v12M7 11l5 5 5-5M4 20h16");
export const IconX = P("M6 6l12 12M18 6L6 18");
export const IconShare = P("M17 8a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM7 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM17 22a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM9.6 13.5l4.8 3M14.4 6.5l-4.8 3");

export function Logo() {
  return (
    <svg viewBox="0 0 32 32" aria-hidden="true">
      <rect width="32" height="32" rx="7" fill="var(--accent)" />
      <path d="M9 21v-8l7-4 7 4v8l-7 4z" fill="white" />
    </svg>
  );
}
