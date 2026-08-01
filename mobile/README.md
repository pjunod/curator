# Mobile — Monarr on iOS and Android

Companion to the root [README](../README.md) (server setup) and
[usage guide](../docs/usage.md) (what every workflow does) — this is how to
run, test, and ship the native app.

The app is one Expo/React Native codebase that produces native Android and
iOS binaries. It connects to an existing Monarr server over `/api/v1`; no
Monarr data or metadata provider keys live on the phone.

## What the app owns

- Library browsing, filtering, item detail, monitoring, and search-now.
- Discover and metadata search, including root/profile choices when adding.
- Wanted items, backlog search, active and retained activity, and calendar.
- Server status and health, with a handoff to the web UI for administration.
- One saved server address and API key. The API key is stored with Android
  Keystore or iOS Keychain through Expo SecureStore.

The native app does not configure indexers, download clients, notifiers,
root folders, profiles, or server security. Those changes are infrequent,
carry more destructive edges, and remain in the full web interface.

## Run it on a device

Expo SDK 57 requires Node 22.13 or newer. Monarr itself must already be
reachable from the phone by LAN, VPN, or HTTPS reverse proxy.

```bash
cd mobile                 # native app workspace
npm ci                    # exact dependencies from package-lock.json
npm start                 # show the Expo QR code and simulator controls
```

Open the QR code with Expo Go, or press `i` / `a` for an available iOS
Simulator / Android emulator. Enter the same address the phone can reach —
`localhost` points at the phone, not the Monarr host.

On first connect, copy the key from **Monarr web → Settings → Security →
Reveal**. The key is optional only while native API authentication is off.
An `http://` LAN address is supported for homelabs; use HTTPS or a trusted VPN
for access outside the local network because cleartext HTTP exposes the API
key in transit.

## Verify a change

```bash
make test-mobile           # strict TypeScript + Vitest
make mobile-export         # Expo Doctor + production JS bundles for both OSes
```

`make mobile-export` is the local no-credentials build gate. A green run
means Metro compiled the application for both native runtimes; it does not
produce signed store archives. The root repository gate still applies to
changes that touch shared server or web behavior.

## Build installable and store binaries

The checked-in [`eas.json`](eas.json) has two profiles:

| Profile | Android | iOS | Use |
|---|---|---|---|
| `preview` | installable APK | internal ad-hoc build | testers and real-device QA |
| `production` | signed AAB | signed IPA | Play Console and App Store Connect |

```bash
cd mobile
npx eas-cli@latest login
npx eas-cli@latest build --platform all --profile preview
npx eas-cli@latest build --platform all --profile production
```

The first production build asks for the Expo project and store signing
credentials. Those belong to the release owner and are never committed.
After real-device QA, submit with `eas submit --platform android` and
`eas submit --platform ios`; iOS enters TestFlight before App Review.

## Layout

- [`App.tsx`](App.tsx) — connection lifecycle, stack state, and bottom tabs.
- [`src/api.ts`](src/api.ts) — typed `/api/v1` client and error handling.
- [`src/storage.ts`](src/storage.ts) — keychain/keystore connection storage.
- `src/screens/` — one native screen per daily workflow.
- `src/components/` — themed controls and media cards shared by screens.
- [`app.json`](app.json) — native identifiers, LAN transport, icons, splash,
  and secure-storage plugins.
- [`eas.json`](eas.json) — preview and production signing/build profiles.
