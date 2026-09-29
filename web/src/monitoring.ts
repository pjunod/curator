export type SeriesMonitorMode = 'all' | 'latest' | 'future' | 'new_seasons' | 'none'

export const SERIES_MONITOR_MODES: { value: SeriesMonitorMode; label: string; description: string }[] = [
  { value: 'all', label: 'All episodes', description: 'Select all regular seasons and episodes, including future additions.' },
  { value: 'latest', label: 'Latest and future seasons', description: 'Select the latest season and seasons that follow it.' },
  { value: 'future', label: 'Future episodes', description: 'Select episodes airing today or later, including new seasons. Earlier episodes stay unselected.' },
  { value: 'new_seasons', label: 'New seasons', description: 'Select seasons that have not started airing yet and seasons announced later.' },
  { value: 'none', label: 'Manual selection', description: 'Clear selections. Choose seasons and episodes yourself; new seasons stay unselected.' },
]

export function monitorLabel(mode?: SeriesMonitorMode): string {
  return SERIES_MONITOR_MODES.find((option) => option.value === (mode ?? 'all'))?.label ?? 'All episodes'
}
