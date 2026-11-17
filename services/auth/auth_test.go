package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"testing"
)

func TestPassword(t *testing.T) {
	h := hashPassword("correct horse")
	if ok, err := verifyPassword("correct horse", h); err != nil || !ok {
		t.Fatalf("正しいパスワードで失敗: ok=%v err=%v", ok, err)
	}
	if ok, _ := verifyPassword("wrong", h); ok {
		t.Fatal("誤ったパスワードで成功してしまう")
	}
	if h == hashPassword("correct horse") {
		t.Fatal("同じパスワードでもソルトが違えばハッシュは変わるはず")
	}
}

func TestVerifyPKCE(t *testing.T) {
	verifier := randomToken(32) // 43文字
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	if !verifyPKCE(verifier, challenge) {
		t.Fatal("正しい verifier で失敗")
	}
	if verifyPKCE(randomToken(32), challenge) {
		t.Fatal("別の verifier で成功してしまう")
	}
	if verifyPKCE("short", challenge) {
		t.Fatal("43文字未満の verifier を受け付けてしまう")
	}
}

func TestValidateAuthz(t *testing.T) {
	a := &app{cfg: config{clientID: "bff", redirectURIs: []string{"https://app.localhost:8443/bff/callback"}}}
	good := url.Values{
		"client_id": {"bff"}, "redirect_uri": {"https://app.localhost:8443/bff/callback"}, "response_type": {"code"},
		"scope": {"openid profile"}, "state": {"s"}, "code_challenge": {"c"}, "code_challenge_method": {"S256"},
	}
	with := func(k, v string) authzRequest {
		q := url.Values{}
		for kk, vv := range good {
			q[kk] = vv
		}
		q.Set(k, v)
		return parseAuthz(q)
	}

	cases := []struct {
		name          string
		req           authzRequest
		fatal, oauthE bool
	}{
		{"正しいリクエスト", parseAuthz(good), false, false},
		{"知らないクライアント", with("client_id", "evil"), true, false},
		{"未登録の redirect_uri（転送してはいけない）", with("redirect_uri", "https://evil.example.com/cb"), true, false},
		{"redirect_uri の前方一致は不可", with("redirect_uri", "https://app.localhost:8443/bff/callback/../x"), true, false},
		{"PKCE の plain は不可", with("code_challenge_method", "plain"), false, true},
		{"openid スコープ必須", with("scope", "profile"), false, true},
		{"state 必須", with("state", ""), false, true},
	}
	for _, tc := range cases {
		fatal, oe := a.validate(tc.req)
		if (fatal != "") != tc.fatal || (oe != "") != tc.oauthE {
			t.Errorf("%s: fatal=%q oauthErr=%q", tc.name, fatal, oe)
		}
	}
}
