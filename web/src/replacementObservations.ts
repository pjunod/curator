export function mapReplacementObservationsByFileID<T>(
  entries: readonly (readonly [string, string])[],
  results: readonly T[],
): Map<number, T> {
  return new Map(entries.map(([fileID], index) => [Number(fileID), results[index]]))
}
