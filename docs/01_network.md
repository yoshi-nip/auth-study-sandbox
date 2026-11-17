# 01 ネットワークと L7 リバースプロキシ

## AWS に置き換えると

| このリポジトリ | AWS でいうと |
|---|---|
| nginx の `http` ブロック（Host・パスで振り分け、TLS 終端） | ALB（Application Load Balancer）。リスナールール |
| nginx の `stream` ブロック（TCP をそのまま流す） | NLB（Network Load Balancer）→ [05](05_l4_and_scale.md) |
| Docker ネットワーク `edge` / `app` / `backend` / `bff-data` / `auth-data` | サブネット＋セキュリティグループ |
| `internal: true` | NAT ゲートウェイもインターネットゲートウェイもないプライベートサブネット |
| `ports:` を持つのは nginx だけ | パブリックなのは ALB だけ。EC2・RDS・ElastiCache はプライベート |

## 誰が誰に届くか

同じネットワークに入っているコンテナどうしだけが、サービス名（`api` など）で通信できます。

| from ＼ to | bff | api | auth | redis | auth-db |
|---|---|---|---|---|---|
| nginx | ○（app） | ×（届かない） | ○（app） | × | × |
| bff | ― | ○（backend） | ○（app） | ○（bff-data） | × |
| api | × | ― | ○（backend・JWKS 取得） | × | × |
| auth | ○ | ○ | ― | × | ○（auth-data） |

E2E の P0-5 は、`curlimages/curl` を**特定のネットワークにだけ**つないで実行し、「app から API は 000（届かない）、backend からは 200」を確かめています。セキュリティグループを手で試すのと同じ感覚です。

## nginx で押さえるところ（`nginx/templates/default.conf.template`）

- **TLS 終端**: 証明書は nginx だけが持つ。内部は HTTP。`X-Forwarded-Proto` で「元は https だった」と伝える
- **Host による振り分け**: `app.localhost`・`auth.localhost`・`kc.localhost` を1つの IP・ポートで受ける（ALB のホストベースルーティング）
- **知らないホストは断る**: SNI が知らない名前なら TLS の握手で拒否（`ssl_reject_handshake`）。SNI と Host が食い違えば 421、:80 は 444（無言で切断）
- **パスによる振り分け**: `/` は nginx 自身が静的ファイルを返し、`/bff/` と `/api/` は BFF へ
- **同一オリジンにまとめる**: フロントと API が同じ `https://app.localhost:8443` なので **CORS の設定が要らない**。リバースプロキシを置く大きな理由の1つ
- **セキュリティヘッダ**: CSP・X-Frame-Options など。`add_header` は「その階層で1つも書いていないときだけ親から引き継ぐ」ので、location の中では書かない
- **`resolve` と resolver**: upstream の名前を起動後も引き直す。BFF を増やすと自動で振り分け先が増え、Keycloak のように後から起動するサービスにもつながる
- **request_id**: nginx が作った ID を `X-Request-ID` で BFF・API に渡す。`make logs` で同じ ID を grep すると、1つのリクエストが層を渡っていく様子が見える
