# 04 Keycloak への差し替え

```bash
make e2e-keycloak   # Keycloak で起動して E2E（ログイン〜API〜ログアウト）
```

```bash
make down-keycloak && make up   # 自作の認証基盤に戻す
```

[`compose.keycloak.yaml`](../compose.keycloak.yaml) は、Keycloak を足し、BFF と API の**環境変数（URL）だけ**を上書きします。
BFF と API は OIDC・JWT・JWKS という標準にしか依存していないので、コードは一行も変わりません。
Realm（クライアント・ロール・デモユーザー）は [`keycloak/realm-sandbox.json`](../keycloak/realm-sandbox.json) から起動時に取り込みます。
管理画面は https://kc.localhost:8443/admin （admin / `DEMO_PASSWORD`）。

## 自作の認証基盤と同じ形にするための設定

| やりたいこと | Keycloak での設定 |
|---|---|
| PKCE を必須にする（S256） | クライアント属性 `pkce.code.challenge.method: S256` |
| アクセストークンの `aud` に `sandbox-api` を入れる | Audience マッパー（既定では `aud` に API の名前が入らない） |
| ロールを `roles` クレームに出す | Realm ロールのマッパー（既定は `realm_access.roles` の入れ子） |
| リフレッシュトークンのローテーション | Realm の `revokeRefreshToken: true` |
| ブラウザ向けとコンテナ間の URL を分ける | `KC_HOSTNAME`（外向き・`iss`）と `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true` |
| nginx の後ろで動かす | `KC_PROXY_HEADERS=xforwarded` |

## 動かしてみて分かった違い

- **リフレッシュトークンを失効させると、SSO セッションも終わる**。自作の認証基盤は失効させても SSO は残す（もう一度ログインするとフォームが出ない）。E2E の P3-4 はこの違いを両方の IdP で確かめています
- 署名は RS256（自作は ES256）。API は許可リストで両方を受け付けます
- Keycloak は自分のドメイン（kc.localhost）の Cookie に JWT を入れます。これは IdP 自身のセッションで、アプリ（app.localhost）の Cookie にトークンが出ないことは変わりません
- 管理画面・ブルートフォース検知・多要素認証・外部 IdP 連携など、自作では省いたものがそろっています。JVM なので起動に数十秒かかり、メモリも食います
