#!/usr/bin/env bash
# E2E: 起動中のスタックに外から HTTP で当てて、受け入れ基準（docs/TASKS.md）を確かめる。
# 「通るべきものが通る」だけでなく「止まるべきものが止まる」を確かめるのが目的。
# 見出しの番号（P0-1 など）は docs/TASKS.md の受け入れ基準と対応する。
set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
set -a; . ./.env; set +a

PORT="${HTTPS_PORT:-8443}"
HTTP="${HTTP_PORT:-8080}"
APP="https://app.localhost:${PORT}"
CA="$root/nginx/certs/ca.crt"
WORK="$root/.e2e"
rm -rf "$WORK" && mkdir -p "$WORK"
PROBE_IMAGE="curlimages/curl:8.17.0"

# *.localhost を確実に 127.0.0.1 に向ける（OS の名前解決に依存しない）
RESOLVE=(--resolve "app.localhost:${PORT}:127.0.0.1" --resolve "auth.localhost:${PORT}:127.0.0.1"
         --resolve "kc.localhost:${PORT}:127.0.0.1" --resolve "unknown.localhost:${PORT}:127.0.0.1"
         --resolve "app.localhost:${HTTP}:127.0.0.1")
c() { curl -sS --cacert "$CA" --max-time 10 "${RESOLVE[@]}" "$@"; }

pass=0; fail=0
ok() { echo "  ✓ $1"; pass=$((pass + 1)); }
ng() { echo "  ✗ $1${2:+  — $2}"; fail=$((fail + 1)); }
expect() { # expect <名前> <実際> <期待>
  if [ "$2" = "$3" ]; then ok "$1"; else ng "$1" "期待: $3 / 実際: $2"; fi
}
expect_match() { # expect_match <名前> <実際> <正規表現>
  if printf '%s' "$2" | grep -qE -- "$3"; then ok "$1"; else ng "$1" "期待: /$3/ / 実際: $(printf '%s' "$2" | head -c 300)"; fi
}
expect_no_match() {
  if printf '%s' "$2" | grep -qE -- "$3"; then ng "$1" "含まれてはいけない: /$3/"; else ok "$1"; fi
}
status() { c -o /dev/null -w '%{http_code}' "$@"; }
section() { echo; echo "== $1"; }
# 指定したネットワークにだけ参加する使い捨てコンテナから curl する（≒「このサブネットから届くか」）
probe() { local net="$1"; shift; docker run --rm --network "$net" "$PROBE_IMAGE" -s -o /dev/null -w '%{http_code}' --max-time 3 "$@" 2>/dev/null; }

########################################################################
section "Phase 0 — L7 ルーティングと境界"

expect "P0-1 http は https へ 301" \
  "$(c -o /dev/null -w '%{http_code} %{redirect_url}' "http://app.localhost:${HTTP}/")" \
  "301 https://app.localhost:${PORT}/"

headers="$(c -D - -o "$WORK/index.html" "$APP/")"
expect_match "P0-2 静的フロントが 200" "$headers" '^HTTP/[0-9.]+ 200'
expect_match "P0-2 CSP が付く" "$headers" '[Cc]ontent-[Ss]ecurity-[Pp]olicy: default-src'
expect_match "P0-2 X-Frame-Options: DENY" "$headers" '[Xx]-[Ff]rame-[Oo]ptions: DENY'
expect_match "P0-2 nosniff" "$headers" '[Xx]-[Cc]ontent-[Tt]ype-[Oo]ptions: nosniff'
expect_match "P0-2 /api/public は nginx → BFF → API で届く" \
  "$(c "$APP/api/public/hello")" '"message":"hello from api"'

if c -o /dev/null "https://unknown.localhost:${PORT}/" 2>/dev/null; then
  ng "P0-3 知らない Host 名は TLS の握手で断る" "応答が返った"
else
  ok "P0-3 知らない Host 名は TLS の握手で断る"
fi

# SNI（TLS で名乗ったホスト）と Host ヘッダが食い違うと、nginx は 421 Misdirected Request で断る
expect_match "P0-3 SNI は正しくても Host が知らない名前なら中身を返さない（421 か切断）" \
  "$(c -o /dev/null -w '%{http_code}' -H 'Host: unknown.localhost' "$APP/" 2>/dev/null)" '^(421|000)$'
expect "P0-3 http でも知らない Host には応答しない" \
  "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H 'Host: unknown' "http://127.0.0.1:${HTTP}/" 2>/dev/null)" "000"

published="$(docker compose ps --format json | jq -r 'select((.Publishers // []) | map(select(.PublishedPort > 0)) | length > 0) | .Service' | sort -u | tr '\n' ' ')"
expect_match "P0-4 ホストにポートを公開しているのは nginx だけ" "$published" '^nginx $'

# 公開先は全ポートで loopback に限る。実際の Docker バインドを検査する。
bindings="$(docker inspect --format '{{json .NetworkSettings.Ports}}' "$(docker compose ps -q nginx)")"
expect "P0-7 nginx の全公開ポートは 127.0.0.1 に限定" \
  "$(printf '%s' "$bindings" | jq '[.[]? // [] | .[] | .HostIp] | length > 0 and all(. == "127.0.0.1")')" "true"

nginx_to() { docker compose exec -T nginx curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$1" 2>/dev/null; }
expect "P0-5 nginx → BFF は届く（陽性対照）" "$(nginx_to http://bff:8080/healthz)" "200"
expect "P0-5 nginx → API は届かない" "$(nginx_to http://api:8080/healthz)" "000"
expect "P0-5 backend ネットワークからは API に届く（陽性対照）" "$(probe auth-sandbox_backend http://api:8080/healthz)" "200"
expect "P0-5 app ネットワークからは API に届かない" "$(probe auth-sandbox_app http://api:8080/healthz)" "000"

for net in app backend bff-data auth-data; do
  docker network inspect "auth-sandbox_${net}" >/dev/null 2>&1 || continue   # そのフェーズでまだ無いネットワークは飛ばす
  internal="$(docker network inspect "auth-sandbox_${net}" --format '{{.Internal}}')"
  expect "P0-6 ${net} は internal" "$internal" "true"
done

########################################################################
# ログインを外から操作するための道具
IDP="${IDP:-own}"   # own = 自作 OIDC / keycloak = Keycloak（make e2e-keycloak）
if [ "$IDP" = "keycloak" ]; then AUTH="https://kc.localhost:${PORT}"; else AUTH="https://auth.localhost:${PORT}"; fi
cj() { local j="$1"; shift; c -b "$j" -c "$j" "$@"; }          # Cookie を jar に保存する curl（≒ 1つのブラウザ）
login_start() { cj "$1" -o /dev/null -w '%{redirect_url}' "$APP/bff/login"; }
# submit_login <jar> <user> <password> : 直前に取得したログイン画面（$WORK/page.html）のフォームを送る
submit_login() {
  local action csrf
  action="$(grep -oE '<form[^>]*action="[^"]*"' "$WORK/page.html" | head -1 | sed -E 's/.*action="([^"]*)"/\1/; s/&amp;/\&/g; s/&#43;/+/g')"   # HTML の文字参照を戻す
  case "$action" in http*) ;; *) action="${AUTH}${action}" ;; esac
  csrf="$(sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' "$WORK/page.html")"
  cj "$1" -o "$WORK/login_res.html" -w '%{http_code} %{redirect_url}' -X POST \
    ${csrf:+--data-urlencode "csrf=$csrf"} --data-urlencode "username=$2" --data-urlencode "password=$3" "$action"
}
# login_as <jar> <user> : ログイン開始 → フォーム → コールバックまで。コールバックのヘッダを $WORK/cb_headers に残す
login_as() {
  local authz cb
  authz="$(login_start "$1")"
  cj "$1" -o "$WORK/page.html" "$authz"
  cb="$(submit_login "$1" "$2" "$DEMO_PASSWORD")"
  cj "$1" -D "$WORK/cb_headers" -o /dev/null -w '%{http_code}' "${cb#* }"
}
sid_of() { awk '$6 == "__Host-sid" { print $7 }' "$1" | tail -1; }
csrf_of() { cj "$1" "$APP/bff/session" | jq -r '.csrf_token // empty'; }
post_note() { # post_note <jar> <text> [csrf] [origin]
  cj "$1" -o "$WORK/post.json" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    -H "Origin: ${4:-$APP}" ${3:+-H "X-CSRF-Token: $3"} -d "{\"text\":\"$2\"}" "$APP/api/notes"
}
redis_sessions() { docker compose exec -T redis redis-cli --scan --pattern 'sess:*' | grep -vc refresh-lock; }

########################################################################
section "Phase 1・2 — BFF のセッションと自作 OIDC（IdP=${IDP}）"

body="$(c "$APP/api/me")"
expect_match "P1-1 未ログインの /api/me は 401（nginx が止める）" "$body" '"rejected_by":"nginx"'

A="$WORK/alice.jar"; B="$WORK/bob.jar"; X="$WORK/x.jar"
authz="$(login_start "$A")"
expect_match "P2-1 認可エンドポイントへ転送" "$authz" "^${AUTH}/"
expect_match "P2-1 PKCE は S256" "$authz" 'code_challenge_method=S256'
expect_match "P2-1 code_challenge・state・nonce 付き" "$authz" 'code_challenge=[^&]+.*'
expect_match "P2-1 state 付き" "$authz" '[?&]state=[A-Za-z0-9_-]{20,}'
expect_match "P2-1 nonce 付き" "$authz" '[?&]nonce=[A-Za-z0-9_-]{20,}'

cj "$A" -o "$WORK/page.html" "$authz"
res="$(submit_login "$A" alice 'wrong-password')"
expect_match "P2-2 誤ったパスワードではコードが発行されない" "$res" '^(200|401) $'

if [ "$IDP" = "own" ]; then
  evil="$(c -o /dev/null -w '%{http_code} %{redirect_url}' "$AUTH/authorize?client_id=sandbox-bff&redirect_uri=https%3A%2F%2Fevil.example.com%2Fcb&response_type=code&scope=openid&state=x&code_challenge=abc&code_challenge_method=S256")"
  expect "P2-2 未登録の redirect_uri には転送しない" "$evil" "400 "
fi

login_start "$X" >/dev/null
expect "P2-3 state が合わないコールバックは 400" \
  "$(cj "$X" -o /dev/null -w '%{http_code}' "$APP/bff/callback?code=x&state=forged")" "400"

expect "P1-2 ログインできる（コールバックから / へ 302）" "$(login_as "$A" alice)" "302"
setcookie="$(grep -i '^set-cookie: __Host-sid=' "$WORK/cb_headers" | tr -d '\r')"
expect_match "P1-2 __Host-sid が付く" "$setcookie" '__Host-sid=[A-Za-z0-9_-]{40,}'
expect_match "P1-2 HttpOnly" "$setcookie" '; HttpOnly'
expect_match "P1-2 Secure" "$setcookie" '; Secure'
expect_match "P1-2 SameSite=Lax" "$setcookie" '; SameSite=Lax'
expect_match "P1-2 Path=/" "$setcookie" '; Path=/'

sid_a="$(sid_of "$A")"
keys="$(docker compose exec -T redis redis-cli --scan --pattern 'sess:*')"
expect_match "P1-3 セッションは Redis にある" "$keys" '^sess:[0-9a-f]{64}'
expect_no_match "P1-3 Redis のキーはセッション ID そのものではない（ハッシュ）" "$keys" "$sid_a"
ttl="$(docker compose exec -T redis redis-cli ttl "$(echo "$keys" | head -1)" | tr -d '\r')"
if [ "$ttl" -gt 0 ] 2>/dev/null; then ok "P1-3 セッションに TTL がある（${ttl}s）"; else ng "P1-3 セッションに TTL がある" "ttl=$ttl"; fi

session="$(cj "$A" "$APP/bff/session")"
expect_match "P2-4 /bff/session でユーザーが分かる" "$session" '"username":"alice"'
expect_no_match "P2-4 /bff/session にトークンが出ない" "$session" 'eyJ|access_token"|refresh_token'
# アプリ（app.localhost）の Cookie だけを見る。IdP のドメインの Cookie は IdP 自身のもの（Keycloak は JWT を入れる）
expect_no_match "P2-4 アプリの Cookie にトークンが出ない" "$(grep 'app.localhost' "$A")" 'eyJ'
expect_no_match "P2-4 コールバックの応答にトークンが出ない" "$(cat "$WORK/cb_headers")" 'eyJ'

# 評価役の指摘（2026-11-18）: path.Clean で "/\evil" に化ける return_to でも、自サイトの外へは戻さない
evil_jar="$WORK/evil.jar"
authz="$(cj "$evil_jar" -o /dev/null -w '%{redirect_url}' "$APP/bff/login?return_to=/./%5Cevil.example.com")"
cj "$evil_jar" -o "$WORK/page.html" "$authz"
cb="$(submit_login "$evil_jar" alice "$DEMO_PASSWORD")"
back="$(cj "$evil_jar" -o /dev/null -w '%{redirect_url}' "${cb#* }")"
expect "P2-2 ログイン後の戻り先は自サイト内だけ（オープンリダイレクトなし）" "$back" "${APP}/"

me="$(cj "$A" "$APP/api/me")"
expect_match "P2-5 ログイン後の /api/me は API まで届く" "$me" '"username":"alice"'

# 固定攻撃の対策: もう一度ログインすると（SSO で即通る）、新しい ID に変わり、古い ID は使えない
login_start "$A" >/dev/null
cj "$A" -o /dev/null "$(cj "$A" -o /dev/null -w '%{redirect_url}' "$(login_start "$A")")"
sid_a2="$(sid_of "$A")"
if [ -n "$sid_a2" ] && [ "$sid_a2" != "$sid_a" ]; then ok "P1-4 再ログインでセッション ID が変わる"; else ng "P1-4 再ログインでセッション ID が変わる"; fi
expect "P1-4 古いセッション ID は使えない" \
  "$(c -o /dev/null -w '%{http_code}' -H "Cookie: __Host-sid=$sid_a" "$APP/bff/session")" "401"

# API を直接たたく（backend ネットワークの使い捨てコンテナから。≒ 内部に入り込まれた状況）
api_with() { probe auth-sandbox_backend -H "Authorization: Bearer $1" http://api:8080/api/me; }
expect "P2-5 トークンなしは API が 401" "$(probe auth-sandbox_backend http://api:8080/api/me)" "401"
expect "P2-5 でたらめなトークンは 401" "$(api_with 'not-a-jwt')" "401"
none_jwt="$(printf '{"alg":"none","typ":"JWT"}' | openssl base64 -A | tr '+/' '-_' | tr -d '=').$(printf '{"sub":"x","aud":"sandbox-api","exp":9999999999}' | openssl base64 -A | tr '+/' '-_' | tr -d '=')."
expect "P2-5 alg=none の偽造トークンは 401" "$(api_with "$none_jwt")" "401"

if [ "$IDP" = "own" ]; then
  # e2e 自身がクライアント（BFF と同じ client_id/secret）になり、トークンを直接もらって境界を確かめる
  verifier="$(openssl rand -base64 48 | tr '+/' '-_' | tr -d '=\n' | cut -c1-64)"
  challenge="$(printf '%s' "$verifier" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
  T="$WORK/direct.jar"
  cj "$T" -o "$WORK/page.html" "$AUTH/authorize?client_id=sandbox-bff&redirect_uri=$(jq -rn --arg u "$APP/bff/callback" '$u|@uri')&response_type=code&scope=openid%20profile&state=e2e-state&nonce=e2e-nonce&code_challenge=$challenge&code_challenge_method=S256"
  code="$(submit_login "$T" alice "$DEMO_PASSWORD" | sed -nE 's/.*[?&]code=([^&]+).*/\1/p')"
  tok() { c -u "sandbox-bff:${BFF_CLIENT_SECRET}" "$AUTH/token" "$@"; }
  bad_pkce="$(tok -d grant_type=authorization_code -d "code=$code" -d "redirect_uri=$APP/bff/callback" -d "code_verifier=$(openssl rand -hex 32)")"
  expect_match "P2-7 code_verifier が違うと交換できない（PKCE）" "$bad_pkce" '"error":"invalid_grant"'
  # PKCE で失敗したコードも使用済みになる。新しいコードを取り直す
  cj "$T" -o "$WORK/page.html" -w '' "$AUTH/authorize?client_id=sandbox-bff&redirect_uri=$(jq -rn --arg u "$APP/bff/callback" '$u|@uri')&response_type=code&scope=openid%20profile&state=e2e-state&nonce=e2e-nonce&code_challenge=$challenge&code_challenge_method=S256" -D "$WORK/sso_headers"
  code="$(grep -i '^location:' "$WORK/sso_headers" | sed -nE 's/.*[?&]code=([^&]+).*/\1/p' | tr -d '\r')"
  tokens="$(tok -d grant_type=authorization_code -d "code=$code" -d "redirect_uri=$APP/bff/callback" -d "code_verifier=$verifier")"
  access="$(echo "$tokens" | jq -r .access_token)"; idtok="$(echo "$tokens" | jq -r .id_token)"; refresh="$(echo "$tokens" | jq -r .refresh_token)"
  expect "P2-7 認可コードは2回目の交換で失敗する" \
    "$(tok -d grant_type=authorization_code -d "code=$code" -d "redirect_uri=$APP/bff/callback" -d "code_verifier=$verifier" | jq -r .error)" "invalid_grant"
  expect "P2-5 正しいアクセストークンなら API は 200" "$(api_with "$access")" "200"
  expect "P2-5 ID トークン（aud が BFF）では API は 401" "$(api_with "$idtok")" "401"
  tampered="${access%.*}.$(printf 'x%.0s' {1..86})"
  expect "P2-5 署名を改ざんしたトークンは 401" "$(api_with "$tampered")" "401"
  r1="$(tok -d grant_type=refresh_token -d "refresh_token=$refresh" | jq -r .refresh_token)"
  expect "P3-1 リフレッシュトークンを2回使うと拒否（再利用検知）" \
    "$(tok -d grant_type=refresh_token -d "refresh_token=$refresh" | jq -r .error)" "invalid_grant"
  expect "P3-1 再利用を検知したら、同じ系統の新しいトークンも失効" \
    "$(tok -d grant_type=refresh_token -d "refresh_token=$r1" | jq -r .error)" "invalid_grant"
fi

expect "P2-6 bob（viewer）でもログインできる" "$(login_as "$B" bob)" "302"
expect "P2-6 bob は書き込み 403（API のロール認可）" "$(post_note "$B" "by bob" "$(csrf_of "$B")")" "403"
expect_match "P2-6 403 を返したのは API" "$(cat "$WORK/post.json")" '"rejected_by":"api"'
expect "P2-6 alice（editor）は書き込み 201" "$(post_note "$A" "by alice" "$(csrf_of "$A")")" "201"

########################################################################
section "Phase 3 — リフレッシュ・CSRF・ログアウト・SSO・層ごとの拒否"

csrf_a="$(csrf_of "$A")"
expect "P3-2 CSRF トークンなしの POST は 403" "$(post_note "$A" "no csrf")" "403"
expect_match "P3-2 403 を返したのは BFF" "$(cat "$WORK/post.json")" '"rejected_by":"bff"'
expect "P3-2 Origin が別サイトなら 403" "$(post_note "$A" "evil" "$csrf_a" "https://evil.example.com")" "403"
expect "P3-2 正しいトークンと Origin なら 201" "$(post_note "$A" "ok" "$csrf_a")" "201"

# auth は distroless（シェルなし）なので、コンテナの設定は docker inspect で読む
ttl_cfg="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$(docker compose ps -q auth)" 2>/dev/null | sed -n 's/^ACCESS_TOKEN_TTL=//p')"
if [ "$IDP" = "own" ] && [ "$ttl_cfg" = "15s" ]; then
  exp1="$(cj "$A" "$APP/bff/session" | jq -r .access_token_expires_at)"
  sleep 11   # 期限 15 秒・5 秒前にリフレッシュ → 10 秒を過ぎた後の API 呼び出しでリフレッシュされる
  expect "P3-1 期限間近でも API 呼び出しは成功する" "$(cj "$A" -o /dev/null -w '%{http_code}' "$APP/api/me")" "200"
  exp2="$(cj "$A" "$APP/bff/session" | jq -r .access_token_expires_at)"
  if [[ "$exp2" > "$exp1" ]]; then ok "P3-1 BFF がアクセストークンをリフレッシュした（$exp1 → ${exp2}）"; else ng "P3-1 BFF がリフレッシュした" "$exp1 → $exp2"; fi
else
  if [ "${E2E_REQUIRE_REFRESH:-0}" = "1" ]; then
    ng "P3-1 BFF のリフレッシュ" "ACCESS_TOKEN_TTL=15s で起動していない（make e2e-up で起動する）"
  else
    echo "  - P3-1 BFF のリフレッシュは ACCESS_TOKEN_TTL=15s で起動したときだけ確かめる（make e2e-up）"
  fi
fi

before="$(redis_sessions)"
revoked_before="$(docker compose exec -T auth-db psql -U auth -tAc 'select count(*) from refresh_tokens where revoked' 2>/dev/null | tr -d '\r')"
logout="$(cj "$A" -X POST -H "Origin: $APP" -H "X-CSRF-Token: $(csrf_of "$A")" "$APP/bff/logout")"
expect_match "P3-3 ログアウトで IdP のログアウト URL が返る" "$logout" '"logout_url":"https://'
expect_no_match "P3-3 ログアウト URL にトークンを含めない" "$logout" 'eyJ'
expect "P3-3 ログアウト後の /api/me は 401" "$(cj "$A" -o /dev/null -w '%{http_code}' "$APP/api/me")" "401"
after="$(redis_sessions)"
if [ "$after" -lt "$before" ]; then ok "P3-3 Redis のセッションが消えた（$before → ${after}）"; else ng "P3-3 Redis のセッションが消えた" "$before → $after"; fi
if [ "$IDP" = "own" ]; then
  revoked_after="$(docker compose exec -T auth-db psql -U auth -tAc 'select count(*) from refresh_tokens where revoked' | tr -d '\r')"
  if [ "$revoked_after" -gt "$revoked_before" ]; then ok "P3-3 リフレッシュトークンが失効した"; else ng "P3-3 リフレッシュトークンが失効した" "$revoked_before → $revoked_after"; fi
fi

# BFF だけログアウトした状態で、もう一度ログインを始める
if [ "$IDP" = "own" ]; then
  # 自作 IdP: /revoke はリフレッシュトークンだけを止め、SSO セッションは残す → フォームなしでログインが終わる
  sso="$(cj "$A" -o /dev/null -w '%{http_code} %{redirect_url}' "$(login_start "$A")")"
  expect_match "P3-4 IdP に SSO セッションがあればフォームを出さない" "$sso" "^302 ${APP}/bff/callback"
  cj "$A" -o /dev/null "${sso#* }"
else
  # Keycloak: リフレッシュトークンを失効させると、そのユーザーセッション（SSO）も終わる → フォームが出る
  expect "P3-4 Keycloak は /revoke で SSO セッションも終わる（フォームが出る）" \
    "$(cj "$A" -o /dev/null -w '%{http_code}' "$(login_start "$A")")" "200"
  login_as "$A" alice >/dev/null
fi
logout_url="$(cj "$A" -X POST -H "Origin: $APP" -H "X-CSRF-Token: $(csrf_of "$A")" "$APP/bff/logout" | jq -r .logout_url)"
expect_match "P3-4 IdP のログアウトはアプリに戻る" "$(cj "$A" -o /dev/null -w '%{http_code} %{redirect_url}' "$logout_url")" "^302 ${APP}/"
expect "P3-4 IdP からもログアウトすると、ログイン画面が出る" "$(cj "$A" -o /dev/null -w '%{http_code}' "$(login_start "$A")")" "200"

expect_match "P3-5 nginx で止まると rejected_by=nginx" "$(c "$APP/api/notes")" '"rejected_by":"nginx"'
expect_match "P3-5 BFF で止まると rejected_by=bff" "$(post_note "$B" "x")$(cat "$WORK/post.json")" '"rejected_by":"bff"'
expect_match "P3-5 API で止まると rejected_by=api" "$(post_note "$B" "x" "$(csrf_of "$B")")$(cat "$WORK/post.json")" '"rejected_by":"api"'

########################################################################
section "Phase 5 — L4 プロキシと BFF のスケールアウト"

L4="http://127.0.0.1:${L4_PORT:-9000}"
whoami="$(curl -s --max-time 5 "$L4/bff/whoami")"
expect_match "P5-1 L4 ポートは TCP をそのまま BFF に流す" "$whoami" '"served_by"'
expect_match "P5-1 L4 では X-Forwarded-For が付かない（HTTP を見ていない）" "$whoami" '"x_forwarded_for":""'
expect "P5-1 L4 はパスで振り分けない（/ も BFF に行き、静的ファイルは返らない）" \
  "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$L4/")" "404"
expect_match "P5-1 L4 では nginx の auth_request が効かないが、BFF の層で止まる" \
  "$(curl -s --max-time 5 "$L4/api/me")" '"rejected_by":"bff"'

if [ "$IDP" = "own" ]; then
  docker compose up -d --no-recreate --scale bff=3 --wait --wait-timeout 180 >/dev/null 2>&1 || {
    ng "P5-2 BFF の台数変更" "3台への変更・起動待ちに失敗"
    exit 1
  }
  sleep 6   # nginx の resolver（valid=5s）が新しい BFF の IP を引き直すのを待つ
  S="$WORK/scale.jar"
  login_as "$S" alice >/dev/null
  seen=""; codes=""
  for _ in $(seq 1 15); do
    res="$(cj "$S" -w '\n%{http_code}' "$APP/bff/session")"
    codes="$codes $(echo "$res" | tail -1)"
    seen="$seen $(echo "$res" | head -1 | jq -r .served_by)"
  done
  n="$(echo "$seen" | tr ' ' '\n' | grep -v '^$' | sort -u | wc -l | tr -d ' ')"
  if [ "$n" -ge 2 ]; then ok "P5-2 BFF 3台に振り分けられる（応答した台: ${n}）"; else ng "P5-2 BFF 3台に振り分けられる" "応答した台: $n"; fi
  expect_no_match "P5-2 どの台に当たっても同じセッションで 200（Redis で共有）" "$codes" '[^ ]*[^2 ][0-9][0-9]'
  api_codes="$(for _ in $(seq 1 6); do cj "$S" -o /dev/null -w '%{http_code} ' "$APP/api/me"; done)"
  expect_match "P5-2 どの台からでも API を呼べる" "$api_codes" '^(200 ){6}$'
  docker compose up -d --no-recreate --scale bff=1 --wait --wait-timeout 180 >/dev/null 2>&1 || {
    ng "P5-2 BFF の台数変更" "1台への変更・起動待ちに失敗"
    exit 1
  }
  sleep 6
fi

########################################################################
section "追加の境界検証 — ログと並行ログアウト"
marker="e2e-query-$(openssl rand -hex 12)"
c -o /dev/null "$APP/bff/callback?code=$marker&state=$marker"
# アクセスログには到達したパスが残り、クエリの認可コード/state は残らない。
access_logs="$(docker compose logs --no-color --no-log-prefix --since 1m nginx 2>/dev/null)"
expect_match "P3-7 コールバックのパスはアクセスログに残る" "$access_logs" '"uri":"/bff/callback"'
expect_no_match "P3-7 クエリの機密値はアクセスログに残らない" "$access_logs" "$marker"
unset access_logs

if docker build --target tests -t auth-sandbox-bff-tests services/bff >"$WORK/session-build.log" 2>&1; then
  if docker run --rm --network auth-sandbox_bff-data -e TEST_REDIS_ADDR=redis:6379 auth-sandbox-bff-tests >"$WORK/session-tests.log" 2>&1; then
    ok "P3-8 実 Redis でログアウトとリフレッシュが競合してもセッションは復活しない（期限切れ・トークン失効も確認）"
  else
    ng "P3-8 セッション競合テスト" "詳細: .e2e/session-tests.log"
  fi
else
  ng "P3-8 セッション競合テストのビルド" "詳細: .e2e/session-build.log"
fi

# レート制限は最後に（他のログインを巻き込まないように）
if [ "$IDP" = "own" ]; then
  codes="$(for _ in $(seq 1 35); do c -o /dev/null -w '%{http_code}\n' -X POST -d 'username=x&password=y' "$AUTH/login?client_id=x"; done | sort | uniq -c | tr '\n' ' ')"
  expect_match "P3-6 ログイン POST を連打すると 429（nginx の limit_req）" "$codes" ' 429'
fi

########################################################################
echo
echo "結果: ${pass} 件成功 / ${fail} 件失敗"
[ "$fail" -eq 0 ]
