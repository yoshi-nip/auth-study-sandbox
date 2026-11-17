package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "__Host-sid"       // __Host- プレフィックス: Secure・Path=/・Domain なしでないとブラウザが受け付けない
	loginCookie   = "__Host-bff_login" // ログイン開始〜コールバックの間だけ、state をこのブラウザに結びつける
)

type rejected struct {
	Error      string `json:"error"`
	RejectedBy string `json:"rejected_by"`
}

// ---- ログイン開始: 認可エンドポイントへ転送 -------------------------------------

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	state := randomToken(24)              // CSRF 対策: 自分が始めたログインの応答だけを受け付ける
	nonce := randomToken(24)              // リプレイ対策: ID トークンがこのログインのために発行されたことを確かめる
	verifier := oauth2.GenerateVerifier() // PKCE: 認可コードを横取りされても交換できないようにする

	if err := a.sessions.saveLogin(r.Context(), state, loginState{
		Verifier: verifier, Nonce: nonce, ReturnTo: safeReturnTo(r.URL.Query().Get("return_to")),
	}); err != nil {
		a.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: state, Path: "/", MaxAge: int(loginStateTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, // IdP からのトップレベル遷移で送られるよう Lax
	})
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

// safeReturnTo はログイン後の戻り先を「自サイト内のパス」に限る（オープンリダイレクト対策）。
//
// 先頭だけを見るのでは足りない。http.Redirect は相対パスに path.Clean をかけるので、
// "/./\evil.example" は検査のあとで "/\evil.example" に変わり、ブラウザは "//evil.example"（別サイト）と解釈する。
// そこで (1) バックスラッシュと制御文字は一切認めない (2) 自分で Clean した結果を検査して、その結果を返す。
func safeReturnTo(p string) string {
	if p == "" || strings.ContainsAny(p, "\\\r\n\t") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path, "\\") {
		return "/"
	}
	cleaned := path.Clean(u.Path)
	if strings.HasPrefix(cleaned, "//") || strings.HasPrefix(p, "//") {
		return "/"
	}
	out := &url.URL{Path: cleaned, RawQuery: u.RawQuery}
	return out.String()
}

// ---- コールバック: コードをトークンに交換し、セッションを作る ------------------------

func (a *app) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clearCookie(w, loginCookie)
	if e := q.Get("error"); e != "" {
		writeJSON(w, http.StatusBadRequest, rejected{Error: "login failed: " + e, RejectedBy: "bff"})
		return
	}
	c, err := r.Cookie(loginCookie)
	state := q.Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		writeJSON(w, http.StatusBadRequest, rejected{Error: "state mismatch", RejectedBy: "bff"})
		return
	}
	ls, err := a.sessions.takeLogin(r.Context(), state)
	if err != nil {
		a.fail(w, err)
		return
	}
	if ls == nil {
		writeJSON(w, http.StatusBadRequest, rejected{Error: "login expired or already used", RejectedBy: "bff"})
		return
	}

	ctx := a.oauthContext(r.Context())
	// バックチャネル: BFF → 認証基盤（コンテナ間の内部 URL）。ブラウザはこの通信を見ない
	tok, err := a.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		slog.Warn("code exchange failed", "err", err)
		writeJSON(w, http.StatusBadRequest, rejected{Error: "code exchange failed", RejectedBy: "bff"})
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := a.idVerifier.Verify(ctx, rawID) // 署名・iss・aud（= client_id）・exp
	if err != nil || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(ls.Nonce)) != 1 {
		slog.Warn("id token rejected", "err", err)
		writeJSON(w, http.StatusBadRequest, rejected{Error: "invalid id token", RejectedBy: "bff"})
		return
	}
	var claims struct {
		Username string   `json:"preferred_username"`
		Name     string   `json:"name"`
		Roles    []string `json:"roles"`
	}
	_ = idt.Claims(&claims)

	// ログイン前に持っていたセッションは捨て、必ず新しい ID を発行する（セッション固定攻撃の対策）
	if old, err := r.Cookie(sessionCookie); err == nil {
		_ = a.sessions.delete(r.Context(), old.Value)
	}
	sid, err := a.sessions.create(r.Context(), &session{
		Subject: idt.Subject, Username: claims.Username, Name: claims.Name, Roles: claims.Roles,
		AccessToken: tok.AccessToken, AccessExpiry: tok.Expiry, RefreshToken: tok.RefreshToken, IDToken: rawID,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sid, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		// MaxAge なし = ブラウザを閉じると消える。期限の正本は Redis 側の TTL
	})
	slog.Info("session created", "sub", idt.Subject)
	http.Redirect(w, r, ls.ReturnTo, http.StatusFound)
}

// ---- セッションの読み出し ------------------------------------------------------

type sessionKeyCtx struct{}
type sidKeyCtx struct{}

// withSession: Cookie からセッションを引き、無ければ 401。
func (a *app) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid, sess, err := a.loadSession(r)
		if err != nil {
			a.fail(w, err)
			return
		}
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, rejected{Error: "unauthenticated", RejectedBy: "bff"})
			return
		}
		ctx := context.WithValue(context.WithValue(r.Context(), sessionKeyCtx{}, sess), sidKeyCtx{}, sid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *app) loadSession(r *http.Request) (string, *session, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", nil, nil
	}
	sess, err := a.sessions.get(r.Context(), c.Value)
	return c.Value, sess, err
}

func sessionFrom(r *http.Request) (string, *session) {
	return r.Context().Value(sidKeyCtx{}).(string), r.Context().Value(sessionKeyCtx{}).(*session)
}

// GET /bff/session: フロントが「ログインしているか」「誰か」を知るためのもの。トークンは返さない。
func (a *app) sessionInfo(w http.ResponseWriter, r *http.Request) {
	_, s := sessionFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":           true,
		"user":                    map[string]any{"sub": s.Subject, "username": s.Username, "name": s.Name, "roles": s.Roles},
		"csrf_token":              s.CSRFToken, // 状態を変えるリクエストで X-CSRF-Token ヘッダに入れて送り返す
		"access_token_expires_at": s.AccessExpiry.UTC(),
		"session_created_at":      s.CreatedAt.UTC(),
		"served_by":               hostname,
	})
}

// GET /bff/session/check: nginx の auth_request 用。本文なしで 204 か 401 を返すだけ。
func (a *app) sessionCheck(w http.ResponseWriter, r *http.Request) {
	_, sess, err := a.loadSession(r)
	if err != nil || sess == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- CSRF ----------------------------------------------------------------------

// checkCSRF: 状態を変えるメソッドは、(1) Origin が自サイト (2) セッションに結びついた CSRF トークンが一致、の両方を求める。
// SameSite=Lax の Cookie でも、同じサイト内の別オリジンや古いブラウザからの偽造は防ぎきれないため、二重にする。
func (a *app) checkCSRF(r *http.Request, s *session) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if r.Header.Get("Origin") != a.cfg.appOrigin {
		return false
	}
	t := r.Header.Get("X-CSRF-Token")
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.CSRFToken)) == 1
}

func (a *app) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, s := sessionFrom(r)
		if !a.checkCSRF(r, s) {
			writeJSON(w, http.StatusForbidden, rejected{Error: "csrf check failed", RejectedBy: "bff"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- アクセストークンのリフレッシュ ------------------------------------------------

var errSessionGone = errors.New("session gone")

// freshToken: アクセストークンの期限が近ければ、リフレッシュトークンで取り直してセッションを更新する。
// 同じセッションから同時に複数のリクエストが来ても、リフレッシュは1回だけにする（Redis のロック）。
// リフレッシュトークンは使うたびに入れ替わる（ローテーション）ので、2回使うと再利用とみなされ全部失効するため。
func (a *app) freshToken(ctx context.Context, sid string, s *session) (string, error) {
	if time.Until(s.AccessExpiry) > a.cfg.refreshSkew {
		return s.AccessToken, nil
	}
	lock := sessionKey(sid) + ":refresh-lock"
	got, err := a.sessions.rdb.SetNX(ctx, lock, hostname, 10*time.Second).Result()
	if err != nil {
		return "", err
	}
	if !got { // 他のリクエスト（別の BFF の台かもしれない）がリフレッシュ中。終わるのを待って読み直す
		for range 50 {
			time.Sleep(100 * time.Millisecond)
			s2, err := a.sessions.get(ctx, sid)
			if err != nil || s2 == nil {
				return "", errSessionGone
			}
			if time.Until(s2.AccessExpiry) > a.cfg.refreshSkew {
				return s2.AccessToken, nil
			}
		}
		return "", errors.New("refresh wait timeout")
	}
	defer a.sessions.rdb.Del(context.WithoutCancel(ctx), lock)

	// ロックを取る直前に、別のリクエストがリフレッシュを終えていたかもしれないので読み直す
	if s2, err := a.sessions.get(ctx, sid); err == nil && s2 != nil {
		s = s2
		if time.Until(s.AccessExpiry) > a.cfg.refreshSkew {
			return s.AccessToken, nil
		}
	}

	nt, err := a.oauth.TokenSource(a.oauthContext(ctx), &oauth2.Token{RefreshToken: s.RefreshToken}).Token()
	if err != nil {
		// リフレッシュトークンが失効・期限切れ: ログインし直してもらうしかない
		slog.Warn("refresh failed; dropping session", "err", err)
		_ = a.sessions.delete(ctx, sid)
		return "", errSessionGone
	}
	s.AccessToken, s.AccessExpiry = nt.AccessToken, nt.Expiry
	if nt.RefreshToken != "" {
		s.RefreshToken = nt.RefreshToken
	}
	if err := a.sessions.save(ctx, sid, s); err != nil {
		return "", err
	}
	slog.Info("access token refreshed", "sub", s.Subject, "expires_at", nt.Expiry)
	return s.AccessToken, nil
}

// ---- ログアウト -----------------------------------------------------------------

// POST /bff/logout: セッションを消し、リフレッシュトークンを失効させ、
// 認証基盤側の SSO セッションも終わらせるための URL を返す（フロントがそこへ遷移する）。
func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	sid, s := sessionFrom(r)
	if a.cfg.revocationURL != "" && s.RefreshToken != "" {
		if err := a.revoke(r.Context(), s.RefreshToken); err != nil {
			slog.Warn("revoke failed", "err", err) // 失効に失敗してもログアウト自体は進める
		}
	}
	_ = a.sessions.delete(r.Context(), sid)
	clearCookie(w, sessionCookie)

	logoutURL := ""
	if a.cfg.endSessionURL != "" {
		u, _ := url.Parse(a.cfg.endSessionURL)
		q := u.Query()
		q.Set("client_id", a.cfg.clientID)
		q.Set("post_logout_redirect_uri", a.cfg.postLogoutURL)
		// id_token_hint は付けない。付けると ID トークンがブラウザ（URL・履歴）に出て、BFF の前提が崩れる
		u.RawQuery = q.Encode()
		logoutURL = u.String()
	}
	writeJSON(w, http.StatusOK, map[string]string{"logout_url": logoutURL})
}

func (a *app) revoke(ctx context.Context, refreshToken string) error {
	form := url.Values{"token": {refreshToken}, "token_type_hint": {"refresh_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.revocationURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(a.cfg.clientID), url.QueryEscape(a.cfg.clientSecret))
	res, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("revoke: " + res.Status)
	}
	return nil
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func (a *app) fail(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	writeJSON(w, http.StatusInternalServerError, rejected{Error: "internal error", RejectedBy: "bff"})
}
