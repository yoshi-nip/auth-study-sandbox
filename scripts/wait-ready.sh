#!/usr/bin/env bash
# BFF の再作成後、nginx が Docker DNS を引き直して API に届くまで待つ。
# nginx 自身の静的な healthz だけでは、まだ古い BFF の IP を参照していても成功してしまう。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
published="$(docker compose port nginx 443)"
port="${published##*:}"
for _ in $(seq 1 30); do
  if curl -fsS --max-time 2 --cacert nginx/certs/ca.crt \
    --resolve "app.localhost:${port}:127.0.0.1" \
    "https://app.localhost:${port}/api/public/hello" 2>/dev/null | jq -e '.message == "hello from api"' >/dev/null; then
    echo '[ready] nginx → BFF → API の応答を確認しました'
    exit 0
  fi
  sleep 1
done
echo '[ready] nginx → BFF → API が応答しません。起動ログを確認してください。' >&2
exit 1
