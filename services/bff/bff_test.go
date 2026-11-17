package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestSafeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"":                         "/",
		"/":                        "/",
		"/notes?x=1":               "/notes?x=1",
		"//evil.example.com":       "/",
		"/\\evil.example.com":      "/",
		"https://evil.example.com": "/",
		"javascript:alert(1)":      "/",
		"notes":                    "/",
	} {
		if got := safeReturnTo(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestSessionTTL(t *testing.T) {
	s := &sessionStore{idle: 30 * time.Minute, absolute: 8 * time.Hour}
	now := time.Now()

	if got := s.ttl(&session{CreatedAt: now}, now); got != 30*time.Minute {
		t.Errorf("ログイン直後はアイドル期限: %v", got)
	}
	if got := s.ttl(&session{CreatedAt: now.Add(-7*time.Hour - 50*time.Minute)}, now); got != 10*time.Minute {
		t.Errorf("絶対期限が近いときは残りの時間: %v", got)
	}
	if got := s.ttl(&session{CreatedAt: now.Add(-9 * time.Hour)}, now); got > 0 {
		t.Errorf("絶対期限を過ぎたら 0 以下: %v", got)
	}
}

func TestCheckCSRF(t *testing.T) {
	a := &app{cfg: config{appOrigin: "https://app.localhost:8443"}}
	s := &session{CSRFToken: "tok"}

	cases := []struct {
		name, method, origin, token string
		want                        bool
	}{
		{"GET は検査しない", "GET", "", "", true},
		{"正しい Origin とトークン", "POST", "https://app.localhost:8443", "tok", true},
		{"トークンなし", "POST", "https://app.localhost:8443", "", false},
		{"トークン違い", "POST", "https://app.localhost:8443", "nope", false},
		{"Origin が別サイト", "POST", "https://evil.example.com", "tok", false},
		{"Origin なし", "DELETE", "", "tok", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, "/api/notes", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.token != "" {
			r.Header.Set("X-CSRF-Token", tc.token)
		}
		if got := a.checkCSRF(r, s); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestSessionKeyIsHashed(t *testing.T) {
	sid := randomToken(32)
	if k := sessionKey(sid); k == "sess:"+sid || len(k) != len("sess:")+64 {
		t.Errorf("Redis のキーにセッション ID をそのまま使っている: %s", k)
	}
}
