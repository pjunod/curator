import {
  APIKeySettings,
  MobilePairingSettings,
  UserLoginSettings,
} from './SettingsDepth'

// AccessPage keeps every way into Curator in one place: browser login,
// mobile pairing, and the raw API credential used by integrations.
export function AccessPage() {
  return (
    <>
      <header className="page-head">
        <h1>Access</h1>
      </header>

      <UserLoginSettings />
      <MobilePairingSettings />
      <APIKeySettings />
    </>
  )
}
