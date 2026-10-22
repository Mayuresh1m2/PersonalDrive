# ADR 0005: Client stack

Status: accepted (2026-10-23)

## Decision

| Client | Stack | Why |
| --- | --- | --- |
| `shared_lib` | TypeScript SDK; types generated from `api/openapi.yaml` | One implementation of auth, resumable uploads, the upload queue and change following, shared by web and mobile |
| `web_app` | React + Vite + TypeScript, **embedded in the server binary** and served from the same origin | No separate hosting, no cross-origin setup, and the browser trusts the same TLS CA as the apps |
| `mobile_app` | React Native via Expo (TypeScript) | Reuses `shared_lib`; `expo-media-library` for the camera roll and background tasks for auto-backup. One codebase for iOS and Android |
| `desktop_app` | Go sync agent (`pdrive-sync`) for Linux/macOS/Windows; `mac_app` folds into it | Two-way folder sync is a background daemon. Go cross-compiles to small static binaries, needs no Rust/webview toolchain, and is the server's language. A tray UI (e.g. Wails) can wrap it later |

## Alternatives considered

- **Flutter for mobile.** Immich uses it, and it has good background-upload plugins.
  Rejected because it would share no code with the web app; we would maintain the
  upload/sync protocol in Dart as well as TypeScript.
- **Tauri for desktop.** Nice UI, but it needs Rust and system webview toolchains,
  and the core of desktop sync is a headless engine anyway. We can revisit when a UI
  matters more.
- **Electron.** Heavy for an always-running background agent.

## Consequences

- Upload and sync logic exists twice: TypeScript (web/mobile, upload-only or
  browse) and Go (desktop two-way sync). Both follow the same protocol in
  `shared_lib/api/openapi.yaml` and ADR 0003, and both are tested against a real
  server.
- iOS limits background execution, so auto-backup runs when the app is opened
  and opportunistically in the background (BGTaskScheduler via expo-background-task).
  This is the same constraint every photo-backup app lives with.
- Native TLS pinning on mobile needs a small native module (or a CA-trusting
  fetch). Until then the mobile app pins by checking the server's `ca_sha256`
  during pairing, and users install the CA profile on iOS.
