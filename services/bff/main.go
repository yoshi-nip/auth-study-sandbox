// bff: Backend For Frontend。ブラウザと内部サービスの「境目」。
//
//	ブラウザ ⇄ BFF : ステートフル。HttpOnly Cookie のセッション ID だけ。中身（トークン）は Redis
//	BFF ⇄ 認証基盤 : OIDC の認可コードフロー（PKCE）。トークン交換・リフレッシュ・失効はサーバー間で行う
//	BFF ⇄ API     : ステートレス。Authorization: Bearer <アクセストークン（JWT）>。Cookie は渡さない
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
)

const addr = ":8080"

type config struct {
	appOrigin         string // ブラウザから見たこのアプリのオリジン（CSRF の Origin 検査に使う）
	clientID          string
	clientSecret      string
	postLogoutURL     string
	revocationURL     string
	endSessionURL     string
	logoutIDTokenHint bool
	refreshSkew       time.Duration
}

type app struct {
	cfg        config
	sessions   *sessionStore
	oauth      *oauth2.Config
	idVerifier *oidc.IDTokenVerifier
	httpClient *http.Client
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(addr))
	}

	appOrigin := strings.TrimRight(mustEnv("APP_ORIGIN"), "/")
	cfg := config{
		appOrigin:         appOrigin,
		clientID:          mustEnv("OIDC_CLIENT_ID"),
		clientSecret:      mustEnv("OIDC_CLIENT_SECRET"),
		postLogoutURL:     appOrigin + "/",
		revocationURL:     env("OIDC_REVOCATION_URL", ""),
		endSessionURL:     env("OIDC_END_SESSION_URL", ""),
		logoutIDTokenHint: env("OIDC_LOGOUT_ID_TOKEN_HINT", "false") == "true",
		refreshSkew:       mustDuration("REFRESH_SKEW", "30s"),
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	a := &app{
		cfg: cfg,
		sessions: &sessionStore{
			rdb:      redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "redis:6379")}),
			idle:     mustDuration("SESSION_IDLE_TTL", "30m"),
			absolute: mustDuration("SESSION_ABSOLUTE_TTL", "8h"),
		},
		oauth: &oauth2.Config{
			ClientID:     cfg.clientID,
			ClientSecret: cfg.clientSecret,
			RedirectURL:  appOrigin + "/bff/callback",
			Scopes:       strings.Fields(env("OIDC_SCOPES", "openid profile")),
			Endpoint: oauth2.Endpoint{
				// 認可エンドポイントはブラウザが開くので「外から見える URL」、
				// トークンエンドポイントは BFF が直接呼ぶので「コンテナ間の URL」。ここが分かれるのがコンテナ構成の肝
				AuthURL:   mustEnv("OIDC_AUTHORIZE_URL"),
				TokenURL:  mustEnv("OIDC_TOKEN_URL"),
				AuthStyle: oauth2.AuthStyleInHeader,
			},
		},
		httpClient: httpClient,
	}
	// ID トークンの検証。iss はブラウザから見える URL、鍵（JWKS）はコンテナ間の URL から取る
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), httpClient), mustEnv("OIDC_JWKS_URL"))
	a.idVerifier = oidc.NewVerifier(mustEnv("OIDC_ISSUER"), keys, &oidc.Config{
		ClientID:             cfg.clientID,
		SupportedSigningAlgs: []string{oidc.ES256, oidc.RS256},
	})

	apiURL, err := url.Parse(env("API_URL", "http://api:8080"))
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /bff/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"served_by": hostname, "remote_addr": r.RemoteAddr, "x_forwarded_for": r.Header.Get("X-Forwarded-For"),
		})
	})
	mux.HandleFunc("GET /bff/login", a.login)
	mux.HandleFunc("GET /bff/callback", a.callback)
	mux.HandleFunc("/bff/session/check", a.sessionCheck)
	mux.Handle("GET /bff/session", a.withSession(http.HandlerFunc(a.sessionInfo)))
	mux.Handle("POST /bff/logout", a.withSession(a.withCSRF(http.HandlerFunc(a.logout))))

	// 公開 API は認証なしでそのまま流す。それ以外の /api/ はセッション＋CSRF＋トークン付与を通す
	mux.Handle("GET /api/public/", a.apiProxy(apiURL, false))
	mux.Handle("/api/", a.withSession(a.withCSRF(a.apiProxy(apiURL, true))))

	serve(addr, mux)
}

type tokenKeyCtx struct{}

// apiProxy は API への中継。ブラウザの Cookie や CSRF ヘッダは捨て、代わりにアクセストークンを付ける。
func (a *app) apiProxy(target *url.URL, withToken bool) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.SetXForwarded()
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("X-CSRF-Token")
			r.Out.Header.Del("Authorization") // ブラウザが送ってきた Authorization は信用しない
			if tok, ok := r.In.Context().Value(tokenKeyCtx{}).(string); ok {
				r.Out.Header.Set("Authorization", "Bearer "+tok)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("api unreachable", "err", err)
			writeJSON(w, http.StatusBadGateway, rejected{Error: "api unreachable", RejectedBy: "bff"})
		},
	}
	if !withToken {
		return proxy
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid, s := sessionFrom(r)
		tok, err := a.freshToken(r.Context(), sid, s)
		if errors.Is(err, errSessionGone) {
			clearCookie(w, sessionCookie)
			writeJSON(w, http.StatusUnauthorized, rejected{Error: "session expired", RejectedBy: "bff"})
			return
		}
		if err != nil {
			a.fail(w, err)
			return
		}
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKeyCtx{}, tok)))
	})
}

// oauthContext: oauth2 ライブラリが使う HTTP クライアント（タイムアウト付き）を渡す
func (a *app) oauthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, a.httpClient)
}

func mustDuration(key, def string) time.Duration {
	v := env(key, def)
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	slog.Error("bad duration", "key", key)
	os.Exit(1)
	return 0
}
