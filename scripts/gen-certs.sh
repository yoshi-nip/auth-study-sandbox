#!/usr/bin/env bash
# ローカル用の TLS 証明書を作る（nginx/certs/。git には載せない）。
#   mkcert があれば mkcert（OS に信頼されたローカル CA）を使う。
#   なければ openssl で「自前の CA」＋「その CA が署名したサーバー証明書」を作る。
#   後者の場合、ブラウザは警告を出す。nginx/certs/ca.crt を OS に信頼させるか、警告を承認して進む。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
dir="$root/nginx/certs"
mkdir -p "$dir"
names=(app.localhost auth.localhost kc.localhost localhost)

if [ -f "$dir/server.crt" ] && [ -f "$dir/server.key" ]; then
  echo "[certs] 既にあります（作り直すときは nginx/certs/ を消してから）"
  exit 0
fi

if command -v mkcert >/dev/null 2>&1; then
  mkcert -cert-file "$dir/server.crt" -key-file "$dir/server.key" "${names[@]}"
  cp "$(mkcert -CAROOT)/rootCA.pem" "$dir/ca.crt"
  echo "[certs] mkcert で作りました"
  exit 0
fi

san="$(printf 'DNS:%s,' "${names[@]}")IP:127.0.0.1"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
  -subj "/CN=auth-sandbox local CA" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -keyout "$dir/ca.key" -out "$dir/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -subj "/CN=app.localhost" -keyout "$dir/server.key" -out "$dir/server.csr" 2>/dev/null
openssl x509 -req -in "$dir/server.csr" -CA "$dir/ca.crt" -CAkey "$dir/ca.key" -CAcreateserial \
  -days 825 -out "$dir/server.crt" \
  -extfile <(printf "subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature" "$san") 2>/dev/null
rm -f "$dir/server.csr" "$dir/ca.srl"
chmod 644 "$dir/server.key"   # nginx コンテナ（別ユーザー）から読めるように。ローカル専用の鍵
echo "[certs] openssl で自前 CA とサーバー証明書を作りました（nginx/certs/ca.crt を信頼させると警告が消えます）"
