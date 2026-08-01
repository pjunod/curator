export function buildPairingCode(server: string, apiKey: string): string {
  const code = new URL('monarr://pair')
  code.searchParams.set('v', '1')
  code.searchParams.set('server', server.trim())
  code.searchParams.set('key', apiKey.trim())
  return code.toString()
}
