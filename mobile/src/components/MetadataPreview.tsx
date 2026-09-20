import { Linking, StyleSheet, Text, View } from 'react-native'
import { useState } from 'react'
import { externalLinks } from '../metadataPreview'
import { useTheme } from '../theme'
import type { MetadataPreview, SearchResult } from '../types'
import { Button, InlineError, SectionTitle } from './UI'
import { Poster } from './Media'

export function PreviewDetails({ item, data, loading, error, conflict, retry }: {
  item: SearchResult; data?: MetadataPreview; loading: boolean; error?: Error; conflict: boolean; retry: () => void
}) {
  const theme = useTheme()
  const [linkError, setLinkError] = useState('')
  const links = externalLinks(item, data, conflict)
  return <>
    <View style={styles.hero}>
      <Poster path={data?.posterPath || item.posterPath} title={item.title} width={90} />
      <View style={styles.heading}>
        <Text accessibilityRole="header" style={[styles.title, { color: theme.text }]}>{data?.title || item.title}</Text>
        <Text style={[styles.meta, { color: theme.muted }]}>{data?.year || item.year || 'Year unknown'}{(data?.author || item.author) && ` · ${data?.author || item.author}`}</Text>
        {data?.runtimeMinutes ? <Text style={[styles.meta, { color: theme.muted }]}>{data.runtimeMinutes} min{item.kind === 'series' ? ' per episode' : ''}</Text> : null}
        {data?.status ? <Text style={[styles.meta, { color: theme.muted }]}>{data.status}{data.previewSource && ` · ${data.previewSource}`}</Text> : null}
      </View>
    </View>
    {data?.genres?.length ? <Text style={[styles.meta, { color: theme.muted }]}>{data.genres.join(' · ')}</Text> : null}
    <SectionTitle>Synopsis</SectionTitle>
    <Text style={[styles.synopsis, { color: theme.text }]}>{data?.overview || item.overview || 'No synopsis available.'}</Text>
    {loading ? <Text accessibilityLiveRegion="polite" style={[styles.meta, { color: theme.muted }]}>Loading more details…</Text> : null}
    {error ? <View style={styles.notice}><InlineError message={conflict ? 'The provider returned conflicting identities. Resolve this before adding.' : 'More details are unavailable. The original search information is still shown.'} /><Button compact secondary label="Retry details" onPress={retry} /></View> : null}
    {data?.ownership === 'unknown' ? <Text style={[styles.meta, { color: theme.muted }]}>Library ownership could not be checked.</Text> : null}
    {data?.addability !== 'supported' && data?.addBlockReason ? <InlineError message={data.addBlockReason} /> : null}
    <View style={styles.links}>{links.map((link) => <Button key={link.label} compact secondary label={`${link.label} ↗`} onPress={() => {
      setLinkError('')
      void Linking.openURL(link.url).catch(() => setLinkError(`Could not open ${link.label}.`))
    }} />)}</View>
    <InlineError message={linkError} />
  </>
}

const styles = StyleSheet.create({
  hero: { flexDirection: 'row', alignItems: 'flex-start', gap: 16 },
  heading: { flex: 1, gap: 8 },
  title: { fontSize: 24, lineHeight: 30, fontWeight: '800' },
  meta: { fontSize: 13, lineHeight: 19, marginTop: 7 },
  synopsis: { fontSize: 16, lineHeight: 25 },
  links: { flexDirection: 'row', flexWrap: 'wrap', gap: 10, marginTop: 24 },
  notice: { gap: 8, marginTop: 16 },
})
