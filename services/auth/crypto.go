package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/argon2"
)

// ---- ランダム値とハッシュ ---------------------------------------------------

// randomToken は推測できない値（認可コード・リフレッシュトークン・SSO セッション ID）を作る。
func randomToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken は DB に保存する前のハッシュ。DB が漏れても、そのまま使えるトークンは出てこない。
// トークン自体が十分長いランダム値なので、パスワードと違って遅いハッシュは要らない。
func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func equalConstantTime(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---- PKCE（RFC 7636） --------------------------------------------------------

// verifyPKCE: クライアントが最初に送った code_challenge = BASE64URL(SHA256(code_verifier)) かを確かめる。
// 認可コードを横取りされても、code_verifier を知らない攻撃者はトークンに交換できない。
func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return equalConstantTime(base64.RawURLEncoding.EncodeToString(sum[:]), challenge)
}

// ---- パスワード（argon2id） --------------------------------------------------

// OWASP の推奨（m=19MiB, t=2, p=1）に合わせた設定。保存形式は PHC 文字列。
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
)

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func verifyPassword(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unknown hash format")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash は「存在しないユーザー」でも同じだけ時間をかけるためのもの。
// 応答時間の差で、ユーザー名が存在するかを当てられないようにする。
var dummyHash = hashPassword("dummy-password-for-timing")

// ---- 署名鍵と JWKS ----------------------------------------------------------

// signer はトークンに署名する秘密鍵を持つ。公開鍵は /jwks で配り、API はそれで検証する。
// 鍵は起動のたびに作り直す（メモリにしか置かない）。再起動すると古いトークンは検証できなくなり、
// BFF がリフレッシュして新しい鍵のトークンを取り直す — 鍵のローテーションを体感できる。
type signer struct {
	key *ecdsa.PrivateKey
	kid string
}

func newSigner() *signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return &signer{key: k, kid: randomToken(9)}
}

func (s *signer) sign(typ string, claims jwt.MapClaims) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	t.Header["kid"] = s.kid
	t.Header["typ"] = typ
	return t.SignedString(s.key)
}

func (s *signer) jwks() map[string]any {
	pub, err := s.key.PublicKey.ECDH()
	if err != nil {
		panic(err)
	}
	b := pub.Bytes() // 0x04 || X(32) || Y(32)
	return map[string]any{"keys": []map[string]string{{
		"kty": "EC",
		"crv": "P-256",
		"use": "sig",
		"alg": "ES256",
		"kid": s.kid,
		"x":   base64.RawURLEncoding.EncodeToString(b[1:33]),
		"y":   base64.RawURLEncoding.EncodeToString(b[33:65]),
	}}}
}
