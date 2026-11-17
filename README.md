# auth-sandbox

Docker だけで、**リバースプロキシ（L7）→ BFF → API** の構成と、**自作の OpenID Connect 認証基盤**を手元に組み、
「どの層で・何を根拠に・リクエストを止めるのか」を動かして確かめるための学習用リポジトリです。

- ブラウザ ⇄ BFF は **ステートフル**（HttpOnly Cookie のセッション ID。中身は Redis）
- BFF ⇄ API は **ステートレス**（`Authorization: Bearer <JWT>`。API は JWKS で自分で検証）
- 認証基盤は Go で自作した最小の OIDC プロバイダ。**設定を差し替えるだけで Keycloak に置き換えられます**
- すべてのサービスは Go（標準の `net/http`）。外部に公開しているポートは nginx だけです

> 自作の認証基盤は学習用です。本番では Keycloak などの実績ある製品やマネージドサービスを使ってください。

## 構成

```mermaid
flowchart LR
  browser([ブラウザ])
  subgraph edge[edge ネットワーク]
    nginx[nginx<br/>L7: TLS 終端・Host/パスで振り分け<br/>auth_request・レート制限]
  end
  subgraph app[app ネットワーク（internal）]
    bff[BFF<br/>セッション・CSRF・トークン付与]
    auth[auth<br/>自作 OIDC プロバイダ]
  end
  subgraph backend[backend ネットワーク（internal）]
    api[API<br/>JWT 検証・ロール認可]
  end
  redis[(Redis<br/>セッション)]
  db[(PostgreSQL<br/>ユーザー・トークン)]

  browser -- "https://app.localhost:8443<br/>Cookie だけ" --> nginx
  browser -- "https://auth.localhost:8443<br/>ログイン画面" --> nginx
  nginx -- "/bff/* /api/*" --> bff
  nginx -- "auth.localhost" --> auth
  bff -- "Bearer JWT" --> api
  bff -- "コード交換・リフレッシュ<br/>（サーバー間）" --> auth
  api -- "JWKS（公開鍵）" --> auth
  bff --- redis
  auth --- db
```

| 層 | 何を見て止めるか | 止めたときの応答 |
|---|---|---|
| nginx（L7） | 知らないホスト名、セッションの有無（`auth_request`）、ログインの連打 | TLS 握手の拒否・421・444・`401 {"rejected_by":"nginx"}`・429 |
| BFF | セッション（Redis）、CSRF トークンと Origin、ログインの state / nonce / PKCE | `401 / 403 {"rejected_by":"bff"}` |
| API | JWT の署名・`iss`・`aud`・`exp`、ロール | `401 / 403 {"rejected_by":"api"}` |
| Docker ネットワーク | そもそも届くか（nginx → API、nginx → Redis は届かない） | 名前解決できない |

## 動かす

必要なもの: Docker（Compose v2）、`make`、`openssl`、`jq`、`curl`。Go は単体テストを手元で回すときだけ使います。

```bash
make setup     # .env（秘密はランダム生成）・ローカル証明書・git フック
```

```bash
make up        # 起動
```

ブラウザで https://app.localhost:8443 を開きます。デモユーザーは `alice`（editor）と `bob`（viewer）、パスワードは `.env` の `DEMO_PASSWORD` です。

- 証明書は自前の CA で署名しています。警告が出たら `nginx/certs/ca.crt` を OS に信頼させるか、警告を承認して進んでください（`mkcert` があれば自動でそちらを使います）
- `*.localhost` は `/etc/hosts` を編集しなくても `127.0.0.1` に解決されます（Chrome・Firefox・curl）

画面から「ログイン → API を呼ぶ → CSRF トークンなしで POST → ログアウト」を試すと、どの層が止めたかが `rejected_by` に出ます。

## 確かめる（完了の定義）

```bash
make verify    # gofmt・vet・機密検査・compose 構成・単体テスト・起動して E2E（80 件以上）
```

E2E（[`scripts/e2e.sh`](scripts/e2e.sh)）は「通るべきものが通る」だけでなく、**「止まるべきものが止まる」**を確かめます。
例: nginx から API には届かない、`alg=none` の偽造トークンは 401、ID トークン（aud が違う）では API を呼べない、
リフレッシュトークンを2回使うと系統ごと失効する、`return_to` で外部サイトへは戻らない、など。

## フェーズごとに追う

`git checkout phase-0` のように、タグで順に追えます。各フェーズの受け入れ基準は [docs/TASKS.md](docs/TASKS.md)。

| タグ | 内容 | 解説 |
|---|---|---|
| `phase-0` | nginx（L7）・静的フロント・ネットワーク分割 | [docs/01_network.md](docs/01_network.md) |
| `phase-3` | BFF のセッション・自作 OIDC・JWT 検証・リフレッシュ・CSRF・auth_request | [docs/02_bff_session.md](docs/02_bff_session.md)・[docs/03_oidc.md](docs/03_oidc.md) |
| `phase-4` | Keycloak への差し替え（`make e2e-keycloak`） | [docs/04_keycloak.md](docs/04_keycloak.md) |
| `phase-5` | L4（TCP）プロキシと BFF のスケールアウト | [docs/05_l4_and_scale.md](docs/05_l4_and_scale.md) |

## よく使うコマンド

| コマンド | 内容 |
|---|---|
| `make up` / `make down` / `make clean` | 起動 / 停止 / ボリュームも消す |
| `make logs` | ログ（nginx・BFF・API が同じ `request_id` で追える） |
| `make e2e-up` → `make e2e` | トークンの期限を 15 秒にして起動し、E2E（リフレッシュも確認） |
| `make scale` | BFF を3台に増やす |
| `make up-keycloak` / `make e2e-keycloak` | 認証基盤を Keycloak に差し替える |
| `NGINX_AUTH_REQUEST=off make up` | nginx の関門を外し、BFF・API の層だけで止まる様子を見る |

## AI と一緒に開発するためのハーネス

このリポジトリは AI（Claude Code など）に実装を任せる前提で、**間違えたら機械が止める仕組み**を先に用意しています。
入口は [AGENTS.md](AGENTS.md)、設計の意図は [docs/harness.md](docs/harness.md)。

- 完了の定義は `make verify` が緑であること（E2E が中心）
- コミット前に機密検査（秘密鍵・`.env`・JWT・個人のパスやメール）と gofmt（`.githooks/pre-commit`）
- `.claude/settings.json` で、検査の迂回や秘密の読み取りを禁止し、push などの外に出る操作は人の確認を求める
- 認証まわりの変更は、仕様だけを渡した独立の評価役（`.claude/agents/reviewer.md`）が確認する。実際にオープンリダイレクトを見つけています（[記録](docs/TASKS.md#評価役の記録)）
