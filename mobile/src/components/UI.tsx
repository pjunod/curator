import type { PropsWithChildren, ReactNode } from 'react'
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  TextInput,
  type TextInputProps,
  View,
  type ViewStyle,
} from 'react-native'
import { SafeAreaView } from 'react-native-safe-area-context'
import { useTheme, type Theme } from '../theme'

export function AppScreen({ children, scroll = false }: PropsWithChildren<{ scroll?: boolean }>) {
  const theme = useTheme()
  const body = scroll ? (
    <ScrollView contentContainerStyle={styles.scrollContent} keyboardShouldPersistTaps="handled">
      {children}
    </ScrollView>
  ) : (
    children
  )
  return (
    <SafeAreaView edges={['top', 'left', 'right']} style={[styles.screen, { backgroundColor: theme.bg }]}>
      {body}
    </SafeAreaView>
  )
}

export function Header({ title, subtitle, left, right }: { title: string; subtitle?: string; left?: ReactNode; right?: ReactNode }) {
  const theme = useTheme()
  return (
    <View style={[styles.header, { borderBottomColor: theme.border }]}>
      <View style={styles.headerSide}>{left}</View>
      <View style={styles.headerText}>
        <Text numberOfLines={1} style={[styles.headerTitle, { color: theme.text }]}>{title}</Text>
        {subtitle ? <Text numberOfLines={1} style={[styles.headerSubtitle, { color: theme.muted }]}>{subtitle}</Text> : null}
      </View>
      <View style={[styles.headerSide, styles.headerRight]}>{right}</View>
    </View>
  )
}

export function Wordmark({ size = 30 }: { size?: number }) {
  const theme = useTheme()
  return (
    <Text style={{ color: theme.text, fontSize: size, fontWeight: '800', letterSpacing: 0.2 }}>
      mon<Text style={{ color: theme.accent }}>arr</Text>
    </Text>
  )
}

export function Button({
  label,
  onPress,
  disabled = false,
  secondary = false,
  compact = false,
}: {
  label: string
  onPress: () => void
  disabled?: boolean
  secondary?: boolean
  compact?: boolean
}) {
  const theme = useTheme()
  return (
    <Pressable
      accessibilityRole="button"
      disabled={disabled}
      onPress={onPress}
      style={({ pressed }) => [
        styles.button,
        compact && styles.buttonCompact,
        {
          backgroundColor: secondary ? theme.raised : theme.accent,
          borderColor: secondary ? theme.border : theme.accent,
          opacity: disabled ? 0.45 : pressed ? 0.75 : 1,
        },
      ]}
    >
      <Text style={[styles.buttonText, { color: secondary ? theme.text : theme.onAccent }]}>{label}</Text>
    </Pressable>
  )
}

export function IconButton({ label, glyph, onPress }: { label: string; glyph: string; onPress: () => void }) {
  const theme = useTheme()
  return (
    <Pressable accessibilityLabel={label} accessibilityRole="button" onPress={onPress} hitSlop={10}>
      <Text style={[styles.iconButton, { color: theme.accent }]}>{glyph}</Text>
    </Pressable>
  )
}

export function Field(props: TextInputProps) {
  const theme = useTheme()
  return (
    <TextInput
      {...props}
      placeholderTextColor={theme.muted}
      selectionColor={theme.accent}
      style={[styles.field, { color: theme.text, backgroundColor: theme.raised, borderColor: theme.border }, props.style]}
    />
  )
}

export function Chip({ label, selected = false, onPress }: { label: string; selected?: boolean; onPress?: () => void }) {
  const theme = useTheme()
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ selected }}
      disabled={!onPress}
      onPress={onPress}
      style={[
        styles.chip,
        { backgroundColor: selected ? theme.accentSoft : theme.raised, borderColor: selected ? theme.accent : theme.border },
      ]}
    >
      <Text style={[styles.chipText, { color: selected ? theme.accent : theme.muted }]}>{label}</Text>
    </Pressable>
  )
}

export function Badge({ label, tone = 'neutral' }: { label: string; tone?: 'ok' | 'warning' | 'error' | 'neutral' }) {
  const theme = useTheme()
  const color = toneColor(theme, tone)
  return (
    <View style={[styles.badge, { borderColor: color, backgroundColor: `${color}18` }]}>
      <Text numberOfLines={1} style={[styles.badgeText, { color }]}>{label}</Text>
    </View>
  )
}

export function Panel({ children, style }: PropsWithChildren<{ style?: ViewStyle }>) {
  const theme = useTheme()
  return <View style={[styles.panel, { backgroundColor: theme.raised, borderColor: theme.border }, style]}>{children}</View>
}

export function SectionTitle({ children, action }: PropsWithChildren<{ action?: ReactNode }>) {
  const theme = useTheme()
  return (
    <View style={styles.sectionHead}>
      <Text style={[styles.sectionTitle, { color: theme.text }]}>{children}</Text>
      {action}
    </View>
  )
}

export function LoadingState({ label = 'Loading…' }: { label?: string }) {
  const theme = useTheme()
  return (
    <View style={styles.centerState}>
      <ActivityIndicator color={theme.accent} />
      <Text style={[styles.stateText, { color: theme.muted }]}>{label}</Text>
    </View>
  )
}

export function MessageState({ title, message, retry }: { title: string; message: string; retry?: () => void }) {
  const theme = useTheme()
  return (
    <View style={styles.centerState}>
      <Text style={[styles.stateTitle, { color: theme.text }]}>{title}</Text>
      <Text style={[styles.stateText, { color: theme.muted }]}>{message}</Text>
      {retry ? <Button label="Try again" secondary onPress={retry} /> : null}
    </View>
  )
}

export function InlineError({ message }: { message: string }) {
  const theme = useTheme()
  if (!message) return null
  return <Text style={[styles.inlineError, { color: theme.error }]}>{message}</Text>
}

function toneColor(theme: Theme, tone: 'ok' | 'warning' | 'error' | 'neutral') {
  if (tone === 'ok') return theme.ok
  if (tone === 'warning') return theme.warning
  if (tone === 'error') return theme.error
  return theme.muted
}

const styles = StyleSheet.create({
  screen: { flex: 1 },
  scrollContent: { padding: 16, paddingBottom: 40 },
  header: { height: 64, borderBottomWidth: StyleSheet.hairlineWidth, flexDirection: 'row', alignItems: 'center', paddingHorizontal: 14 },
  headerSide: { width: 64, justifyContent: 'center' },
  headerRight: { alignItems: 'flex-end' },
  headerText: { flex: 1, alignItems: 'center' },
  headerTitle: { fontSize: 18, fontWeight: '700' },
  headerSubtitle: { fontSize: 11, marginTop: 2 },
  button: { minHeight: 44, paddingHorizontal: 16, borderRadius: 12, borderWidth: 1, justifyContent: 'center', alignItems: 'center' },
  buttonCompact: { minHeight: 34, borderRadius: 9, paddingHorizontal: 11 },
  buttonText: { fontWeight: '700', fontSize: 14 },
  iconButton: { fontSize: 30, fontWeight: '400', lineHeight: 32 },
  field: { minHeight: 46, borderWidth: 1, borderRadius: 12, paddingHorizontal: 14, fontSize: 16 },
  chip: { minHeight: 34, borderWidth: 1, borderRadius: 17, paddingHorizontal: 12, alignItems: 'center', justifyContent: 'center' },
  chipText: { fontSize: 13, fontWeight: '600' },
  badge: { alignSelf: 'flex-start', maxWidth: '100%', borderWidth: 1, borderRadius: 8, paddingHorizontal: 7, paddingVertical: 3 },
  badgeText: { fontSize: 11, fontWeight: '700' },
  panel: { borderWidth: 1, borderRadius: 16, padding: 14 },
  sectionHead: { marginTop: 22, marginBottom: 10, flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center' },
  sectionTitle: { fontSize: 18, fontWeight: '700' },
  centerState: { flex: 1, minHeight: 220, padding: 30, alignItems: 'center', justifyContent: 'center', gap: 12 },
  stateTitle: { fontSize: 18, fontWeight: '700', textAlign: 'center' },
  stateText: { fontSize: 14, lineHeight: 20, textAlign: 'center' },
  inlineError: { fontSize: 13, lineHeight: 19 },
})
