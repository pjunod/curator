import { describe, expect, it } from 'vitest'

describe('iOS device build script', () => {
  it('installs a self-contained Release build', () => {
    const packageJson = require('../package.json') as {
      scripts?: Record<string, string>
    }

    expect(packageJson.scripts?.['ios:device']).toBe(
      'expo run:ios --configuration Release --device',
    )
  })

  it('keeps the app release and native build identifiers aligned', () => {
    const packageJson = require('../package.json') as { version: string }
    const appJson = require('../app.json') as {
      expo: {
        version: string
        ios: { buildNumber: string }
        android: { versionCode: number }
      }
    }

    expect(appJson.expo.version).toBe(packageJson.version)
    expect(Number(appJson.expo.ios.buildNumber)).toBe(appJson.expo.android.versionCode)
  })
})
