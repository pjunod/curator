const {
  IOSConfig,
  withAppDelegate,
  withInfoPlist,
} = require('expo/config-plugins')

const SCENE_DELEGATE_SOURCE = `import UIKit

class SceneDelegate: UIResponder, UIWindowSceneDelegate {
  var window: UIWindow?

  func scene(
    _ scene: UIScene,
    willConnectTo session: UISceneSession,
    options connectionOptions: UIScene.ConnectionOptions
  ) {
    guard
      let windowScene = scene as? UIWindowScene,
      let appDelegate = UIApplication.shared.delegate as? AppDelegate,
      let factory = appDelegate.reactNativeFactory
    else { return }

    let window = UIWindow(windowScene: windowScene)
    self.window = window
    appDelegate.window = window
    factory.startReactNative(
      withModuleName: "main",
      in: window,
      launchOptions: nil)
  }
}
`

const LEGACY_STARTUP_PATTERN = /\n\s*#if os\(iOS\) \|\| os\(tvOS\)\n\s*window = UIWindow\(frame: UIScreen\.main\.bounds\)\n\s*factory\.startReactNative\(\n\s*withModuleName: "main",\n\s*in: window,\n\s*launchOptions: launchOptions\)\n\s*#endif\n/

function patchAppDelegate(contents) {
  if (!contents.includes('factory.startReactNative')) {
    return contents
  }

  if (!LEGACY_STARTUP_PATTERN.test(contents)) {
    throw new Error(
      'Unable to move React Native startup into SceneDelegate: AppDelegate template changed.',
    )
  }

  return contents.replace(LEGACY_STARTUP_PATTERN, '\n')
}

function withIosSceneLifecycle(config) {
  config = withAppDelegate(config, (modConfig) => {
    modConfig.modResults.contents = patchAppDelegate(
      modConfig.modResults.contents,
    )
    return modConfig
  })

  config = withInfoPlist(config, (modConfig) => {
    modConfig.modResults.UIApplicationSceneManifest = {
      UIApplicationSupportsMultipleScenes: false,
      UISceneConfigurations: {
        UIWindowSceneSessionRoleApplication: [
          {
            UISceneConfigurationName: 'Default Configuration',
            UISceneDelegateClassName:
              '$(PRODUCT_MODULE_NAME).SceneDelegate',
          },
        ],
      },
    }
    return modConfig
  })

  return IOSConfig.XcodeProjectFile.withBuildSourceFile(config, {
    filePath: 'SceneDelegate.swift',
    contents: SCENE_DELEGATE_SOURCE,
    overwrite: true,
  })
}

module.exports = withIosSceneLifecycle
module.exports.patchAppDelegate = patchAppDelegate
module.exports.sceneDelegateSource = SCENE_DELEGATE_SOURCE
