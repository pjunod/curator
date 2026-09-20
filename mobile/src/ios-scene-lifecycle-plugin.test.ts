import { describe, expect, it } from 'vitest'

const plugin = require('../plugins/withIosSceneLifecycle') as {
  mergeSceneManifest: (existing?: Record<string, unknown>) => Record<string, any>
  patchAppDelegate: (contents: string) => string
  sceneDelegateSource: string
}

const appDelegate = `import UIKit

class AppDelegate: ExpoAppDelegate {
  var reactNativeFactory: RCTReactNativeFactory?

  override func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
  ) -> Bool {
    reactNativeFactory = factory

    #if os(iOS) || os(tvOS)
    window = UIWindow(frame: UIScreen.main.bounds)
    factory.startReactNative(
      withModuleName: "main",
      in: window,
      launchOptions: launchOptions)
    #endif

    return super.application(application, didFinishLaunchingWithOptions: launchOptions)
  }
}
`

describe('withIosSceneLifecycle', () => {
  it('moves React Native startup out of AppDelegate', () => {
    const patched = plugin.patchAppDelegate(appDelegate)

    expect(patched).not.toContain('UIWindow(frame:')
    expect(patched).not.toContain('factory.startReactNative')
    expect(patched).toContain('reactNativeFactory = factory')
  })

  it('is idempotent', () => {
    const patched = plugin.patchAppDelegate(appDelegate)

    expect(plugin.patchAppDelegate(patched)).toBe(patched)
  })

  it('fails when the generated AppDelegate template drifts', () => {
    expect(() =>
      plugin.patchAppDelegate('factory.startReactNative(withModuleName: "main")'),
    ).toThrow(/AppDelegate template changed/)
  })

  it('starts React Native from a UIWindowScene', () => {
    expect(plugin.sceneDelegateSource).toContain(
      'let window = UIWindow(windowScene: windowScene)',
    )
    expect(plugin.sceneDelegateSource).toContain(
      'let factory = appDelegate.reactNativeFactory',
    )
    expect(plugin.sceneDelegateSource).toContain('withModuleName: "main"')
    expect(plugin.sceneDelegateSource).toContain(
      'launchOptions: launchOptions(from: connectionOptions)',
    )
    expect(plugin.sceneDelegateSource).toContain('openURLContexts URLContexts')
    expect(plugin.sceneDelegateSource).toContain('continue userActivity')
  })

  it('adds the application scene while preserving other scene roles', () => {
    const manifest = plugin.mergeSceneManifest({
      CustomKey: 'preserved',
      UISceneConfigurations: {
        UIWindowSceneSessionRoleExternalDisplay: [{ Name: 'external' }],
      },
    })

    expect(manifest.CustomKey).toBe('preserved')
    expect(manifest.UIApplicationSupportsMultipleScenes).toBe(false)
    expect(
      manifest.UISceneConfigurations.UIWindowSceneSessionRoleExternalDisplay,
    ).toEqual([{ Name: 'external' }])
    expect(
      manifest.UISceneConfigurations.UIWindowSceneSessionRoleApplication,
    ).toEqual([
      {
        UISceneConfigurationName: 'Default Configuration',
        UISceneDelegateClassName: '$(PRODUCT_MODULE_NAME).SceneDelegate',
      },
    ])
  })

  it('preserves a compatible existing application scene', () => {
    const applicationScenes = [
      {
        UISceneConfigurationName: 'Existing name',
        UISceneDelegateClassName: '$(PRODUCT_MODULE_NAME).SceneDelegate',
      },
    ]

    const manifest = plugin.mergeSceneManifest({
      UISceneConfigurations: {
        UIWindowSceneSessionRoleApplication: applicationScenes,
      },
    })

    expect(
      manifest.UISceneConfigurations.UIWindowSceneSessionRoleApplication,
    ).toEqual(applicationScenes)
  })

  it('rejects incompatible existing scene configuration', () => {
    expect(() =>
      plugin.mergeSceneManifest({
        UISceneConfigurations: {
          UIWindowSceneSessionRoleApplication: [
            { UISceneDelegateClassName: 'Other.SceneDelegate' },
          ],
        },
      }),
    ).toThrow(/incompatible application scene configuration/)

    expect(() =>
      plugin.mergeSceneManifest({ UIApplicationSupportsMultipleScenes: true }),
    ).toThrow(/multiple scenes are already enabled/)
  })
})
