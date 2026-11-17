#!/bin/sh
# Claude Code の PostToolUse フック: 編集した .go ファイルに gofmt をかける（docs/harness.md H6）。
# 書式は人もAIも気にしなくてよいように、機械が直す。失敗しても編集自体は止めない。
f="$(jq -r '.tool_input.file_path // empty' 2>/dev/null)"
case "$f" in
  *.go) [ -f "$f" ] && command -v gofmt >/dev/null 2>&1 && gofmt -w "$f" ;;
esac
exit 0
