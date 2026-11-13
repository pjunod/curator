import type { Availability } from './types'

export type AvailabilityFilter = 'all' | 'to-get' | 'present' | 'unknown'
export function availabilityLabel(value?: Availability): string {
  if (!value || value.catalog === 'unknown') return 'Library check unavailable'
  if (value.catalog === 'ambiguous') return 'Conflicting library identity'
  if (value.catalog === 'absent') return 'Not in Curator'
  if (value.presence === 'unknown') return 'In Curator · availability unknown'
  if (value.presence === 'offloaded') return 'Offloaded'
  if (value.presence === 'partial') return `${value.present} of ${value.expected} aired episodes available`
  if (value.presence === 'missing') return 'Not available at last check'
  return value.seasons?.length ? `${value.present} of ${value.expected} aired episodes available` : 'Available in library'
}
export function matchesAvailability(value: Availability | undefined, filter: AvailabilityFilter): boolean {
  if (filter === 'all') return true
  if (filter === 'unknown') return !value || value.catalog === 'unknown' || value.catalog === 'ambiguous' || value.catalog === 'present' && value.presence === 'unknown'
  if (filter === 'present') return value?.catalog === 'present' && value.presence === 'present'
  return !(value?.catalog === 'present' && value.presence === 'present')
}
export function canAddFromAvailability(value?: Availability): boolean { return value?.catalog === 'absent' }
