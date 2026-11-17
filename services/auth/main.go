// auth: 学習用の「最小の OpenID Connect プロバイダ」。
//
// 提供するもの（Keycloak などと同じ形にしてあるので、BFF は設定だけで差し替えられる）
//
//	GET  /.well-known/openid-configuration  ディスカバリ
//	GET  /jwks                              署名の公開鍵（API がトークンの検証に使う）
//	GET  /authorize                         認可エンドポイント（ログイン画面 or SSO で即コード発行）
//	POST /login                             ログインフォームの送信先
//	POST /token                             認可コード／リフレッシュトークンをトークンに交換（BFF がサーバー間で呼ぶ）
//	POST /revoke                            リフレッシュトークンの失効
//	GET  /logout                            認証基盤の SSO セッションを終える
//
// 本番では使わないこと。省いたもの（同意画面・複数クライアント管理・鍵の永続化など）は docs/03 を参照。
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const addr = ":8080"

type config struct {
	issuer          string // ブラウザから見える URL（トークンの iss）
	clientID        string
	clientSecret    string
	redirectURIs    []string
	postLogoutURIs  []string
	audience        string // アクセストークンの aud（API の名前）
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
	ssoTTL          time.Duration
	codeTTL         time.Duration
}

type app struct {
	cfg    config
	store  *store
	signer *signer
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(addr))
	}

	cfg := config{
		issuer:          strings.TrimRight(mustEnv("ISSUER"), "/"),
		clientID:        mustEnv("CLIENT_ID"),
		clientSecret:    mustEnv("CLIENT_SECRET"),
		redirectURIs:    splitList(mustEnv("REDIRECT_URIS")),
		postLogoutURIs:  splitList(env("POST_LOGOUT_REDIRECT_URIS", "")),
		audience:        env("API_AUDIENCE", "sandbox-api"),
		accessTokenTTL:  mustDuration("ACCESS_TOKEN_TTL", "5m"),
		refreshTokenTTL: mustDuration("REFRESH_TOKEN_TTL", "8h"),
		ssoTTL:          mustDuration("SSO_TTL", "8h"),
		codeTTL:         time.Minute,
	}

	ctx := context.Background()
	st, err := openStore(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		slog.Error("db", "err", err)
		os.Exit(1)
	}
	if err := seedUsers(ctx, st, env("SEED_USERS", ""), mustEnv("DEMO_PASSWORD")); err != nil {
		slog.Error("seed", "err", err)
		os.Exit(1)
	}

	a := &app{cfg: cfg, store: st, signer: newSigner()}
	slog.Info("signing key ready", "kid", a.signer.kid, "issuer", cfg.issuer)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /.well-known/openid-configuration", a.discovery)
	mux.HandleFunc("GET /jwks", a.jwks)
	mux.HandleFunc("GET /authorize", a.authorize)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /token", a.token)
	mux.HandleFunc("POST /revoke", a.revoke)
	mux.HandleFunc("GET /logout", a.logout)
	mux.HandleFunc("GET /assets/style.css", serveCSS)

	serve(addr, mux)
}

// SEED_USERS="alice:Alice:editor,viewer;bob:Bob:viewer" — 全員 DEMO_PASSWORD でログインできる
func seedUsers(ctx context.Context, st *store, spec, password string) error {
	for _, entry := range splitBy(spec, ";") {
		f := strings.SplitN(entry, ":", 3)
		if len(f) != 3 {
			continue
		}
		if err := st.seedUser(ctx, f[0], f[1], password, splitList(f[2])); err != nil {
			return err
		}
	}
	return nil
}

func splitList(s string) []string { return splitBy(s, ",") }

func splitBy(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mustDuration(key, def string) time.Duration {
	d, err := time.ParseDuration(env(key, def))
	if err != nil {
		slog.Error("bad duration", "key", key, "err", err)
		os.Exit(1)
	}
	return d
}
