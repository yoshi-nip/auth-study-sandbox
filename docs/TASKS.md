# TASKS — 今どこ・次に何

作業を1つ終えたら更新する。受け入れ基準の番号（例: `P1-3`）は `scripts/e2e.sh` のチェックの見出しと対応する。

## 今どこ

- [x] Phase 0: Nginx（L7）＋静的フロント＋Go API、ネットワーク分割
- [x] Phase 1: BFF＋Redis セッション
- [x] Phase 2: 自作 OIDC（認可コード＋PKCE）、API は JWKS で検証
- [x] Phase 3: リフレッシュ・ログアウト・CSRF・nginx auth_request・レート制限・TLS
- [x] Phase 4: Keycloak への差し替え
- [ ] Phase 5: L4 プロキシ・BFF のスケールアウト・README

## 受け入れ基準

### Phase 0 — L7 ルーティングと境界
- P0-1 `http://` は `https://` へ 301 で転送される
- P0-2 `https://app.localhost` で静的フロントが返り、セキュリティヘッダ（CSP・X-Frame-Options・nosniff）が付く
- P0-3 知らない Host 名には応答しない（default_server で切る）
- P0-4 ホストにポートを公開しているのは nginx だけ
- P0-5 nginx から API・Redis・DB へはネットワーク的に届かない（名前も引けない）
- P0-6 内部ネットワーク（app / backend / bff-data / auth-data）は `internal: true`

### Phase 1 — BFF とセッション
- P1-1 未ログインで `/api/*` は 401
- P1-2 ログインでセッション Cookie `__Host-sid` が `HttpOnly; Secure; SameSite=Lax; Path=/` で付く
- P1-3 セッションの中身は Redis にあり、TTL が付いている
- P1-4 ログイン前に持っていたセッション ID はログイン後に使えない（固定攻撃の対策）

### Phase 2 — 自作 OIDC と JWT
- P2-1 `/bff/login` は PKCE(S256)・state・nonce 付きで認可エンドポイントへ転送する
- P2-2 誤ったパスワードではログインできない。登録されていない redirect_uri には転送しない
- P2-3 state が合わないコールバックは 400
- P2-4 ブラウザに返るもの（本文・Cookie）にトークンが含まれない
- P2-5 API は署名を JWKS で検証する。トークンなし・改ざん・別 audience は 401
- P2-6 ロールで認可する（bob=viewer は書き込み 403、alice=editor は 201）
- P2-7 認可コードは一回しか使えない

### Phase 3 — 運用で効く部分
- P3-1 アクセストークンの期限が近づくと、BFF がリフレッシュする（ブラウザは何もしない）
- P3-2 状態を変えるリクエストは CSRF トークンと Origin が合わないと 403
- P3-3 ログアウトで Redis のセッションが消え、以後 401。リフレッシュトークンも失効する
- P3-4 IdP の SSO セッションがあれば再ログインでフォームを出さない。IdP からもログアウトすると再びフォームが出る
- P3-5 どの層で止めたかが分かる（nginx の auth_request／BFF／API）
- P3-6 ログイン POST にレート制限（429）

### Phase 4 — Keycloak
- P4-1 `make up-keycloak` で、BFF と API のコードを変えずに Keycloak でログイン〜API〜ログアウトができる

### Phase 5 — L4 とスケール
- P5-1 L4（stream）ポートは TCP をそのまま流すだけ: パスで振り分けず、nginx の auth_request も効かない（BFF の層で止まる）
- P5-2 BFF を3台にしても、同じセッションでどの台に当たっても API を呼べる

## 決めたこと

- サービスは Go の別モジュール（`services/api`・`services/bff`・`services/auth`）。ルートの `go.work` で束ねる
- 認証基盤は「最小の OIDC プロバイダ」として自作し、Keycloak と差し替えられる形にする
- 署名は ES256（自作）。Keycloak は RS256。API は両方を受け付ける（アルゴリズムは許可リストで固定）
- トークンの検証は `coreos/go-oidc` を使う（検証は自作しない。発行側だけ自作して学ぶ）
- ホスト名は `*.localhost`（`/etc/hosts` 不要）。ブラウザから見える URL（issuer）とコンテナ間の URL を分けて設定する

## 評価役の記録

### 2026-11-18 Phase 1〜3（渡した仕様: 受け入れ基準 P0〜P3・BFF パターンの不変条件・OIDC の要件・ポート公開）

| 指摘 | 重大度 | 対応 |
|---|---|---|
| BFF の `return_to` にオープンリダイレクト（`/./\evil` が `http.Redirect` の path.Clean で `/\evil` に化ける） | 中 | 修正。バックスラッシュ・制御文字を拒否し、Clean 後の値で検査して返す。単体テストと E2E（P2-2）を追加 |
| Keycloak 構成で `id_token_hint` によりログアウト URL に ID トークンが載る | 中 | 修正。`id_token_hint` を使うオプションを削除。Keycloak は `/revoke` で SSO セッションも終わるため不要だった。E2E（P3-3）を両 IdP で実行 |
| IdP の `GET /logout` は呼び出し元を確かめない（他サイトから IdP をログアウトさせられる） | 低 | 残す。理由は docs/03 の「省いたもの」 |
| SNI は正しいが Host が知らない名前の場合を検査していない | 低 | 確認・追加。nginx は 421 で断っていた。:80 の知らない Host は 444 にした。E2E（P0-3）を追加 |
| リフレッシュトークンの期限がローテーションのたびに延びる | 低 | 修正。系統の期限を最初のログインから延ばさない |
| 再利用された認可コードから発行済みのトークンを失効させない | 低 | 残す（RFC 6749 では SHOULD）。docs/03 に記載 |

自己レビューでは見つけられなかった種類（ライブラリの内部処理で値が化ける）だった。
