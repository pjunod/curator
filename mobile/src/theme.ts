import { useColorScheme } from 'react-native'
import { usePreferences } from './preferences-context'
import { resolveTheme, type PalettePreference } from './preferences'

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

type ThemeColors = Omit<Theme, 'dark' | 'accentSoft'>
type PaletteThemes = { dark: Theme; light?: Theme }

function alpha(hex: string, opacity: number): string {
  const value = hex.replace('#', '')
  const red = Number.parseInt(value.slice(0, 2), 16)
  const green = Number.parseInt(value.slice(2, 4), 16)
  const blue = Number.parseInt(value.slice(4, 6), 16)
  return `rgba(${red}, ${green}, ${blue}, ${opacity})`
}

function makeTheme(dark: boolean, colors: ThemeColors): Theme {
  return { dark, ...colors, accentSoft: alpha(colors.accent, dark ? 0.16 : 0.14) }
}

const palettes: Record<PalettePreference, PaletteThemes> = {
  classic: {
    dark: makeTheme(true, {
      bg: '#0e1117', raised: '#151a23', hover: '#1a202c', border: '#232a38',
      text: '#e6e9ef', muted: '#9aa3b2', accent: '#8b7cf6', onAccent: '#0e1117',
      ok: '#3fb27f', warning: '#e5a53d', error: '#e05d5d',
    }),
    light: makeTheme(false, {
      bg: '#f4f5f8', raised: '#ffffff', hover: '#e9ebf1', border: '#d7dbe4',
      text: '#1b212b', muted: '#5b6472', accent: '#6a58d8', onAccent: '#ffffff',
      ok: '#1f8e5c', warning: '#99660f', error: '#c23b3b',
    }),
  },
  terminal: {
    dark: makeTheme(true, {
      bg: '#050705', raised: '#0b100b', hover: '#111a12', border: '#1e3020',
      text: '#c9e8c0', muted: '#7a9670', accent: '#3fe170', onAccent: '#041508',
      ok: '#3fe170', warning: '#d9c25a', error: '#ff7066',
    }),
    light: makeTheme(false, {
      bg: '#eee8d5', raised: '#fdf6e3', hover: '#e6dfc8', border: '#d5cdb4',
      text: '#073642', muted: '#657b83', accent: '#6e7f00', onAccent: '#fdf6e3',
      ok: '#5c6900', warning: '#7d5e00', error: '#ba2a27',
    }),
  },
  noirr: {
    dark: makeTheme(true, {
      bg: '#0a0a0c', raised: '#101014', hover: '#16161b', border: '#242429',
      text: '#ededef', muted: '#9a9aa3', accent: '#e5484d', onAccent: '#ffffff',
      ok: '#5fb582', warning: '#d9a05b', error: '#ff7a66',
    }),
    light: makeTheme(false, {
      bg: '#f2efe8', raised: '#faf8f2', hover: '#ffffff', border: '#d8d5cf',
      text: '#1a1a1e', muted: '#5d5c63', accent: '#c2343a', onAccent: '#ffffff',
      ok: '#307a54', warning: '#8d6425', error: '#aa5438',
    }),
  },
  amber: {
    dark: makeTheme(true, {
      bg: '#191a1d', raised: '#212327', hover: '#2a2d32', border: '#383c43',
      text: '#eceef0', muted: '#9aa0a7', accent: '#e5a00d', onAccent: '#1c1303',
      ok: '#52b788', warning: '#f2c14e', error: '#ee7168',
    }),
    light: makeTheme(false, {
      bg: '#f3f4f6', raised: '#ffffff', hover: '#e9ebee', border: '#d5d9de',
      text: '#1e2124', muted: '#5f666d', accent: '#8b5e00', onAccent: '#ffffff',
      ok: '#246b49', warning: '#8a6116', error: '#bd332d',
    }),
  },
  giallo: {
    dark: makeTheme(true, {
      bg: '#0c0a06', raised: '#14100a', hover: '#1c160d', border: '#332a1d',
      text: '#f2e9d8', muted: '#a89c85', accent: '#e8a33d', onAccent: '#1a1002',
      ok: '#5fb582', warning: '#c9723a', error: '#e5484d',
    }),
    light: makeTheme(false, {
      bg: '#f5eed9', raised: '#fbf7ea', hover: '#ffffff', border: '#d8cdb2',
      text: '#241d10', muted: '#6e6350', accent: '#7f4e00', onAccent: '#ffffff',
      ok: '#2c734d', warning: '#8a6116', error: '#b23a35',
    }),
  },
  silver: {
    dark: makeTheme(true, {
      bg: '#0a0a0b', raised: '#131315', hover: '#1b1b1e', border: '#29292c',
      text: '#f2f2f2', muted: '#9a9a9e', accent: '#e8e8ea', onAccent: '#0a0a0b',
      ok: '#9fbfa8', warning: '#c9b48c', error: '#d09088',
    }),
    light: makeTheme(false, {
      bg: '#f4f4f2', raised: '#ffffff', hover: '#eaeae7', border: '#d5d5d1',
      text: '#141416', muted: '#66666a', accent: '#1a1a1c', onAccent: '#ffffff',
      ok: '#467353', warning: '#765f28', error: '#a05248',
    }),
  },
  void: {
    dark: makeTheme(true, {
      bg: '#000000', raised: '#0a0a0a', hover: '#131313', border: '#292929',
      text: '#e8e8e8', muted: '#8a8a8a', accent: '#4cc2ff', onAccent: '#001018',
      ok: '#34d399', warning: '#fbbf24', error: '#f87171',
    }),
  },
  vhs: {
    dark: makeTheme(true, {
      bg: '#140d22', raised: '#1d1430', hover: '#291c42', border: '#492647',
      text: '#f4e9ff', muted: '#a78fc7', accent: '#ff4fd8', onAccent: '#22041c',
      ok: '#3ddc97', warning: '#ffb454', error: '#ff5c7a',
    }),
  },
  paper: {
    dark: makeTheme(true, {
      bg: '#171715', raised: '#201f1c', hover: '#2a2823', border: '#454139',
      text: '#f2eee6', muted: '#aaa49a', accent: '#9db2ff', onAccent: '#101322',
      ok: '#6fc49a', warning: '#e2c36f', error: '#ff8a80',
    }),
    light: makeTheme(false, {
      bg: '#f4f0e8', raised: '#fffdf8', hover: '#e9e2d7', border: '#c8bfb2',
      text: '#1f2328', muted: '#5f625f', accent: '#3451b2', onAccent: '#ffffff',
      ok: '#26724c', warning: '#765c00', error: '#b3261e',
    }),
  },
  tide: {
    dark: makeTheme(true, {
      bg: '#071412', raised: '#0d1e1b', hover: '#142a25', border: '#2a4841',
      text: '#e4f2ed', muted: '#93aaa2', accent: '#73d6b1', onAccent: '#052019',
      ok: '#6fd39f', warning: '#e5c875', error: '#ff8a80',
    }),
    light: makeTheme(false, {
      bg: '#edf4f0', raised: '#fbfdfa', hover: '#dfeae4', border: '#bccfc5',
      text: '#14201c', muted: '#586a62', accent: '#176b54', onAccent: '#ffffff',
      ok: '#196b48', warning: '#735b0b', error: '#b33a32',
    }),
  },
  panoptic: {
    dark: makeTheme(true, {
      bg: '#0a0a0f', raised: '#10131b', hover: '#171c25', border: '#293e48',
      text: '#e8eaed', muted: '#9eaab2', accent: '#00d4ff', onAccent: '#001014',
      ok: '#5ce1b4', warning: '#ffd178', error: '#ff8191',
    }),
  },
  redline: {
    dark: makeTheme(true, {
      bg: '#070708', raised: '#111214', hover: '#191a1d', border: '#44282c',
      text: '#f0eded', muted: '#aaa1a3', accent: '#ff5964', onAccent: '#170204',
      ok: '#6ccf9a', warning: '#f6c760', error: '#ff9f70',
    }),
  },
  panovic: {
    dark: makeTheme(true, {
      bg: '#000000', raised: '#181818', hover: '#242424', border: 'rgba(255, 255, 255, 0.06)',
      text: '#e8e6e1', muted: '#9a9aa0', accent: '#e8871e', onAccent: '#150b00',
      ok: '#5fb582', warning: '#d9a05b', error: '#ff7a66',
    }),
  },
  copper: {
    dark: makeTheme(true, {
      bg: '#000000', raised: '#181818', hover: '#242424', border: 'rgba(255, 255, 255, 0.06)',
      text: '#e8e6e1', muted: '#9a9aa0', accent: '#cf7643', onAccent: '#160b06',
      ok: '#5fb582', warning: '#d9a05b', error: '#ff7a66',
    }),
  },
}

export function paletteTheme(palette: PalettePreference, mode: 'light' | 'dark'): Theme {
  const choices = palettes[palette]
  return mode === 'light' && choices.light ? choices.light : choices.dark
}

export function useTheme(): Theme {
  const system = useColorScheme()
  const preferences = usePreferences()
  const mode = resolveTheme(preferences.theme, system, preferences.palette)
  return paletteTheme(preferences.palette, mode)
}
