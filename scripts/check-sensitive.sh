#!/usr/bin/env bash
# 公開リポジトリに載せてはいけないものを検査する（docs/harness.md H6）。
#   --staged : ステージした版（コミットに入る中身）を検査する。pre-commit から呼ぶ
#   --all    : 追跡中の全ファイルを検査する。CI と make verify から呼ぶ
# 形で見つけられるものだけ。実名のように形で見分けられないものは人が見る。
set -euo pipefail

mode="${1:---all}"
root="$(git rev-parse --show-toplevel)"
cd "$root"

if [ "$mode" = "--staged" ]; then
  files="$(git diff --cached --name-only --diff-filter=ACMR)"
  show() { git show ":$1"; }
  fsize() { git cat-file -s ":$1"; }
else
  files="$(git ls-files --cached --others --exclude-standard)"
  show() { cat "$1"; }
  fsize() { wc -c < "$1" | tr -d ' '; }
fi
# 先頭 64KB に NUL があればバイナリ。grep -q が先に終わっても誤判定しないよう、ここだけ pipefail を外す
is_text() { ( set +o pipefail; show "$1" 2>/dev/null | head -c 65536 | LC_ALL=C grep -Iq . ); }

fail=0
report() { echo "  ✗ $1: $2" >&2; fail=1; }

while IFS= read -r f; do
  [ -z "$f" ] && continue
  base="$(basename "$f")"

  # 1) 置いてはいけないファイル名
  case "$base" in
    .env|.env.local|*.pem|*.key|*.p12|*.pfx|id_rsa*|id_ed25519*)
      report "$f" "秘密を含むファイル名（.env.example 以外の .env・鍵・証明書）"; continue ;;
  esac

  # 検査スクリプト自身は中身を見ない
  [ "$f" = "scripts/check-sensitive.sh" ] && continue
  if [ "$mode" != "--staged" ] && [ ! -f "$f" ]; then continue; fi
  size="$(fsize "$f")"
  [ "$size" -eq 0 ] && continue

  # 2) バイナリは画像だけ許す。手元でビルドした実行ファイルには PC の絶対パス（ユーザー名）が埋め込まれる
  if ! is_text "$f"; then
    case "$base" in
      *.png|*.gif|*.jpg|*.jpeg|*.webp|*.ico)
        [ "$size" -gt 3145728 ] && report "$f" "画像が大きすぎる（3MB 超）" ;;
      *)
        report "$f" "画像以外のバイナリ（ビルドした実行ファイルなど。ビルドはコンテナの中で行う）" ;;
    esac
    continue
  fi
  content="$(show "$f" 2>/dev/null || true)"

  # 3) 中身の形
  printf '%s' "$content" | grep -nE -- '-----BEGIN ([A-Z]+ )?PRIVATE KEY-----' >/dev/null \
    && report "$f" "秘密鍵"
  printf '%s' "$content" | grep -nE 'AKIA[0-9A-Z]{16}' >/dev/null \
    && report "$f" "AWS アクセスキーらしき文字列"
  printf '%s' "$content" | grep -nE 'gh[pousr]_[A-Za-z0-9]{36}' >/dev/null \
    && report "$f" "GitHub トークンらしき文字列"
  printf '%s' "$content" | grep -nE 'eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.' >/dev/null \
    && report "$f" "JWT（実際のトークン）らしき文字列"
  printf '%s' "$content" | grep -nE '/Users/[A-Za-z0-9._-]+/|/home/[a-z][A-Za-z0-9._-]+/' >/dev/null \
    && report "$f" "個人の絶対パス（ユーザー名が漏れる）"
  emails="$(printf '%s' "$content" | grep -oE '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' \
    | grep -vE '@(example\.(com|org|net)|anthropic\.com|users\.noreply\.github\.com)$' || true)"
  [ -n "$emails" ] && report "$f" "example.com 以外のメールアドレス: $(echo "$emails" | head -3 | tr '\n' ' ')"
done <<< "$files"

if [ "$fail" -ne 0 ]; then
  echo "[check-sensitive] 公開してはいけない内容が見つかりました。取り除いてから進めてください。" >&2
  exit 1
fi
echo "[check-sensitive] OK ($mode)"
