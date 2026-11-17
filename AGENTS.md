# auth-sandbox — エージェント向けの入口

Docker で Nginx（L7 リバースプロキシ）・BFF・API・自作 OIDC 認証基盤・Redis・PostgreSQL を組み、
「どの層で何を止めるか」を手元で確かめる学習用の**公開**リポジトリ。サービスはすべて Go（`services/`）。

## 先に読む

- 今どこ・次に何・各フェーズの受け入れ基準: [`docs/TASKS.md`](docs/TASKS.md)（作業を1つ終えたら更新する）
- 構成と学べること: [`README.md`](README.md)、各論は `docs/NN_*.md`
- ハーネス（機械で止めていること・評価役・無人で進めるときの止め方）: [`docs/harness.md`](docs/harness.md)

## 完了の定義（「できた」はこれで言う）

- `make verify` が緑（gofmt・go vet・単体テスト・機密検査・compose 構成・E2E）。結果のコマンドと出力で報告する
- フェーズの受け入れ基準（TASKS.md）は、**対応する E2E を `scripts/e2e.sh` に足してから**完了にする
- 認証・セッション・トークン・Cookie・ネットワーク境界を変えたら、評価役（`.claude/agents/reviewer.md`）に仕様だけ渡して確認する

## 機械で止めていること（外さない・回り道をしない）

- コミット前: 秘密鍵・`.env`・トークンらしき文字列・個人のパスやメール・gofmt 漏れ（`.githooks/pre-commit`。`make setup` で有効）
- `.claude/settings.json`: `--no-verify`・フックの付け替え・`.env` と鍵の読み取りは deny、push と破壊的操作は ask
- Go ファイルを編集すると gofmt が自動で走る（PostToolUse フック）
- 止められたら、検査やフックを外す方法を探さず、原因を直すか人に聞く

## 守ること

- 公開リポジトリ。実在の個人情報・本物の秘密を入れない。デモの資格情報は `.env.example` の値だけ
- **ホストに公開するポートは nginx だけ**。他のサービスに `ports:` を足さない（E2E が検査する）
- トークンはブラウザに渡さない（BFF パターンの前提。E2E が検査する）
- テストを消す・弱めることで緑にしない。2回直して緑にならなければ止まり、原因を TASKS.md に書く
- 自作の認証は学習用。ただし「学習用だから」で安全側の実装を省かない（省くなら docs に理由を書く）

## コマンド

`make setup`（初回: .env・証明書・git フック）／`make up`／`make verify`／`make e2e`／`make down`
