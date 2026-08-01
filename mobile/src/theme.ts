import { useColorScheme } from 'react-native'
import { usePreferences } from './preferences-context'
import { resolveTheme } from './preferences'

export interface Theme {
  dark: boolean
  bg: string
  raised: string
  hover: string
  border: string
  text: string
  muted: string
  accent: string
  onAccent: string
  accentSoft: string
  ok: string
  warning: string
  error: string
}

const dark: Theme = {
  dark: true,
  bg: '#0e1117',
  raised: '#151a23',
  hover: '#1a202c',
  border: '#232a38',
  text: '#e6e9ef',
  muted: '#9aa3b2',
  accent: '#8b7cf6',
  onAccent: '#0e1117',
  accentSoft: 'rgba(139, 124, 246, 0.16)',
  ok: '#3fb27f',
  warning: '#e5a53d',
  error: '#e05d5d',
}

const light: Theme = {
  dark: false,
  bg: '#f4f5f8',
  raised: '#ffffff',
  hover: '#e9ebf1',
  border: '#d7dbe4',
  text: '#1b212b',
  muted: '#5b6472',
  accent: '#6a58d8',
  onAccent: '#ffffff',
  accentSoft: 'rgba(106, 88, 216, 0.14)',
  ok: '#1f8e5c',
  warning: '#99660f',
  error: '#c23b3b',
}

export function useTheme(): Theme {
  const system = useColorScheme()
  const preferences = usePreferences()
  return resolveTheme(preferences.theme, system) === 'light' ? light : dark
}
