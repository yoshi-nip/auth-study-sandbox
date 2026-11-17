// api: ビジネスロジックを持つバックエンド。ブラウザのことは知らない。
// 受け付けるのは「Authorization: Bearer <JWT>」だけで、Cookie もセッションも持たない（ステートレス）。
// JWT は認証基盤の公開鍵（JWKS）で自分で検証する。認証基盤に毎回問い合わせない。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
)

const addr = ":8080"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(addr))
	}

	// JWKS は最初に必要になったときに取りに行き、知らない kid が来たら取り直す（鍵のローテーションに追従）
	keys := oidc.NewRemoteKeySet(context.Background(), mustEnv("OIDC_JWKS_URL"))
	verifier := oidc.NewVerifier(mustEnv("OIDC_ISSUER"), keys, &oidc.Config{
		ClientID: env("API_AUDIENCE", "sandbox-api"), // aud にこの名前が入っていないトークンは拒否
		// 受け付ける署名アルゴリズムを固定する（alg=none や想定外のアルゴリズムへのすり替えを防ぐ）
		SupportedSigningAlgs: []string{oidc.ES256, oidc.RS256},
	})
	a := &api{notes: map[string][]note{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/public/hello", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "hello from api", "served_by": hostname})
	})

	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/me", a.me)
	authed.HandleFunc("GET /api/notes", a.listNotes)
	authed.Handle("POST /api/notes", requireRole("editor", http.HandlerFunc(a.createNote)))
	mux.Handle("/api/", requireToken(verifier, authed))

	serve(addr, mux)
}

// ---- 認証（トークンの検証） ----------------------------------------------------

type claims struct {
	Subject  string   `json:"sub"`
	Username string   `json:"preferred_username"`
	Name     string   `json:"name"`
	Roles    []string `json:"roles"`
	Scope    string   `json:"scope"`
	Expiry   int64    `json:"exp"`
}

type claimsKey struct{}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func unauthorized(w http.ResponseWriter, desc string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token", "detail": desc, "rejected_by": "api"})
}

func requireToken(v *oidc.IDTokenVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			unauthorized(w, "missing bearer token")
			return
		}
		// 署名（JWKS）・iss・aud・exp をまとめて検証する
		tok, err := v.Verify(r.Context(), raw)
		if err != nil {
			unauthorized(w, "token verification failed")
			return
		}
		var c claims
		if err := tok.Claims(&c); err != nil || c.Subject == "" {
			unauthorized(w, "bad claims")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, &c)))
	})
}

func claimsFrom(r *http.Request) *claims { return r.Context().Value(claimsKey{}).(*claims) }

// ---- 認可（ロール） --------------------------------------------------------------

// requireRole: 「誰か」は分かった（認証）うえで、「それをしてよいか」（認可）を API 自身が決める。
func requireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(claimsFrom(r).Roles, role) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "detail": "role '" + role + "' required", "rejected_by": "api"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- ハンドラ ----------------------------------------------------------------

type note struct {
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

type api struct {
	mu    sync.Mutex
	notes map[string][]note // ユーザー（sub）ごとのメモ。学習用なのでメモリに置く
}

func (a *api) me(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"sub": c.Subject, "username": c.Username, "name": c.Name, "roles": c.Roles, "scope": c.Scope,
		"token_expires_at": time.Unix(c.Expiry, 0).UTC(), "served_by": hostname,
	})
}

func (a *api) listNotes(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.notes[claimsFrom(r).Subject]
	if list == nil {
		list = []note{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": list})
}

func (a *api) createNote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil ||
		strings.TrimSpace(in.Text) == "" || utf8.RuneCountInString(in.Text) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required (max 200 chars)"})
		return
	}
	n := note{Text: strings.TrimSpace(in.Text), CreatedAt: time.Now().UTC()}
	a.mu.Lock()
	sub := claimsFrom(r).Subject
	a.notes[sub] = append(a.notes[sub], n)
	a.mu.Unlock()
	writeJSON(w, http.StatusCreated, n)
}
