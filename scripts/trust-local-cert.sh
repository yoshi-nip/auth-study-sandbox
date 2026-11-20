#!/usr/bin/env bash
# ローカルサーバーの証明書だけを、現在の macOS ユーザーの SSL 用に信頼登録する。
# CA 全体は登録しない。OS の承認画面はユーザーが操作する。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
if [ "$(uname -s)" != Darwin ]; then
  echo 'このコマンドは macOS 用です。OS の証明書管理を使ってください。' >&2
  exit 1
fi
if [ ! -f "$root/nginx/certs/server.crt" ]; then
  echo '先に make setup で証明書を生成してください。' >&2
  exit 1
fi
security add-trusted-cert -r trustAsRoot -p ssl -k "$HOME/Library/Keychains/login.keychain-db" "$root/nginx/certs/server.crt"
for host in app.localhost auth.localhost kc.localhost; do
  security verify-cert -c "$root/nginx/certs/server.crt" -p ssl -s "$host"
done
printf '%s\n' 'サーバー証明書の信頼登録を確認しました。ブラウザで再読み込みしてください。'
