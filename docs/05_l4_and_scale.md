# 05 L4 プロキシとスケールアウト

## L4（`nginx/stream.d/l4.conf`）と L7 の違い

nginx の `stream` ブロックは、TCP のバイト列をそのまま BFF に流します（AWS の NLB の立場）。`http://127.0.0.1:9000` で試せます。

```bash
curl http://127.0.0.1:9000/bff/whoami   # 届く。ただし x_forwarded_for は空、remote_addr は nginx
```

```bash
curl https://app.localhost:8443/bff/whoami --cacert nginx/certs/ca.crt   # L7: x_forwarded_for にクライアントが入る
```

```bash
curl http://127.0.0.1:9000/api/me   # 401 rejected_by=bff（nginx の auth_request は効かない）
```

| | L7（http） | L4（stream） |
|---|---|---|
| 振り分けの根拠 | Host・パス・ヘッダ | IP・ポートだけ |
| TLS | 終端して中身を見る | そのまま流す（または終端だけ） |
| auth_request・ヘッダ付与・レート制限（リクエスト単位） | できる | できない |
| 本当のクライアント IP | `X-Forwarded-For` | 分からない（PROXY protocol が必要） |
| 向いている用途 | Web・API | DB・独自プロトコル・TLS パススルー・低遅延 |

L4 から入ると nginx の関門は素通りになりますが、BFF の層で 401 になります。
**入口の1か所に頼らず、各層が自分で確かめる（多層防御）**ことの意味がここで見えます（E2E P5-1）。

## スケールアウト

```bash
make scale   # BFF を3台に
```

- nginx の upstream は `server bff:8080 resolve;`。Docker の DNS が返す3台の IP を引き直して振り分けます（ラウンドロビン）
- セッションは Redis にあるので、どの台に当たっても同じログイン状態です（**スティッキーセッションが要らない**）
- 画面の「応答した BFF」がリクエストごとに変わるのを見てください
- アクセストークンのリフレッシュは、台をまたいでも Redis のロックで1回だけになります（[02](02_bff_session.md)）

E2E の P5-2 は、3台に増やしてから同じ Cookie で15回アクセスし、2台以上に振り分けられ、すべて 200 であることを確かめています。
