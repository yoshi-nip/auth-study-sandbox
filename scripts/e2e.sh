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

published="$(docker compose ps --format json | jq -r 'select((.Publishers // []) | map(select(.PublishedPort > 0)) | length > 0) | .Service' | sort -u | tr '\n' ' ')"
expect_match "P0-4 ホストにポートを公開しているのは nginx だけ" "$published" '^nginx $'

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
echo
echo "結果: ${pass} 件成功 / ${fail} 件失敗"
[ "$fail" -eq 0 ]
