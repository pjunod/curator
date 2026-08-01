import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { DefaultProfiles, ProfileInput, QualityProfile } from '../api'
import {
  createProfile,
  deleteProfile,
  getProfiles,
  getSettings,
  updateProfile,
  updateSettings,
} from '../api'
import { DOWNLOAD_PRIORITIES, downloadPriorityLabel } from '../downloadPriority'

// The quality vocabulary, worst to best on each axis. Kept here rather than
// fetched because it is a property of the model, not of the deployment — and
// a picker that cannot render until a request lands is a picker that flickers.
const VIDEO_SOURCES = [
  { value: 'hdtv', label: 'HDTV' },
  { value: 'webrip', label: 'WEBRip' },
  { value: 'webdl', label: 'WEB-DL' },
  { value: 'bluray', label: 'Bluray' },
  { value: 'remux', label: 'Remux' },
]
const RESOLUTIONS = [
  { value: 480, label: '480p' },
  { value: 720, label: '720p' },
  { value: 1080, label: '1080p' },
  { value: 2160, label: '2160p (4K)' },
]
const EBOOK_FORMATS = [
  { value: 'pdf', label: 'PDF' },
  { value: 'mobi', label: 'MOBI' },
  { value: 'azw3', label: 'AZW3' },
  { value: 'epub', label: 'EPUB' },
]
const AUDIOBOOK_FORMATS = [
  { value: 'mp3', label: 'MP3' },
  { value: 'm4b', label: 'M4B' },
]

type Axis = 'video' | 'ebook' | 'audiobook'

function axisOf(source: string): Axis {
  if (EBOOK_FORMATS.some((f) => f.value === source)) return 'ebook'
  if (AUDIOBOOK_FORMATS.some((f) => f.value === source)) return 'audiobook'
  return 'video'
}

function sourcesFor(axis: Axis) {
  if (axis === 'ebook') return EBOOK_FORMATS
  if (axis === 'audiobook') return AUDIOBOOK_FORMATS
  return VIDEO_SOURCES
}

// blankDraft is a new profile before anyone has typed: a 1080p target, which
// is what most people want and what the seeded default already says.
const blankDraft = (): Draft => ({
  name: '',
  axis: 'video',
  targetSource: 'webdl',
  targetResolution: 1080,
  hasFloor: false,
  floorSource: 'hdtv',
  floorResolution: 1080,
  upgradesAllowed: true,
  downloadPriority: 0,
})

interface Draft {
  name: string
  axis: Axis
  targetSource: string
  targetResolution: number
  hasFloor: boolean
  floorSource: string
  floorResolution: number
  upgradesAllowed: boolean
  downloadPriority: number
}

function draftOf(p: QualityProfile): Draft {
  const axis = axisOf(p.target.source)
  return {
    name: p.name,
    axis,
    targetSource: p.target.source,
    targetResolution: p.target.resolution,
    hasFloor: !!p.floor,
    floorSource: p.floor?.source ?? p.target.source,
    floorResolution: p.floor?.resolution ?? p.target.resolution,
    upgradesAllowed: p.upgradesAllowed,
    downloadPriority: p.downloadPriority,
  }
}

function toInput(d: Draft): ProfileInput {
  const res = d.axis === 'video' ? d.targetResolution : 0
  const body: ProfileInput = {
    name: d.name.trim(),
    target: { source: d.targetSource, resolution: res },
    upgradesAllowed: d.upgradesAllowed,
    downloadPriority: d.downloadPriority,
  }
  if (d.hasFloor) {
    body.floor = {
      source: d.floorSource,
      resolution: d.axis === 'video' ? d.floorResolution : 0,
    }
  }
  return body
}

// previewSentence mirrors the server's Profile.Sentence so the editor can show
// what a profile will say about itself BEFORE it is saved. The server remains
// the authority — every saved profile renders the sentence the server sent —
// but a picker whose consequence you only learn after saving is a guessing
// game, and guessing games are how "Any" happened.
function previewSentence(d: Draft): string {
  const label = (source: string, resolution: number) => {
    const name = sourcesFor(d.axis).find((s) => s.value === source)?.label ?? source
    return resolution > 0 ? `${name} ${resolution}p` : name
  }
  const res = d.axis === 'video' ? d.targetResolution : 0
  let s = `hunts the best release up to ${label(d.targetSource, res)}, then stops`
  if (d.hasFloor) {
    s += `; never below ${label(d.floorSource, d.axis === 'video' ? d.floorResolution : 0)}`
  }
  if (!d.upgradesAllowed) s += '; no upgrades once a file is present'
  return s
}

// The three kinds a default can be set for, in the order the library lists
// them. Books share one setting: a book item is one kind, and whether it wants
// EPUB or M4B is exactly what choosing a profile decides.
const DEFAULT_KINDS: { key: keyof DefaultProfiles; label: string; book: boolean }[] = [
  { key: 'movie', label: 'Movies', book: false },
  { key: 'series', label: 'Series', book: false },
  { key: 'book', label: 'Books', book: true },
]

/**
 * DefaultProfileSettings: what a newly added item gets when the add form does
 * not name one.
 *
 * This was a constant in the server — profile 1, or Ebook for books — with no
 * UI anywhere. Anyone who wanted 4K films had to open every title they added
 * and change it by hand, and nothing on screen ever said what was about to
 * happen. One default per kind rather than one global one, because a book
 * cannot use a video profile at all and wanting 4K films beside 1080p
 * television is the ordinary case.
 */
function DefaultProfileSettings(props: { profiles: QualityProfile[] }) {
  const qc = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const [error, setError] = useState('')

  const save = useMutation({
    mutationFn: (patch: Partial<DefaultProfiles>) => updateSettings({ defaultProfiles: patch }),
    onSuccess: () => {
      setError('')
      void qc.invalidateQueries({ queryKey: ['settings'] })
    },
    onError: (e) => setError((e as Error).message),
  })

  const defaults = settings.data?.defaultProfiles
  if (!defaults) return null

  return (
    <div className="profile-defaults" data-testid="profile-defaults">
      <h3>Defaults for new items</h3>
      <p className="muted">
        What an item gets when you add it without picking a profile. Changing this does not
        touch anything already in the library.
      </p>
      <div className="form-row">
        {DEFAULT_KINDS.map(({ key, label, book }) => {
          // Only same-axis profiles are offered: format families never
          // compete, so a book on a video profile would sit wanted forever
          // with nothing able to satisfy it.
          const eligible = props.profiles.filter((p) => (axisOf(p.target.source) !== 'video') === book)
          return (
            <label key={key}>
              {label}
              <select
                aria-label={`Default profile for ${label}`}
                value={defaults[key]}
                disabled={save.isPending || eligible.length === 0}
                onChange={(e) => save.mutate({ [key]: Number(e.target.value) })}
              >
                {eligible.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            </label>
          )
        })}
      </div>
      {error && <p className="error-text">✕ {error}</p>}
    </div>
  )
}

function ProfileForm(props: {
  draft: Draft
  onChange: (d: Draft) => void
  onSubmit: () => void
  onCancel?: () => void
  busy: boolean
  submitLabel: string
  error?: string
}) {
  const { draft: d, onChange } = props
  const set = (patch: Partial<Draft>) => onChange({ ...d, ...patch })
  const sources = sourcesFor(d.axis)

  return (
    <div className="profile-form">
      <div className="form-row">
        <label>
          Name
          <input
            aria-label="Profile name"
            value={d.name}
            placeholder="1080p"
            onChange={(e) => set({ name: e.target.value })}
          />
        </label>
        <label>
          Kind
          <select
            value={d.axis}
            onChange={(e) => {
              const axis = e.target.value as Axis
              const first = sourcesFor(axis)
              set({
                axis,
                targetSource: axis === 'video' ? 'webdl' : first[first.length - 1].value,
                floorSource: first[0].value,
              })
            }}
          >
            <option value="video">Film &amp; TV</option>
            <option value="ebook">Ebook</option>
            <option value="audiobook">Audiobook</option>
          </select>
        </label>
      </div>

      <div className="form-row">
        <label>
          Target
          <select
            aria-label="Target source"
            value={d.targetSource}
            onChange={(e) => set({ targetSource: e.target.value })}
          >
            {sources.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </select>
        </label>
        {d.axis === 'video' && (
          <label>
            Resolution
            <select
              aria-label="Target resolution"
              value={d.targetResolution}
              onChange={(e) => set({ targetResolution: Number(e.target.value) })}
            >
              {RESOLUTIONS.map((r) => (
                <option key={r.value} value={r.value}>
                  {r.label}
                </option>
              ))}
            </select>
          </label>
        )}
      </div>

      <label className="checkbox-row">
        <input
          type="checkbox"
          checked={d.hasFloor}
          onChange={(e) => set({ hasFloor: e.target.checked })}
        />
        Set a floor — below this, do not grab at all
      </label>

      <label>
        Download priority
        <select
          aria-label="Download priority"
          value={d.downloadPriority}
          onChange={(e) => set({ downloadPriority: Number(e.target.value) })}
        >
          {DOWNLOAD_PRIORITIES.map((priority) => (
            <option key={priority.value} value={priority.value}>
              {priority.label}
            </option>
          ))}
        </select>
        <span className="muted"> Applied when an item does not have its own override.</span>
      </label>
      {d.hasFloor && (
        <div className="form-row">
          <label>
            Floor
            <select
              aria-label="Floor source"
              value={d.floorSource}
              onChange={(e) => set({ floorSource: e.target.value })}
            >
              {sources.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.label}
                </option>
              ))}
            </select>
          </label>
          {d.axis === 'video' && (
            <label>
              Floor resolution
              <select
                aria-label="Floor resolution"
                value={d.floorResolution}
                onChange={(e) => set({ floorResolution: Number(e.target.value) })}
              >
                {RESOLUTIONS.map((r) => (
                  <option key={r.value} value={r.value}>
                    {r.label}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      )}

      <label className="checkbox-row">
        <input
          type="checkbox"
          checked={d.upgradesAllowed}
          onChange={(e) => set({ upgradesAllowed: e.target.checked })}
        />
        Keep looking for better until the target is met
      </label>

      <p className="profile-sentence" data-testid="profile-preview">
        <strong>{d.name.trim() || 'This profile'}</strong> — {previewSentence(d)}
      </p>

      <div className="form-actions">
        <button onClick={props.onSubmit} disabled={props.busy || !d.name.trim()}>
          {props.submitLabel}
        </button>
        {props.onCancel && <button onClick={props.onCancel}>Cancel</button>}
        {props.error && <span className="error-text"> ✕ {props.error}</span>}
      </div>
    </div>
  )
}

/**
 * QualityProfileSettings: the editor that should have existed from the start.
 *
 * A model users cannot see or change is how "Any" happened — five seeded rows
 * were the only profiles that had ever existed, so nobody ever had to make the
 * old allowed-list-plus-cutoff shape express an intent, and nobody noticed it
 * could not (ADR 0014 §5).
 */
export function QualityProfileSettings() {
  const qc = useQueryClient()
  const profiles = useQuery({ queryKey: ['profiles'], queryFn: getProfiles })
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const [creating, setCreating] = useState(false)
  const [draft, setDraft] = useState<Draft>(blankDraft())
  const [editingId, setEditingId] = useState<number | null>(null)
  const [editDraft, setEditDraft] = useState<Draft>(blankDraft())
  const [error, setError] = useState('')

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['profiles'] })
    void qc.invalidateQueries({ queryKey: ['library'] })
    void qc.invalidateQueries({ queryKey: ['settings'] })
  }

  // Which kinds start on this profile. Shown in the table because "which one
  // is the default" is the question the settings screen exists to answer, and
  // a picker further down the page answers it only if you scroll to it.
  const defaultFor = (id: number) => {
    const d = settings.data?.defaultProfiles
    if (!d) return []
    return DEFAULT_KINDS.filter((k) => d[k.key] === id).map((k) => k.label)
  }

  const create = useMutation({
    mutationFn: () => createProfile(toInput(draft)),
    onSuccess: () => {
      setCreating(false)
      setDraft(blankDraft())
      setError('')
      refresh()
    },
    onError: (e) => setError((e as Error).message),
  })
  const save = useMutation({
    mutationFn: () => updateProfile(editingId as number, toInput(editDraft)),
    onSuccess: () => {
      setEditingId(null)
      setError('')
      refresh()
    },
    onError: (e) => setError((e as Error).message),
  })
  const remove = useMutation({
    mutationFn: deleteProfile,
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (e) => setError((e as Error).message),
  })

  return (
    <section className="panel" id="profiles">
      <h2>Quality profiles</h2>
      <p className="muted">
        A profile is a target: hunt the best release at or below the target&apos;s resolution,
        keep looking while what&apos;s on disk is below it, and stop once it&apos;s met. An
        optional floor says what&apos;s not worth grabbing at all.
      </p>

      {profiles.data && profiles.data.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>What it does</th>
              <th>Priority</th>
              <th>Default for</th>
              <th>In use</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {profiles.data.map((p) => {
              const isDefaultFor = defaultFor(p.id)
              return (
                <tr key={p.id}>
                  <td>
                    <strong>{p.name}</strong>
                  </td>
                  <td className="muted">{p.sentence}</td>
                  <td>{downloadPriorityLabel(p.downloadPriority)}</td>
                  <td>
                    {isDefaultFor.length === 0 ? (
                      <span className="muted">—</span>
                    ) : (
                      isDefaultFor.map((label) => (
                        <span key={label} className="default-badge">
                          {label}
                        </span>
                      ))
                    )}
                  </td>
                  <td className="muted">{p.inUse ? `${p.inUse}` : '—'}</td>
                  <td>
                    <button
                      onClick={() => {
                        setEditingId(p.id)
                        setEditDraft(draftOf(p))
                        setError('')
                      }}
                    >
                      Edit
                    </button>{' '}
                    <button
                      onClick={() => remove.mutate(p.id)}
                      disabled={remove.isPending || !!p.inUse || isDefaultFor.length > 0}
                      title={
                        isDefaultFor.length > 0
                          ? `New ${isDefaultFor.join(' and ')} use this profile — pick a different default first`
                          : p.inUse
                            ? `${p.inUse} item(s), cop(ies) or import list(s) still use this profile`
                            : undefined
                      }
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}

      {profiles.data && profiles.data.length > 0 && (
        <DefaultProfileSettings profiles={profiles.data} />
      )}

      {editingId !== null && (
        <>
          <h3>Edit profile</h3>
          <ProfileForm
            draft={editDraft}
            onChange={setEditDraft}
            onSubmit={() => save.mutate()}
            onCancel={() => {
              setEditingId(null)
              setError('')
            }}
            busy={save.isPending}
            submitLabel="Save"
            error={error}
          />
        </>
      )}

      {creating ? (
        <>
          <h3>New profile</h3>
          <ProfileForm
            draft={draft}
            onChange={setDraft}
            onSubmit={() => create.mutate()}
            onCancel={() => {
              setCreating(false)
              setError('')
            }}
            busy={create.isPending}
            submitLabel="Create"
            error={error}
          />
        </>
      ) : (
        <p>
          <button
            onClick={() => {
              setCreating(true)
              setEditingId(null)
              setError('')
            }}
          >
            New profile
          </button>
          {error && !editingId && <span className="error-text"> ✕ {error}</span>}
        </p>
      )}
    </section>
  )
}
