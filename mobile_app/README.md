# mobile_app

A phone app (iOS and Android, React Native via Expo SDK 58) built on
`@pdrive/client`.

- **Pairing.** Scan the QR code from the web app (Devices → Show pairing code) or
  from `pdrive pair <user>`. The app tries every server address in the code and
  checks the server's CA fingerprint. You can also sign in with a password.
- **Camera-roll backup.**
  - Photos and videos go to `Camera Roll/<year>/<month>/` in your library, or into
    a shared space such as "Family Photos".
  - Choose "new photos only" or "everything", and Wi-Fi-only or charging-only.
  - Uploads are resumable. Files the server already has (even from another family
    member) finish instantly.
  - Backup runs when the app opens and in the background via `expo-background-task`.
    The OS decides when; each run gets about 30 s and the queue resumes next time.
  - Backup is upload-only: deleting a photo on the phone never deletes it on the
    server.
- **Free up space.** Removes photos older than 30 days from the phone, but only
  ones whose exact content (SHA-256) the server confirms it has. The OS asks you
  to confirm.
- **Library.** Browse your folders with thumbnails.

## Trusting your server

The server uses its own certificate authority (no public domain is needed), so
the phone has to trust it once:

- **Android.**
  1. Open `https://<server>:8443/api/v1/ca.pem` in Chrome and accept the warning
     once.
  2. Go to Settings → Security → Encryption & credentials → Install a certificate →
     CA certificate.
  
  The app is configured (`plugins/withUserCAs.js`) to trust user-installed CAs.
  No other app is affected.
- **iOS.**
  1. Open the same URL in Safari and allow the profile download.
  2. Install it in Settings → General → VPN & Device Management.
  3. Enable full trust in Settings → General → About → Certificate Trust Settings.

## Develop

Phones need a development build, because background tasks and media access don't
work in Expo Go:

```bash
pnpm install
pnpm --filter @pdrive/client build
cd mobile_app
npx expo run:android     # needs Android SDK; or: npx expo run:ios (macOS + Xcode)
pnpm test                # backup planning logic
pnpm bundle              # checks the JS bundles for Android
```

## Known limits

- iOS gives background work little time, so a big first backup goes fastest with
  the app open (on the charger and Wi-Fi).
- If you edit a photo after it was backed up, the edited version uploads as a
  "(conflict …)" copy next to the original, so both are kept.
- **This app has only been type-checked, unit-tested and bundled. It hasn't run
  on a device yet** (no Android SDK or Xcode where it was built). Expect the first
  device run to need small fixes.
