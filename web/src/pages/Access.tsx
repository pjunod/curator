import {CinemaConnectionSettings} from '../Cinema'
import {HouseholdAccounts} from '../Household'
import {useQuery} from '@tanstack/react-query'
import {getSignInInfo} from '../api'
import {
  APIKeySettings,
  MobilePairingSettings,
  UserLoginSettings,
} from './SettingsDepth'

// AccessPage keeps every way into Curator in one place: browser login,
// mobile pairing, and the raw API credential used by integrations.
export function AccessPage() {
 const info=useQuery({queryKey:['sign-in-info'],queryFn:getSignInInfo})
  return (
    <>
      <header className="page-head">
        <h1>Access</h1>
      </header>

      {!info.data?.household&&<UserLoginSettings />}
      <HouseholdAccounts />
      <CinemaConnectionSettings/>
      <MobilePairingSettings />
      <APIKeySettings />
    </>
  )
}
