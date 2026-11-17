package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// session はブラウザとの間の「ステートフル」な部分。ブラウザには推測できない ID（Cookie）だけを渡し、
// トークンを含む中身はすべて Redis に置く。BFF を何台に増やしても、Redis を共有していれば同じセッションを読める。
type session struct {
	Subject      string    `json:"sub"`
	Username     string    `json:"username"`
	Name         string    `json:"name"`
	Roles        []string  `json:"roles"`
	AccessToken  string    `json:"access_token"`
	AccessExpiry time.Time `json:"access_expiry"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	CSRFToken    string    `json:"csrf_token"`
	CreatedAt    time.Time `json:"created_at"`
}

type sessionStore struct {
	rdb      *redis.Client
	idle     time.Duration // 最後のアクセスからこの時間で切れる（使っている間は延びる）
	absolute time.Duration // ログインからこの時間で必ず切れる（延びない）
}

func randomToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Redis のキーはセッション ID そのものではなくハッシュにする。
// Redis の中身が漏れても、キーをそのまま Cookie に入れてなりすますことはできない。
func sessionKey(sid string) string {
	sum := sha256.Sum256([]byte(sid))
	return "sess:" + hex.EncodeToString(sum[:])
}

// ttl はアイドル期限と絶対期限の短いほう。0 以下なら期限切れ。
func (s *sessionStore) ttl(sess *session, now time.Time) time.Duration {
	remain := sess.CreatedAt.Add(s.absolute).Sub(now)
	if remain < s.idle {
		return remain
	}
	return s.idle
}

func (s *sessionStore) create(ctx context.Context, sess *session) (string, error) {
	sid := randomToken(32)
	sess.CreatedAt = time.Now()
	sess.CSRFToken = randomToken(24)
	return sid, s.save(ctx, sid, sess)
}

func (s *sessionStore) save(ctx context.Context, sid string, sess *session) error {
	ttl := s.ttl(sess, time.Now())
	if ttl <= 0 {
		return s.delete(ctx, sid)
	}
	b, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, sessionKey(sid), b, ttl).Err()
}

// get はセッションを読み、アイドル期限を延ばす。無い・期限切れなら nil。
func (s *sessionStore) get(ctx context.Context, sid string) (*session, error) {
	b, err := s.rdb.Get(ctx, sessionKey(sid)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sess session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, err
	}
	ttl := s.ttl(&sess, time.Now())
	if ttl <= 0 {
		_ = s.delete(ctx, sid)
		return nil, nil
	}
	s.rdb.Expire(ctx, sessionKey(sid), ttl)
	return &sess, nil
}

func (s *sessionStore) delete(ctx context.Context, sid string) error {
	return s.rdb.Del(ctx, sessionKey(sid)).Err()
}

// ---- ログイン途中の状態（state・nonce・PKCE の verifier） ------------------------

type loginState struct {
	Verifier string `json:"verifier"`
	Nonce    string `json:"nonce"`
	ReturnTo string `json:"return_to"`
}

const loginStateTTL = 10 * time.Minute

func (s *sessionStore) saveLogin(ctx context.Context, state string, ls loginState) error {
	b, _ := json.Marshal(ls)
	return s.rdb.Set(ctx, "login:"+state, b, loginStateTTL).Err()
}

// takeLogin は読むと同時に消す（GETDEL）。同じ state は二度使えない。
func (s *sessionStore) takeLogin(ctx context.Context, state string) (*loginState, error) {
	b, err := s.rdb.GetDel(ctx, "login:"+state).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ls loginState
	return &ls, json.Unmarshal(b, &ls)
}
