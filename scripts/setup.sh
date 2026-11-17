#!/usr/bin/env bash
# 初回セットアップ: .env の生成（秘密はランダム値）、ローカル証明書、git フックの有効化。
# 何度実行しても既存の .env と証明書は上書きしない。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

if [ ! -f .env ]; then
  cp .env.example .env
  # change-me を、その場で作ったランダム値に置き換える（値は画面に出さない）
  while grep -q 'change-me' .env; do
    secret="$(openssl rand -hex 24)"
    # 1つずつ置き換える（BSD/GNU sed の両方で動く書き方）
    awk -v s="$secret" '!done && /change-me/ { sub(/change-me/, s); done=1 } { print }' .env > .env.tmp
    mv .env.tmp .env
  done
  chmod 600 .env
  echo "[setup] .env を作りました（秘密はランダム値）"
else
  echo "[setup] .env は既にあります（そのまま使います）"
fi

"$root/scripts/gen-certs.sh"

if git rev-parse --git-dir >/dev/null 2>&1; then
  git config core.hooksPath .githooks
  chmod +x .githooks/* scripts/*.sh scripts/hooks/*.sh
  echo "[setup] git フックを有効にしました（core.hooksPath=.githooks）"
fi
