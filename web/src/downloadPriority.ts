export const DOWNLOAD_PRIORITIES = [
  { value: -100, label: 'Very low' },
  { value: -50, label: 'Low' },
  { value: 0, label: 'Normal' },
  { value: 50, label: 'High' },
  { value: 100, label: 'Very high' },
  { value: 900, label: 'Force' },
] as const

export function downloadPriorityLabel(priority: number): string {
  return DOWNLOAD_PRIORITIES.find(({ value }) => value === priority)?.label ?? `Custom (${priority})`
}
