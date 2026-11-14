package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

var ErrAccountConflict = errors.New("account or invitation is unavailable")
var ErrLastOwner = errors.New("keep at least one active local owner")

const accountColumns = `id,username,display_name,password_hash,legacy_salt,role,status,credential_generation,created_at,revision`

type accountScanner interface{ Scan(...any) error }

func scanAccount(row accountScanner) (domain.Account, error) {
	var a domain.Account
	var created int64
	err := row.Scan(&a.ID, &a.Username, &a.DisplayName, &a.PasswordHash, &a.LegacySalt, &a.Role, &a.Status, &a.CredentialGeneration, &created, &a.Revision)
	a.CreatedAt = time.UnixMilli(created)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	return a, err
}

// EnsureInitialHouseholdOwner imports configured standalone credentials once.
// It never resets an existing account or merges two users by name.
func (d *DB) EnsureInitialHouseholdOwner(ctx context.Context) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM household_accounts`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	var name, hash, salt string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM app_meta WHERE key='auth_username'),''),COALESCE((SELECT value FROM app_meta WHERE key='auth_password_hash'),''),COALESCE((SELECT value FROM app_meta WHERE key='auth_password_salt'),'')`).Scan(&name, &hash, &salt); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" || hash == "" {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO household_accounts(username,display_name,password_hash,legacy_salt,role,created_at,initial_owner) VALUES(?,?,?,?,'owner',?,1)`, strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(name), hash, salt, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) HouseholdEstablished(ctx context.Context) (bool, error) {
	var v bool
	err := d.R.QueryRowContext(ctx, `SELECT established FROM household_security WHERE id=1`).Scan(&v)
	return v, err
}
func (d *DB) AccountByUsername(ctx context.Context, name string) (domain.Account, error) {
	return scanAccount(d.R.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM household_accounts WHERE username=?`, strings.ToLower(strings.TrimSpace(name))))
}
func (d *DB) AccountByID(ctx context.Context, id int64) (domain.Account, error) {
	return scanAccount(d.R.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM household_accounts WHERE id=?`, id))
}
func (d *DB) ListAccounts(ctx context.Context) ([]domain.Account, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT `+accountColumns+` FROM household_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (d *DB) CreateAccountSession(ctx context.Context, digest string, a domain.Account, device string, expires time.Time) error {
	res, err := d.W.ExecContext(ctx, `INSERT INTO household_sessions(token_digest,account_id,credential_generation,method,device,created_at,expires_at) SELECT ?,id,credential_generation,'local',?,?,? FROM household_accounts WHERE id=? AND status='active' AND credential_generation=? AND password_hash=?`, digest, device, time.Now().UnixMilli(), expires.UnixMilli(), a.ID, a.CredentialGeneration, a.PasswordHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAccountConflict
	}
	return nil
}
func (d *DB) AccountSession(ctx context.Context, digest string) (domain.AccountSession, error) {
	var session domain.AccountSession
	var created, sessionCreated, expires int64
	// Every request reads current account status and credential generation. Cinema
	// links add their own authoritative check after this local boundary.
	err := d.R.QueryRowContext(ctx, `SELECT a.id,a.username,a.display_name,a.role,a.status,a.created_at,s.method,s.link_id,s.link_generation,s.created_at,s.expires_at,a.credential_generation,a.revision,s.integration_grant FROM household_sessions s JOIN household_accounts a ON a.id=s.account_id WHERE s.token_digest=? AND s.credential_generation=a.credential_generation AND a.status='active' AND s.expires_at>?`, digest, time.Now().UnixMilli()).Scan(&session.Account.ID, &session.Account.Username, &session.Account.DisplayName, &session.Account.Role, &session.Account.Status, &created, &session.Method, &session.LinkID, &session.LinkGeneration, &sessionCreated, &expires, &session.Account.CredentialGeneration, &session.Account.Revision, &session.IntegrationGrant)
	// Scan timestamps separately; never rely on driver time conversions.
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return session, ErrNotFound
		}
		return session, err
	}
	session.Account.CreatedAt = time.UnixMilli(created)
	session.CreatedAt = time.UnixMilli(sessionCreated)
	session.ExpiresAt = time.UnixMilli(expires)
	return session, nil
}
func (d *DB) DropAccountSession(ctx context.Context, digest string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO cinema_revocations(base_url,client_id,client_secret,grant_secret,created_at,expires_at) SELECT c.base_url,c.client_id,c.client_secret,s.integration_grant,?,s.integration_grant_expires FROM household_sessions s JOIN cinema_links l ON l.id=s.link_id JOIN cinema_connection c ON c.id=1 AND c.generation=l.connection_generation WHERE s.token_digest=? AND s.method='cinema' AND s.integration_grant<>''`, time.Now().UnixMilli(), digest); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM household_sessions WHERE token_digest=?`, digest); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) UpgradeAccountPassword(ctx context.Context, a domain.Account, hash string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE household_accounts SET password_hash=?,legacy_salt='' WHERE id=? AND credential_generation=? AND password_hash=?`, hash, a.ID, a.CredentialGeneration, a.PasswordHash)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAccountConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE app_meta SET value=?,updated_at=? WHERE key='auth_password_hash' AND EXISTS(SELECT 1 FROM household_accounts WHERE id=? AND initial_owner=1)`, hash, time.Now().UnixMilli(), a.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM app_meta WHERE key='auth_password_salt' AND EXISTS(SELECT 1 FROM household_accounts WHERE id=? AND initial_owner=1)`, a.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) CreateHouseholdInvite(ctx context.Context, actor domain.Account, sessionDigest, digest string, expires time.Time) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var owner bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_accounts WHERE id=? AND credential_generation=? AND role='owner' AND status='active' AND password_hash<>'' AND EXISTS(SELECT 1 FROM household_sessions s WHERE s.account_id=household_accounts.id AND s.token_digest=? AND s.method='local' AND s.credential_generation=household_accounts.credential_generation AND s.expires_at>?))`, actor.ID, actor.CredentialGeneration, sessionDigest, time.Now().UnixMilli()).Scan(&owner); err != nil {
		return err
	}
	if !owner {
		return ErrAccountConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_security SET established=1 WHERE id=1`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO household_invites(token_digest,created_by,created_generation,created_at,expires_at) VALUES(?,?,?,?,?)`, digest, actor.ID, actor.CredentialGeneration, time.Now().UnixMilli(), expires.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) AcceptHouseholdInvite(ctx context.Context, digest, username, displayName, hash string) (domain.Account, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return domain.Account{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var valid bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_invites i JOIN household_accounts a ON a.id=i.created_by WHERE i.token_digest=? AND i.consumed_by IS NULL AND i.expires_at>? AND a.status='active' AND a.role='owner' AND i.created_generation=a.credential_generation)`, digest, time.Now().UnixMilli()).Scan(&valid); err != nil {
		return domain.Account{}, err
	}
	if !valid {
		return domain.Account{}, ErrAccountConflict
	}
	a, err := scanAccount(tx.QueryRowContext(ctx, `INSERT INTO household_accounts(username,display_name,password_hash,role,created_at) VALUES(?,?,?,'member',?) RETURNING `+accountColumns, username, displayName, hash, time.Now().UnixMilli()))
	if err != nil {
		if isConstraint(err) {
			return a, ErrAccountConflict
		}
		return a, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_invites SET consumed_by=? WHERE token_digest=? AND consumed_by IS NULL`, a.ID, digest); err != nil {
		return a, err
	}
	if err = tx.Commit(); err != nil {
		return a, err
	}
	return a, nil
}

// UpdateAccount compares the acting owner's current credential generation and
// enforces the last-owner invariant in the same write transaction.
func (d *DB) UpdateAccount(ctx context.Context, actor domain.Account, sessionDigest string, id, targetRevision int64, role, status, displayName, hash string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var allowed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_accounts WHERE id=? AND status='active' AND credential_generation=? AND (role='owner' OR id=?) AND EXISTS(SELECT 1 FROM household_sessions s WHERE s.account_id=household_accounts.id AND s.token_digest=? AND s.method='local' AND s.credential_generation=household_accounts.credential_generation AND s.expires_at>?))`, actor.ID, actor.CredentialGeneration, id, sessionDigest, time.Now().UnixMilli()).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrAccountConflict
	}
	current, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM household_accounts WHERE id=?`, id))
	if err != nil {
		return err
	}
	if current.Revision != targetRevision {
		return ErrAccountConflict
	}
	if actor.Role != "owner" && (role != current.Role || status != current.Status) {
		return ErrAccountConflict
	}
	if current.Role == "owner" && current.Status == "active" && (role != "owner" || status != "active") {
		var others int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM household_accounts WHERE role='owner' AND status='active' AND id<>?`, id).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return ErrLastOwner
		}
	}
	if (role != "owner" && role != "member") || (status != "active" && status != "suspended") {
		return fmt.Errorf("invalid account role or status")
	}
	if hash == "" {
		hash = current.PasswordHash
	}
	changed := hash != current.PasswordHash || role != current.Role || status != current.Status
	_, err = tx.ExecContext(ctx, `UPDATE household_accounts SET role=?,status=?,display_name=?,password_hash=?,legacy_salt=CASE WHEN password_hash<>? THEN '' ELSE legacy_salt END,credential_generation=credential_generation+?,revision=revision+1 WHERE id=?`, role, status, displayName, hash, hash, boolInt(changed), id)
	if err != nil {
		return err
	}
	if hash != current.PasswordHash {
		if _, err = tx.ExecContext(ctx, `UPDATE app_meta SET value=?,updated_at=? WHERE key='auth_password_hash' AND EXISTS(SELECT 1 FROM household_accounts WHERE id=? AND initial_owner=1)`, hash, time.Now().UnixMilli(), id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM app_meta WHERE key='auth_password_salt' AND EXISTS(SELECT 1 FROM household_accounts WHERE id=? AND initial_owner=1)`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConfigureLocalOwner keeps the original standalone settings interface usable
// while atomically updating its permanent account and credential generation.
func (d *DB) ConfigureLocalOwner(ctx context.Context, username, hash *string, required *bool) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var name, password, salt string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM app_meta WHERE key='auth_username'),''),COALESCE((SELECT value FROM app_meta WHERE key='auth_password_hash'),''),COALESCE((SELECT value FROM app_meta WHERE key='auth_password_salt'),'')`).Scan(&name, &password, &salt); err != nil {
		return err
	}
	if username != nil && strings.TrimSpace(*username) != "" {
		name = strings.ToLower(strings.TrimSpace(*username))
	}
	if hash != nil && *hash != "" {
		password = *hash
		salt = ""
	}
	var established bool
	if err = tx.QueryRowContext(ctx, `SELECT established FROM household_security WHERE id=1`).Scan(&established); err != nil {
		return err
	}
	if established && (username != nil || hash != nil) {
		return fmt.Errorf("manage household credentials from Accounts after local password confirmation")
	}
	if required != nil && !*required && established {
		return fmt.Errorf("household accounts require authenticated access")
	}
	if required != nil && *required && (name == "" || password == "") {
		return fmt.Errorf("set a username and password before enabling authentication")
	}
	now := time.Now().UnixMilli()
	save := func(key, value string) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO app_meta(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, now)
		return e
	}
	if username != nil && *username != "" {
		if err = save("auth_username", name); err != nil {
			return err
		}
	}
	if hash != nil && *hash != "" {
		if err = save("auth_password_hash", password); err != nil {
			return err
		}
	}
	if required != nil {
		value := "false"
		if *required {
			value = "true"
		}
		if err = save("auth_required", value); err != nil {
			return err
		}
	}
	owner, e := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM household_accounts WHERE initial_owner=1`))
	if errors.Is(e, ErrNotFound) {
		if name != "" && password != "" {
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM household_accounts`).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				_, err = tx.ExecContext(ctx, `INSERT INTO household_accounts(username,display_name,password_hash,legacy_salt,role,created_at,initial_owner) VALUES(?,?,?,?,'owner',?,1)`, name, name, password, salt, now)
				if err != nil {
					return err
				}
			}
		}
	} else if e != nil {
		return e
	} else {
		nextName, nextHash, nextSalt := owner.Username, owner.PasswordHash, owner.LegacySalt
		if username != nil && *username != "" {
			nextName = name
		}
		if hash != nil && *hash != "" {
			nextHash = password
			nextSalt = ""
		}
		changed := nextName != owner.Username || nextHash != owner.PasswordHash
		if _, err = tx.ExecContext(ctx, `UPDATE household_accounts SET username=?,password_hash=?,legacy_salt=?,credential_generation=credential_generation+? WHERE id=?`, nextName, nextHash, nextSalt, boolInt(changed), owner.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Opportunistic cleanup is bounded so sign-in cannot become an unbounded sweep.
func (d *DB) PruneExpiredHouseholdCredentials(ctx context.Context) error {
	now := time.Now().UnixMilli()
	if err := d.QueueDeadCinemaSessions(ctx); err != nil {
		return err
	}
	if _, err := d.W.ExecContext(ctx, `DELETE FROM household_sessions WHERE token_digest IN(SELECT token_digest FROM household_sessions WHERE expires_at<=? ORDER BY expires_at LIMIT 64)`, now); err != nil {
		return err
	}
	_, err := d.W.ExecContext(ctx, `DELETE FROM household_invites WHERE token_digest IN(SELECT token_digest FROM household_invites WHERE expires_at<=? ORDER BY expires_at LIMIT 64)`, now)
	return err
}

// ResetLocalOwnerPassword is an operator-only database operation; it is never
// exposed over HTTP. Filesystem access is the recovery authority, and the
// permanent account and request ownership survive the reset.
func (d *DB) ResetLocalOwnerPassword(ctx context.Context, username, hash string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM household_accounts WHERE username=? AND role='owner' AND status='active'`, strings.ToLower(strings.TrimSpace(username))))
	if err != nil {
		return ErrAccountConflict
	}
	if hash == "" {
		return ErrAccountConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_accounts SET password_hash=?,legacy_salt='',credential_generation=credential_generation+1,revision=revision+1 WHERE id=?`, hash, a.ID); err != nil {
		return err
	}
	if err = enqueueCinemaRevocations(ctx, tx, a.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cinema_handoffs WHERE account_id=? OR ready_link IN(SELECT id FROM cinema_links WHERE account_id=?)`, a.ID, a.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM household_sessions WHERE account_id=?`, a.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE app_meta SET value=?,updated_at=? WHERE key='auth_password_hash' AND EXISTS(SELECT 1 FROM household_accounts WHERE id=? AND initial_owner=1)`, hash, time.Now().UnixMilli(), a.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM app_meta WHERE key='auth_password_salt' AND EXISTS(SELECT 1 FROM household_accounts WHERE id=? AND initial_owner=1)`, a.ID); err != nil {
		return err
	}
	return tx.Commit()
}
