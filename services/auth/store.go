package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// store は認証基盤が持つ状態（ユーザー・認可コード・リフレッシュトークン・SSO セッション）。
// トークン類はハッシュだけを保存する。
type store struct{ db *pgxpool.Pool }

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,
  username      TEXT UNIQUE NOT NULL,
  name          TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  roles         TEXT[] NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS auth_codes (
  code_hash      TEXT PRIMARY KEY,
  client_id      TEXT NOT NULL,
  user_id        TEXT NOT NULL REFERENCES users(id),
  redirect_uri   TEXT NOT NULL,
  scope          TEXT NOT NULL,
  nonce          TEXT NOT NULL,
  code_challenge TEXT NOT NULL,
  auth_time      TIMESTAMPTZ NOT NULL,
  expires_at     TIMESTAMPTZ NOT NULL,
  used           BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS refresh_tokens (
  token_hash TEXT PRIMARY KEY,
  family_id  TEXT NOT NULL,
  client_id  TEXT NOT NULL,
  user_id    TEXT NOT NULL REFERENCES users(id),
  scope      TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  rotated    BOOLEAN NOT NULL DEFAULT false,
  revoked    BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS refresh_tokens_family ON refresh_tokens(family_id);
CREATE TABLE IF NOT EXISTS sso_sessions (
  id_hash    TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id),
  auth_time  TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL
);
`

func openStore(ctx context.Context, url string) (*store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	// DB の起動を待つ（compose の depends_on は healthy を待つが、念のため）
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("db not ready: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &store{db: pool}, nil
}

type user struct {
	ID, Username, Name, PasswordHash string
	Roles                            []string
}

func (s *store) seedUser(ctx context.Context, username, name, password string, roles []string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO users (id, username, name, password_hash, roles) VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (username) DO UPDATE
		 SET name = EXCLUDED.name, roles = EXCLUDED.roles, password_hash = EXCLUDED.password_hash`,
		"u_"+randomToken(12), username, name, hashPassword(password), roles)
	return err
}

func (s *store) userBy(ctx context.Context, column, value string) (*user, error) {
	var u user
	err := s.db.QueryRow(ctx,
		`SELECT id, username, name, password_hash, roles FROM users WHERE `+column+` = $1`, value).
		Scan(&u.ID, &u.Username, &u.Name, &u.PasswordHash, &u.Roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

func (s *store) userByUsername(ctx context.Context, username string) (*user, error) {
	return s.userBy(ctx, "username", username)
}

func (s *store) userByID(ctx context.Context, id string) (*user, error) {
	return s.userBy(ctx, "id", id)
}

// ---- 認可コード -------------------------------------------------------------

type authCode struct {
	ClientID, UserID, RedirectURI, Scope, Nonce, CodeChallenge string
	AuthTime, ExpiresAt                                        time.Time
}

func (s *store) saveCode(ctx context.Context, code string, c authCode) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO auth_codes (code_hash, client_id, user_id, redirect_uri, scope, nonce, code_challenge, auth_time, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		hashToken(code), c.ClientID, c.UserID, c.RedirectURI, c.Scope, c.Nonce, c.CodeChallenge, c.AuthTime, c.ExpiresAt)
	return err
}

// consumeCode は認可コードを「使用済み」にしながら読む。UPDATE ... WHERE used = false で
// 同時に2回交換されても、成功するのは1回だけになる。
func (s *store) consumeCode(ctx context.Context, code string) (*authCode, error) {
	var c authCode
	err := s.db.QueryRow(ctx,
		`UPDATE auth_codes SET used = true
		 WHERE code_hash = $1 AND used = false AND expires_at > now()
		 RETURNING client_id, user_id, redirect_uri, scope, nonce, code_challenge, auth_time, expires_at`,
		hashToken(code)).
		Scan(&c.ClientID, &c.UserID, &c.RedirectURI, &c.Scope, &c.Nonce, &c.CodeChallenge, &c.AuthTime, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// ---- リフレッシュトークン（ローテーション＋再利用検知） ---------------------------

type refreshToken struct {
	FamilyID, ClientID, UserID, Scope string
	ExpiresAt                         time.Time
}

func (s *store) saveRefresh(ctx context.Context, token string, rt refreshToken) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO refresh_tokens (token_hash, family_id, client_id, user_id, scope, expires_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		hashToken(token), rt.FamilyID, rt.ClientID, rt.UserID, rt.Scope, rt.ExpiresAt)
	return err
}

var errRefreshReused = errors.New("refresh token reused")

// rotateRefresh はリフレッシュトークンを一度だけ使える形で消費する。
// 既に使われた（rotated）トークンがもう一度来たら、盗まれて二者が使っている疑いがあるので、
// 同じ系統（family）を丸ごと失効させる（RFC 9700 の refresh token rotation）。
func (s *store) rotateRefresh(ctx context.Context, token string) (*refreshToken, error) {
	h := hashToken(token)
	var rt refreshToken
	err := s.db.QueryRow(ctx,
		`UPDATE refresh_tokens SET rotated = true
		 WHERE token_hash = $1 AND rotated = false AND revoked = false AND expires_at > now()
		 RETURNING family_id, client_id, user_id, scope, expires_at`, h).
		Scan(&rt.FamilyID, &rt.ClientID, &rt.UserID, &rt.Scope, &rt.ExpiresAt)
	if err == nil {
		return &rt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var family string
	var rotated bool
	err = s.db.QueryRow(ctx, `SELECT family_id, rotated FROM refresh_tokens WHERE token_hash = $1`, h).Scan(&family, &rotated)
	if err == nil && rotated {
		_ = s.revokeFamily(ctx, family)
		return nil, errRefreshReused
	}
	return nil, nil
}

func (s *store) revokeFamily(ctx context.Context, family string) error {
	_, err := s.db.Exec(ctx, `UPDATE refresh_tokens SET revoked = true WHERE family_id = $1`, family)
	return err
}

// revokeRefresh はログアウト時の失効（RFC 7009）。そのトークンの系統をすべて止める。
func (s *store) revokeRefresh(ctx context.Context, token, clientID string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE refresh_tokens SET revoked = true
		 WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1 AND client_id = $2)`,
		hashToken(token), clientID)
	return err
}

// ---- SSO セッション（認証基盤自身のログイン状態） -----------------------------------

func (s *store) createSSO(ctx context.Context, id, userID string, authTime, exp time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO sso_sessions (id_hash, user_id, auth_time, expires_at) VALUES ($1,$2,$3,$4)`,
		hashToken(id), userID, authTime, exp)
	return err
}

func (s *store) ssoSession(ctx context.Context, id string) (userID string, authTime time.Time, ok bool) {
	err := s.db.QueryRow(ctx, `SELECT user_id, auth_time FROM sso_sessions WHERE id_hash = $1 AND expires_at > now()`,
		hashToken(id)).Scan(&userID, &authTime)
	return userID, authTime, err == nil
}

func (s *store) deleteSSO(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sso_sessions WHERE id_hash = $1`, hashToken(id))
	return err
}
