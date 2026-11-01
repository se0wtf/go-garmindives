package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

var authMigration = migration{version: 11, run: func(ctx context.Context, s *sqlStore, tx *sql.Tx) error {
	dataType := "BLOB"
	if s.dialect == outputPostgres {
		dataType = "BYTEA"
	}
	for _, statement := range []string{
		`CREATE TABLE admin_credentials(id INTEGER PRIMARY KEY CHECK(id=1),instance_id TEXT NOT NULL,password_hash TEXT NOT NULL,auth_version BIGINT NOT NULL DEFAULT 1,initialized_at TEXT NOT NULL)`,
		`CREATE TABLE admin_sessions(token_hash TEXT PRIMARY KEY,data ` + dataType + ` NOT NULL,expires_at BIGINT NOT NULL)`,
		`CREATE INDEX admin_sessions_expiry_idx ON admin_sessions(expires_at)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}}

type adminCredentials struct {
	instanceID, passwordHash string
	version                  int64
}

func (s *sqlStore) adminCredentials(ctx context.Context) (adminCredentials, error) {
	var c adminCredentials
	err := s.db.QueryRowContext(ctx, `SELECT instance_id,password_hash,auth_version FROM admin_credentials WHERE id=1`).Scan(&c.instanceID, &c.passwordHash, &c.version)
	return c, err
}
func (s *sqlStore) checkInitialization(ctx context.Context, instance string) error {
	c, err := s.adminCredentials(ctx)
	if err != nil || c.instanceID != instance {
		return errors.New("database is not initialized for this configuration")
	}
	return nil
}

// SCS owns session token generation and cookie handling. Only a SHA-256 digest
// of each high-entropy token is persisted, using the existing SQL driver.
type sqlSessionStore struct{ s *sqlStore }

func sessionDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func (s *sqlSessionStore) Delete(token string) error { return s.DeleteCtx(context.Background(), token) }
func (s *sqlSessionStore) Find(token string) ([]byte, bool, error) {
	return s.FindCtx(context.Background(), token)
}
func (s *sqlSessionStore) Commit(token string, data []byte, expires time.Time) error {
	return s.CommitCtx(context.Background(), token, data, expires)
}
func (s *sqlSessionStore) DeleteCtx(ctx context.Context, token string) error {
	_, err := s.s.db.ExecContext(ctx, s.s.bind(`DELETE FROM admin_sessions WHERE token_hash=?`), sessionDigest(token))
	return err
}
func (s *sqlSessionStore) FindCtx(ctx context.Context, token string) ([]byte, bool, error) {
	if len(token) > 128 {
		return nil, false, nil
	}
	var data []byte
	err := s.s.db.QueryRowContext(ctx, s.s.bind(`SELECT data FROM admin_sessions WHERE token_hash=? AND expires_at>?`), sessionDigest(token), time.Now().UnixMilli()).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return data, err == nil, err
}
func (s *sqlSessionStore) CommitCtx(ctx context.Context, token string, data []byte, expires time.Time) error {
	_, err := s.s.db.ExecContext(ctx, s.s.bind(`INSERT INTO admin_sessions(token_hash,data,expires_at) VALUES(?,?,?) ON CONFLICT(token_hash) DO UPDATE SET data=excluded.data,expires_at=excluded.expires_at`), sessionDigest(token), data, expires.UnixMilli())
	return err
}
func (s *sqlStore) setAdminPassword(ctx context.Context, expectedVersion int64, encoded string) error {
	tx, err := s.beginTravelTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, s.bind(`UPDATE admin_credentials SET password_hash=?,auth_version=auth_version+1 WHERE id=1 AND auth_version=?`), encoded, expectedVersion)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return errors.New("administrator credentials changed; authenticate again")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions`); err != nil {
		return err
	}
	return tx.Commit()
}
