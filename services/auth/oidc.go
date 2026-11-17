package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	ssoCookie  = "__Host-auth_sso"  // 認証基盤自身のログイン状態（BFF のセッションとは別物）
	csrfCookie = "__Host-auth_csrf" // ログインフォームの CSRF 対策（ダブルサブミット）
)

// ---- ディスカバリと JWKS -----------------------------------------------------

func (a *app) discovery(w http.ResponseWriter, r *http.Request) {
	iss := a.cfg.issuer
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/authorize",
		"token_endpoint":                        iss + "/token",
		"jwks_uri":                              iss + "/jwks",
		"revocation_endpoint":                   iss + "/revoke",
		"end_session_endpoint":                  iss + "/logout",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"ES256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"scopes_supported":                      []string{"openid", "profile"},
	})
}

func (a *app) jwks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(a.signer.jwks())
}

// ---- 認可リクエスト ---------------------------------------------------------

type authzRequest struct {
	ClientID, RedirectURI, ResponseType, Scope, State, Nonce, CodeChallenge, CodeChallengeMethod, Prompt string
}

func parseAuthz(q url.Values) authzRequest {
	return authzRequest{
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), ResponseType: q.Get("response_type"),
		Scope: q.Get("scope"), State: q.Get("state"), Nonce: q.Get("nonce"),
		CodeChallenge: q.Get("code_challenge"), CodeChallengeMethod: q.Get("code_challenge_method"), Prompt: q.Get("prompt"),
	}
}

// validate は2段階。client_id と redirect_uri が正しくなければ、どこにも転送してはいけない
// （オープンリダイレクトになる）ので画面でエラーを返す。それ以外の誤りは redirect_uri にエラーを返す。
func (a *app) validate(req authzRequest) (fatal string, oauthErr string) {
	if req.ClientID != a.cfg.clientID {
		return "unknown client_id", ""
	}
	if !slices.Contains(a.cfg.redirectURIs, req.RedirectURI) { // 前方一致ではなく完全一致
		return "redirect_uri is not registered", ""
	}
	switch {
	case req.ResponseType != "code":
		return "", "unsupported_response_type"
	case !slices.Contains(strings.Fields(req.Scope), "openid"):
		return "", "invalid_scope"
	case req.State == "":
		return "", "invalid_request"
	case req.CodeChallenge == "" || req.CodeChallengeMethod != "S256": // PKCE は必須。plain は受け付けない
		return "", "invalid_request"
	}
	return "", ""
}

func (a *app) authorize(w http.ResponseWriter, r *http.Request) {
	req := parseAuthz(r.URL.Query())
	if fatal, oauthErr := a.validate(req); fatal != "" {
		a.errorPage(w, http.StatusBadRequest, fatal)
		return
	} else if oauthErr != "" {
		redirectWithError(w, r, req, oauthErr)
		return
	}

	// SSO: 認証基盤に既にログインしていれば、フォームを出さずにコードを発行する
	if req.Prompt != "login" {
		if c, err := r.Cookie(ssoCookie); err == nil {
			if userID, authTime, ok := a.store.ssoSession(r.Context(), c.Value); ok {
				slog.Info("sso hit: issuing code without login form", "user", userID)
				a.issueCode(w, r, req, userID, authTime)
				return
			}
		}
	}
	a.loginPage(w, r, http.StatusOK, "")
}

// ---- ログインフォーム ---------------------------------------------------------

//go:embed login.html
var loginHTML string
var loginTmpl = template.Must(template.New("login").Parse(loginHTML))

func (a *app) loginPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	csrf := randomToken(24)
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: csrf, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, map[string]string{
		// 認可リクエストのパラメータは、フォームの送信先 URL のクエリでそのまま引き継ぐ
		"Action": "/login?" + r.URL.RawQuery,
		"CSRF":   csrf,
		"Error":  msg,
	})
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	req := parseAuthz(r.URL.Query())
	if fatal, oauthErr := a.validate(req); fatal != "" || oauthErr != "" {
		a.errorPage(w, http.StatusBadRequest, "invalid authorization request")
		return
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil || !equalConstantTime(c.Value, r.PostFormValue("csrf")) {
		a.errorPage(w, http.StatusForbidden, "login form expired. please retry")
		return
	}

	username, password := r.PostFormValue("username"), r.PostFormValue("password")
	u, err := a.store.userByUsername(r.Context(), username)
	if err != nil {
		a.errorPage(w, http.StatusInternalServerError, "internal error")
		return
	}
	hash := dummyHash
	if u != nil {
		hash = u.PasswordHash
	}
	okPw, _ := verifyPassword(password, hash) // ユーザーがいなくても同じ計算をして、時間差を作らない
	if u == nil || !okPw {
		slog.Info("login failed", "remote", r.Header.Get("X-Real-IP")) // ユーザー名・パスワードはログに出さない
		a.loginPage(w, r, http.StatusUnauthorized, "ユーザー名またはパスワードが違います")
		return
	}

	// ログインのたびに新しい SSO セッション ID を発行する（固定攻撃の対策）
	if old, err := r.Cookie(ssoCookie); err == nil {
		_ = a.store.deleteSSO(r.Context(), old.Value)
	}
	now := time.Now()
	sid := randomToken(32)
	if err := a.store.createSSO(r.Context(), sid, u.ID, now, now.Add(a.cfg.ssoTTL)); err != nil {
		a.errorPage(w, http.StatusInternalServerError, "internal error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: ssoCookie, Value: sid, Path: "/", MaxAge: int(a.cfg.ssoTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true})
	slog.Info("login succeeded", "user", u.ID)
	a.issueCode(w, r, req, u.ID, now)
}

// issueCode は1分だけ有効な、一度しか使えない認可コードを発行して redirect_uri に返す。
func (a *app) issueCode(w http.ResponseWriter, r *http.Request, req authzRequest, userID string, authTime time.Time) {
	code := randomToken(32)
	err := a.store.saveCode(r.Context(), code, authCode{
		ClientID: req.ClientID, UserID: userID, RedirectURI: req.RedirectURI, Scope: req.Scope,
		Nonce: req.Nonce, CodeChallenge: req.CodeChallenge, AuthTime: authTime, ExpiresAt: time.Now().Add(a.cfg.codeTTL),
	})
	if err != nil {
		a.errorPage(w, http.StatusInternalServerError, "internal error")
		return
	}
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	q.Set("state", req.State)
	q.Set("iss", a.cfg.issuer) // RFC 9207: どの IdP からの応答かを示す（IdP 取り違え攻撃の対策）
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func redirectWithError(w http.ResponseWriter, r *http.Request, req authzRequest, code string) {
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	q.Set("error", code)
	if req.State != "" {
		q.Set("state", req.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// ---- トークンエンドポイント（BFF がサーバー間で呼ぶ） -------------------------------

func (a *app) authenticateClient(r *http.Request) bool {
	id, secret, ok := r.BasicAuth()
	if ok {
		// client_secret_basic では、ID とシークレットは URL エンコードしてから Basic にする決まり（RFC 6749 2.3.1）
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	return equalConstantTime(id, a.cfg.clientID) && equalConstantTime(secret, a.cfg.clientSecret)
}

func tokenError(w http.ResponseWriter, status int, code string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="token"`)
	}
	writeJSON(w, status, map[string]string{"error": code})
}

func (a *app) token(w http.ResponseWriter, r *http.Request) {
	if !a.authenticateClient(r) {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		a.tokenFromCode(w, r)
	case "refresh_token":
		a.tokenFromRefresh(w, r)
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type")
	}
}

func (a *app) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := a.store.consumeCode(ctx, r.PostFormValue("code"))
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if c == nil || c.ClientID != a.cfg.clientID || c.RedirectURI != r.PostFormValue("redirect_uri") ||
		!verifyPKCE(r.PostFormValue("code_verifier"), c.CodeChallenge) {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	u, err := a.store.userByID(ctx, c.UserID)
	if err != nil || u == nil {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	a.respondTokens(w, ctx, u, c.Scope, c.Nonce, c.AuthTime, "", time.Time{})
}

func (a *app) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rt, err := a.store.rotateRefresh(ctx, r.PostFormValue("refresh_token"))
	if err == errRefreshReused {
		slog.Warn("refresh token reuse detected: family revoked")
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if rt == nil || rt.ClientID != a.cfg.clientID {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	u, err := a.store.userByID(ctx, rt.UserID) // ロールが変わっていれば、新しいトークンに反映される
	if err != nil || u == nil {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	a.respondTokens(w, ctx, u, rt.Scope, "", time.Time{}, rt.FamilyID, rt.ExpiresAt)
}

// respondTokens: アクセストークン（API 向け）・ID トークン（BFF 向け）・リフレッシュトークンを返す。
// family が空なら新しい系統（ログイン）、空でなければ同じ系統で次のリフレッシュトークン（ローテーション）。
// ローテーションしても系統の期限（familyExpiry）は延ばさない。延ばすと、使い続ける限り永遠に有効になってしまう。
func (a *app) respondTokens(w http.ResponseWriter, ctx context.Context, u *user, scope, nonce string, authTime time.Time, family string, familyExpiry time.Time) {
	now := time.Now()
	access, err := a.signer.sign("at+jwt", jwt.MapClaims{ // RFC 9068: アクセストークンの JWT 形式
		"iss":                a.cfg.issuer,
		"sub":                u.ID,
		"aud":                a.cfg.audience, // このトークンを受け取ってよいのは API だけ
		"client_id":          a.cfg.clientID,
		"scope":              scope,
		"iat":                now.Unix(),
		"exp":                now.Add(a.cfg.accessTokenTTL).Unix(),
		"jti":                randomToken(12),
		"preferred_username": u.Username,
		"name":               u.Name,
		"roles":              u.Roles,
	})
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error")
		return
	}

	refresh := randomToken(32)
	if family == "" {
		family = randomToken(12)
		familyExpiry = now.Add(a.cfg.refreshTokenTTL)
	}
	if err := a.store.saveRefresh(ctx, refresh, refreshToken{
		FamilyID: family, ClientID: a.cfg.clientID, UserID: u.ID, Scope: scope, ExpiresAt: familyExpiry,
	}); err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error")
		return
	}

	res := map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(a.cfg.accessTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	}
	if !authTime.IsZero() { // ID トークンはログイン（コード交換）のときだけ
		idClaims := jwt.MapClaims{
			"iss":                a.cfg.issuer,
			"sub":                u.ID,
			"aud":                a.cfg.clientID, // ID トークンを受け取るのは BFF（クライアント）
			"iat":                now.Unix(),
			"exp":                now.Add(5 * time.Minute).Unix(),
			"auth_time":          authTime.Unix(),
			"preferred_username": u.Username,
			"name":               u.Name,
			"roles":              u.Roles,
		}
		if nonce != "" {
			idClaims["nonce"] = nonce
		}
		id, err := a.signer.sign("JWT", idClaims)
		if err != nil {
			tokenError(w, http.StatusInternalServerError, "server_error")
			return
		}
		res["id_token"] = id
	}
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, res)
}

// ---- 失効とログアウト ---------------------------------------------------------

func (a *app) revoke(w http.ResponseWriter, r *http.Request) {
	if !a.authenticateClient(r) {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	_ = a.store.revokeRefresh(r.Context(), r.PostFormValue("token"), a.cfg.clientID)
	w.WriteHeader(http.StatusOK) // RFC 7009: 知らないトークンでも 200 を返す
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(ssoCookie); err == nil {
		_ = a.store.deleteSSO(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	target := r.URL.Query().Get("post_logout_redirect_uri")
	if target != "" && slices.Contains(a.cfg.postLogoutURIs, target) {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	a.errorPage(w, http.StatusOK, "ログアウトしました")
}

// ---- 画面 --------------------------------------------------------------------

var errorTmpl = template.Must(template.New("error").Parse(`<!doctype html><html lang="ja"><head><meta charset="utf-8">
<title>auth-sandbox IdP</title><link rel="stylesheet" href="/assets/style.css"></head>
<body><main class="card"><h1>auth-sandbox IdP</h1><p>{{.}}</p></main></body></html>`))

func (a *app) errorPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = errorTmpl.Execute(w, msg)
}

//go:embed style.css
var styleCSS []byte

func serveCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(styleCSS)
}
