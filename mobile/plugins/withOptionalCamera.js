const { withAndroidManifest } = require('expo/config-plugins')

module.exports = function withOptionalCamera(config) {
  return withAndroidManifest(config, (modConfig) => {
    const manifest = modConfig.modResults.manifest
    const features = manifest['uses-feature'] ?? []
    let camera = features.find(
      (feature) => feature.$?.['android:name'] === 'android.hardware.camera',
    )
    if (!camera) {
      camera = { $: { 'android:name': 'android.hardware.camera' } }
      features.push(camera)
    }
    camera.$['android:required'] = 'false'
    manifest['uses-feature'] = features
    return modConfig
  })
}
