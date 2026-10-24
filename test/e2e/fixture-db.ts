import { join } from 'node:path'
import { DatabaseSync } from 'node:sqlite'

// Fixture SQL runs beside the real server, whose writer may hold SQLite's
// single-writer lock while a test advances a due time. Match the application's
// five-second SQLite busy timeout instead of failing on transient contention.
export function openFixtureDatabase() {
  const dataDir = process.env.E2E_DATA_DIR
  if (!dataDir) throw new Error('E2E_DATA_DIR is required to open the fixture database')

  const db = new DatabaseSync(join(dataDir, 'monarr.db'))
  db.exec('PRAGMA busy_timeout = 5000')
  return db
}
