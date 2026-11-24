import type { Availability } from './api'
import { fmtRelative } from './api'
import { availabilityLabel } from './availability'
export function AvailabilityBadge({value, checked=false}:{value?:Availability;checked?:boolean}) {
  return <span className="availability-facts"><span className={`pill ${value?.catalog==='present'&&value.presence==='present'?'pill-ok':value?.catalog==='ambiguous'||value?.catalog==='unknown'||value?.catalog==='present'&&value.presence==='unknown'?'pill-warning':'pill-neutral'}`}>{availabilityLabel(value)}</span>{value?.acquisition==='active'&&<span className="pill pill-neutral">Work in progress</span>}{checked&&value?.checkedAt&&<small className="muted">Last checked <time dateTime={value.checkedAt}>{fmtRelative(value.checkedAt)}</time></small>}</span>
}
