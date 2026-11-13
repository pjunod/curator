package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"github.com/pjunod/monarr/internal/domain"
	"time"
)

func (d *DB) CinemaConnection(ctx context.Context) (domain.CinemaConnection, error) {
	var c domain.CinemaConnection
	err := d.R.QueryRowContext(ctx, `SELECT base_url,issuer,client_id,client_secret,redirect,generation FROM cinema_connection WHERE id=1`).Scan(&c.BaseURL, &c.Issuer, &c.ClientID, &c.ClientSecret, &c.Redirect, &c.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return c, err
}
func enqueueCinemaRevocations(ctx context.Context, tx *sql.Tx, accountID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO cinema_revocations(base_url,client_id,client_secret,grant_secret,created_at) SELECT c.base_url,c.client_id,c.client_secret,s.integration_grant,? FROM household_sessions s JOIN cinema_links l ON l.id=s.link_id JOIN cinema_connection c ON c.id=1 AND c.generation=l.connection_generation WHERE s.method='cinema' AND s.integration_grant<>'' AND (?=0 OR l.account_id=?)`, time.Now().UnixMilli(), accountID, accountID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO cinema_revocations(base_url,client_id,client_secret,grant_secret,created_at) SELECT c.base_url,c.client_id,c.client_secret,h.grant_secret,? FROM cinema_handoffs h JOIN cinema_connection c ON c.id=1 AND c.generation=h.connection_generation WHERE h.grant_secret<>'' AND (?=0 OR h.account_id=? OR EXISTS(SELECT 1 FROM cinema_links l WHERE l.id=h.ready_link AND l.account_id=?))`, time.Now().UnixMilli(), accountID, accountID, accountID)
	return err
}
func (d *DB) SaveCinemaConnection(ctx context.Context, a domain.Account, digest string, c *domain.CinemaConnection) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requireHouseholdActor(ctx, tx, a, digest, true); err != nil {
		return err
	}
	if err = enqueueCinemaRevocations(ctx, tx, 0); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM household_sessions WHERE method='cinema'`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE cinema_links SET active=0,generation=generation+1 WHERE active=1`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cinema_handoffs`); err != nil {
		return err
	}
	// Retain an increasing generation even across disconnect/reconnect.
	if c != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE household_security SET established=1 WHERE id=1`); err != nil {
			return err
		}
	}
	if c == nil {
		_, err = tx.ExecContext(ctx, `UPDATE cinema_connection SET client_secret='',generation=generation+1 WHERE id=1`)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO cinema_connection(id,base_url,issuer,client_id,client_secret,redirect) VALUES(1,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET base_url=excluded.base_url,issuer=excluded.issuer,client_id=excluded.client_id,client_secret=excluded.client_secret,redirect=excluded.redirect,generation=cinema_connection.generation+1`, c.BaseURL, c.Issuer, c.ClientID, c.ClientSecret, c.Redirect)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) CreateCinemaHandoff(ctx context.Context, h domain.CinemaHandoff) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if h.Mode == "link" {
		var valid bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_accounts a JOIN household_sessions s ON s.account_id=a.id WHERE a.id=? AND a.credential_generation=? AND a.status='active' AND s.token_digest=? AND s.method='local' AND s.credential_generation=a.credential_generation AND s.expires_at>?)`, h.AccountID, h.CredentialGeneration, h.SessionDigest, time.Now().UnixMilli()).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrAccountConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cinema_handoffs WHERE state_digest IN(SELECT state_digest FROM cinema_handoffs WHERE expires_at<=? AND grant_secret='' LIMIT 64)`, time.Now().UnixMilli()); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO cinema_handoffs(state_digest,mode,account_id,credential_generation,session_digest,connection_generation,verifier,cookie_digest,native_challenge,expires_at) SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM cinema_connection WHERE id=1 AND generation=? AND client_secret<>'') AND (SELECT count(*) FROM cinema_handoffs)<1024`, h.StateDigest, h.Mode, h.AccountID, h.CredentialGeneration, h.SessionDigest, h.ConnectionGeneration, h.Verifier, h.CookieDigest, h.NativeChallenge, h.ExpiresAt, h.ConnectionGeneration)
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
	return tx.Commit()
}
func (d *DB) CinemaHandoff(ctx context.Context, digest string) (domain.CinemaHandoff, error) {
	var h domain.CinemaHandoff
	err := d.R.QueryRowContext(ctx, `SELECT state_digest,mode,account_id,credential_generation,session_digest,connection_generation,verifier,cookie_digest,native_challenge,claim,ready_link,expires_at,grant_secret FROM cinema_handoffs WHERE state_digest=? AND expires_at>?`, digest, time.Now().UnixMilli()).Scan(&h.StateDigest, &h.Mode, &h.AccountID, &h.CredentialGeneration, &h.SessionDigest, &h.ConnectionGeneration, &h.Verifier, &h.CookieDigest, &h.NativeChallenge, &h.Claim, &h.ReadyLink, &h.ExpiresAt, &h.Grant)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return h, err
}
func (d *DB) OpenCinemaBrowser(ctx context.Context, digest, cookie string) error {
	result, err := d.W.ExecContext(ctx, `UPDATE cinema_handoffs SET browser_opened=1,cookie_digest=? WHERE state_digest=? AND mode='native' AND browser_opened=0 AND claim='' AND expires_at>?`, cookie, digest, time.Now().UnixMilli())
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
	return nil
}
func (d *DB) ClaimCinemaCallback(ctx context.Context, h domain.CinemaHandoff, claim string) error {
	result, err := d.W.ExecContext(ctx, `UPDATE cinema_handoffs SET claim=? WHERE state_digest=? AND claim='' AND cookie_digest=? AND expires_at>? AND EXISTS(SELECT 1 FROM cinema_connection c WHERE c.id=1 AND c.generation=cinema_handoffs.connection_generation AND c.client_secret<>'')`, claim, h.StateDigest, h.CookieDigest, time.Now().UnixMilli())
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
	return nil
}

const cinemaLinkColumns = `id,account_id,issuer,subject,grant_secret,connection_generation,generation`

func scanCinemaLink(r accountScanner) (domain.CinemaLink, error) {
	var l domain.CinemaLink
	err := r.Scan(&l.ID, &l.AccountID, &l.Issuer, &l.Subject, &l.Grant, &l.ConnectionGeneration, &l.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return l, err
}
func (d *DB) CinemaLinkForAccount(ctx context.Context, id int64) (domain.CinemaLink, error) {
	return scanCinemaLink(d.R.QueryRowContext(ctx, `SELECT `+cinemaLinkColumns+` FROM cinema_links WHERE account_id=? AND active=1`, id))
}
func (d *DB) CinemaLinkForSubject(ctx context.Context, issuer, subject string) (domain.CinemaLink, error) {
	return scanCinemaLink(d.R.QueryRowContext(ctx, `SELECT `+cinemaLinkColumns+` FROM cinema_links WHERE issuer=? AND subject=? AND active=1`, issuer, subject))
}
func (d *DB) CinemaLinkForSession(ctx context.Context, id, generation int64) (domain.CinemaLink, error) {
	return scanCinemaLink(d.R.QueryRowContext(ctx, `SELECT `+cinemaLinkColumns+` FROM cinema_links WHERE id=? AND generation=? AND active=1`, id, generation))
}
func (d *DB) FinishCinemaLink(ctx context.Context, h domain.CinemaHandoff, l domain.CinemaLink) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cinema_handoffs h JOIN household_accounts a ON a.id=h.account_id JOIN household_sessions s ON s.account_id=a.id JOIN cinema_connection c ON c.id=1 WHERE h.state_digest=? AND h.claim=? AND h.claim<>'' AND h.mode='link' AND h.account_id=? AND h.credential_generation=a.credential_generation AND h.expires_at>? AND h.connection_generation=c.generation AND c.issuer=? AND a.status='active' AND s.token_digest=h.session_digest AND s.method='local' AND s.credential_generation=a.credential_generation AND s.expires_at>?)`, h.StateDigest, h.Claim, h.AccountID, time.Now().UnixMilli(), l.Issuer, time.Now().UnixMilli()).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrAccountConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO cinema_links(account_id,issuer,subject,grant_secret,connection_generation) VALUES(?,?,?,?,?)`, h.AccountID, l.Issuer, l.Subject, "", h.ConnectionGeneration); err != nil {
		return ErrAccountConflict
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cinema_handoffs WHERE state_digest=?`, h.StateDigest); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) UnlinkCinema(ctx context.Context, a domain.Account, digest string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requireHouseholdActor(ctx, tx, a, digest, false); err != nil {
		return err
	}
	// requireHouseholdActor permits integrated members for requests. Unlink needs
	// the original local method and a password proof from the HTTP boundary.
	var local bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_sessions WHERE token_digest=? AND method='local')`, digest).Scan(&local); err != nil {
		return err
	}
	if !local {
		return ErrAccountConflict
	}
	if err = enqueueCinemaRevocations(ctx, tx, a.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM household_sessions WHERE account_id=? AND method='cinema'`, a.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE cinema_links SET active=0,generation=generation+1 WHERE account_id=? AND active=1`, a.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cinema_handoffs WHERE account_id=? OR ready_link IN(SELECT id FROM cinema_links WHERE account_id=?)`, a.ID, a.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) ReadyCinemaNative(ctx context.Context, h domain.CinemaHandoff, l domain.CinemaLink) error {
	result, err := d.W.ExecContext(ctx, `UPDATE cinema_handoffs SET ready_link=?,grant_secret=?,expires_at=? WHERE state_digest=? AND claim=? AND mode='native' AND ready_link=0 AND expires_at>? AND EXISTS(SELECT 1 FROM cinema_links l JOIN cinema_connection c ON c.id=1 WHERE l.id=? AND l.active=1 AND l.generation=? AND l.connection_generation=c.generation AND c.generation=cinema_handoffs.connection_generation)`, l.ID, l.Grant, time.Now().Add(time.Minute).UnixMilli(), h.StateDigest, h.Claim, time.Now().UnixMilli(), l.ID, l.Generation)
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
	return nil
}
func (d *DB) CreateCinemaSession(ctx context.Context, h domain.CinemaHandoff, l domain.CinemaLink, digest, device string, expires time.Time) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO household_sessions(token_digest,account_id,credential_generation,method,link_id,link_generation,device,created_at,expires_at,integration_grant) SELECT ?,a.id,a.credential_generation,'cinema',l.id,l.generation,?,?,?,? FROM cinema_links l JOIN household_accounts a ON a.id=l.account_id JOIN cinema_connection c ON c.id=1 JOIN cinema_handoffs h ON h.state_digest=? WHERE l.id=? AND l.generation=? AND l.active=1 AND l.connection_generation=c.generation AND c.generation=h.connection_generation AND a.status='active' AND h.claim=? AND h.claim<>'' AND h.expires_at>? AND (h.mode='signin' OR (h.mode='native' AND h.ready_link=l.id))`, digest, device, time.Now().UnixMilli(), expires.UnixMilli(), l.Grant, h.StateDigest, l.ID, l.Generation, h.Claim, time.Now().UnixMilli())
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM cinema_handoffs WHERE state_digest=?`, h.StateDigest); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) QueueCinemaRevocation(ctx context.Context, c domain.CinemaConnection, grant string) error {
	_, err := d.W.ExecContext(ctx, `INSERT INTO cinema_revocations(base_url,client_id,client_secret,grant_secret,created_at) VALUES(?,?,?,?,?)`, c.BaseURL, c.ClientID, c.ClientSecret, grant, time.Now().UnixMilli())
	return err
}
func (d *DB) CinemaRevocations(ctx context.Context) ([]domain.CinemaRevocation, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id,base_url,client_id,client_secret,grant_secret FROM cinema_revocations ORDER BY id LIMIT 32`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.CinemaRevocation{}
	for rows.Next() {
		var r domain.CinemaRevocation
		if err = rows.Scan(&r.ID, &r.BaseURL, &r.ClientID, &r.ClientSecret, &r.Grant); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (d *DB) FinishCinemaRevocation(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, `DELETE FROM cinema_revocations WHERE id=?`, id)
	return err
}

func (d *DB) QueueDeadCinemaSessions(ctx context.Context) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const dead = `s.method='cinema' AND (s.expires_at<=? OR a.status<>'active' OR s.credential_generation<>a.credential_generation OR l.active=0 OR s.link_generation<>l.generation)`
	clock := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO cinema_revocations(base_url,client_id,client_secret,grant_secret,created_at) SELECT c.base_url,c.client_id,c.client_secret,s.integration_grant,? FROM household_sessions s JOIN household_accounts a ON a.id=s.account_id JOIN cinema_links l ON l.id=s.link_id JOIN cinema_connection c ON c.id=1 AND c.generation=l.connection_generation WHERE `+dead, clock, clock); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM household_sessions WHERE token_digest IN(SELECT s.token_digest FROM household_sessions s JOIN household_accounts a ON a.id=s.account_id JOIN cinema_links l ON l.id=s.link_id WHERE `+dead+`)`, clock); err != nil {
		return err
	}
	// An abandoned native handoff still owns a remote grant until revocation is
	// durably queued. It must never disappear in a generic expiry sweep.
	if _, err = tx.ExecContext(ctx, `INSERT INTO cinema_revocations(base_url,client_id,client_secret,grant_secret,created_at) SELECT c.base_url,c.client_id,c.client_secret,h.grant_secret,? FROM cinema_handoffs h JOIN cinema_connection c ON c.id=1 AND c.generation=h.connection_generation WHERE h.expires_at<=? AND h.grant_secret<>''`, clock, clock); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cinema_handoffs WHERE expires_at<=?`, clock); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) CinemaLinkByID(ctx context.Context, id int64) (domain.CinemaLink, error) {
	return scanCinemaLink(d.R.QueryRowContext(ctx, `SELECT `+cinemaLinkColumns+` FROM cinema_links WHERE id=? AND active=1`, id))
}
