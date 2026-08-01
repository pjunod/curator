# Mobile — Monarr on iOS and Android

Companion to the root [README](../README.md) (server setup) and
[usage guide](../docs/usage.md) (what every workflow does) — this is how to
run, test, and ship the native app.

The app is one Expo/React Native codebase that produces native Android and
iOS binaries. It connects to an existing Monarr server over `/api/v1`; no
Monarr data or metadata provider keys live on the phone.

## What the app owns

- Library browsing, filtering, item detail, item/profile/location editing,
  season monitoring, additional quality copies, and search-now.
- Discover and metadata search, including root/profile choices when adding.
- Wanted items, backlog search, active and retained activity, and calendar.
- Server status and health, with a handoff to the web UI for administration.
- Automatic first-run Wi-Fi discovery over DNS-SD and camera-based pairing by
  QR.
- Confirmed bulk clearing for retained failures under Activity → Failed.
- Per-device Auto/Light/Dark theme and Small/Medium/Large item-size choices.
- One saved server address and API key. The API key is stored with Android
  Keystore or iOS Keychain through Expo SecureStore.

The native app assigns existing root folders and quality profiles to items and
copies, but does not define those resources or configure indexers, download
clients, notifiers, or server security. Those system-level changes remain in
the full web interface.

## Run it on a device

Expo SDK 57 requires Node 22.13 or newer. Monarr itself must already be
reachable from the phone by LAN, VPN, or HTTPS reverse proxy.

```bash
cd mobile                 # native app workspace
npm ci                    # exact dependencies from package-lock.json
npm start                 # show the Expo QR code and simulator controls
```

Open the development QR code with Expo Go, or press `i` / `a` for an available
iOS Simulator / Android emulator. In a native build, the connection screen
immediately browses `_monarr._tcp` with DNS-SD and displays verified servers as
they answer. The server supplies its real service port and resolved IPv4/IPv6
addresses; the app never assumes a subnet size or probes an address range.
DNS-SD may be blocked by guest Wi-Fi, VLAN isolation, or a VPN. A Docker
install must run the host-network `monarr-discovery` companion included in the
server's v0.18.5 Compose template; multicast cannot leave the main container's
bridge. QR and manual entry remain visible while the scan runs and remain
available when the server is otherwise reachable.

For QR pairing, open **Monarr web → Settings → Security → Reveal → Link a
mobile app**, enter the address the phone can reach, show the QR, then choose
**Scan pairing QR** in the native app. The code includes the API key; show it
only to a trusted device.

For manual entry, `localhost` points at the phone, not the Monarr host. A bare
direct host such as `192.168.1.20` automatically uses Monarr's default port,
`7676`; explicit ports and HTTPS reverse-proxy URLs are preserved. Bracket a
literal IPv6 address when including a port: `http://[fd12:3456::20]:7676`.

On first connect, copy the key from **Monarr web → Settings → Security →
Reveal**. The key is optional only while native API authentication is off.
An `http://` LAN address is supported for homelabs; use HTTPS or a trusted VPN
for access outside the local network because cleartext HTTP exposes the API
key in transit.

Under **More → Appearance**, Item size changes both Library tiles and Discover
results; Medium is the original layout. Theme starts on Auto, follows the
device's light/dark setting, and resolves an unspecified system setting to
dark. Both choices persist on that device.

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
- [`app.json`](app.json) — native identifiers, LAN discovery, camera access,
  icons, splash, and secure-storage plugins.
- [`eas.json`](eas.json) — preview and production signing/build profiles.
