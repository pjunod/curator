const { withAndroidStyles } = require('expo/config-plugins')

module.exports = function withAndroidSplashApiGuard(config) {
  return withAndroidStyles(config, (modConfig) => {
    const resources = modConfig.modResults.resources
    resources.$ ??= {}
    resources.$['xmlns:tools'] ??= 'http://schemas.android.com/tools'

    const styles = resources.style ?? []
    const splashStyle = styles.find(
      (style) => style.$?.name === 'Theme.App.SplashScreen',
    )
    const behavior = splashStyle?.item?.find(
      (item) =>
        item.$?.name === 'android:windowSplashScreenBehavior',
    )

    if (behavior) {
      behavior.$['tools:targetApi'] = '33'
    }

    return modConfig
  })
}
