import type { MediaKind, RootFolder } from './types'

export function compatibleRootFolders(roots: RootFolder[], kind: MediaKind): RootFolder[] {
  return roots.filter((root) => root.kind === kind || root.kind === 'mixed')
}

export function resolveRootFolderID(current: number, roots: RootFolder[]): number {
  return roots.some((root) => root.id === current) ? current : (roots[0]?.id ?? 0)
}
