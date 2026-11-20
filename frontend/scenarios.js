// 通信の流れ（ステップ列）を組み立てる。画面には依存しない純粋なデータ。
//
// 1ステップ = 1本の矢印。
//   from/to : 役者の id（stage.js の ACTORS）
//   kind    : 'req'（行き）| 'res'（戻り）| 'round'（行って戻る。Redis・DB の読み書き）| 'self'（その場での判定）
//   tone    : 'req' | 'ok' | 'err' | 'info'（色）
//   chip    : 運ばれるもの（パケットの横に出す短い文字）
//   proto   : HTTPS | HTTP | Redis | SQL | JS
//   title / body / kv : 右の解説パネルに出す。本文の `…` はコード表示になる
//   reject  : true なら、この役者が止めた（赤く表示する）
//
// API 呼び出し・ログアウトは「実際に返ってきた結果」から組み立てる。
// 返ってきたステータスと rejected_by で、どこまで進んでどこで止まったかを決める。
'use strict';

const step = (o) => ({ kind: 'req', tone: 'req', kv: [], ...o });

function statusTone(status) {
  return status >= 200 && status < 400 ? 'ok' : 'err';
}

function responseKv(status, data) {
  const kv = [['ステータス', String(status)]];
  if (data && typeof data === 'object') {
    if (data.rejected_by) kv.push(['rejected_by', data.rejected_by]);
    if (data.error) kv.push(['error', data.error]);
    if (data.detail) kv.push(['detail', data.detail]);
    if (data.served_by) kv.push(['served_by', data.served_by]);
  }
  return kv;
}

// 戻りの3本（API → BFF → nginx → ブラウザ のうち、指定した役者から先）
function returnPath(from, status, data, { reject = false } = {}) {
  const tone = statusTone(status);
  const steps = [];
  const chip = `${status}${data && data.rejected_by ? ` rejected_by: ${data.rejected_by}` : ''}`;
  if (from === 'api') {
    steps.push(step({
      from: 'api', to: 'bff', kind: 'res', tone, proto: 'HTTP', chip,
      summary: `${status} を BFF へ返す`,
      title: tone === 'ok' ? 'API が結果を返す' : 'API がエラーを返す',
      body: 'API は処理結果（または拒否の理由）を JSON で BFF に返します。',
      kv: responseKv(status, data),
    }));
  }
  if (from === 'api' || from === 'bff') {
    steps.push(step({
      from: 'bff', to: 'nginx', kind: 'res', tone, proto: 'HTTP', chip, reject: reject && from === 'bff',
      summary: `${status} を nginx へ返す`,
      title: 'BFF が応答を返す',
      body: 'BFF は応答を nginx に返します。トークンは本文にも Cookie にも入れません（BFF パターンの前提）。',
      kv: responseKv(status, data),
    }));
  }
  steps.push(step({
    from: 'nginx', to: 'browser', kind: 'res', tone, proto: 'HTTPS', chip,
    summary: `${status} をブラウザへ返す`,
    title: tone === 'ok' ? 'ブラウザに結果が届く' : 'ブラウザにエラーが届く',
    body: tone === 'ok'
      ? 'nginx が TLS で暗号化してブラウザに返します。画面の JavaScript が受け取るのは JSON だけで、トークンは含まれません。'
      : '拒否した層は `rejected_by` で分かります。どの層で止まったかを見比べてみてください。',
    kv: responseKv(status, data),
  }));
  return steps;
}

// ---- API 呼び出し -------------------------------------------------------------

/**
 * @param {object} c
 * @param {string} c.method
 * @param {string} c.path
 * @param {number} c.status    実際のステータス
 * @param {any}    c.data      実際の本文
 * @param {boolean} c.hadSession 呼び出し前にログインしていたか
 * @param {boolean} c.csrfSent  X-CSRF-Token を付けたか
 * @param {boolean} c.refreshed 呼び出しの間にアクセストークンが更新されたか
 */
export function apiCall(c) {
  const { method, path, status, data } = c;
  const by = data && typeof data === 'object' ? data.rejected_by : undefined;
  const isPublic = path.startsWith('/api/public/');
  const write = method !== 'GET';
  const steps = [];

  // 1. ブラウザ → nginx
  const sends = [];
  if (!isPublic) sends.push(c.hadSession ? 'Cookie `__Host-sid`（ブラウザが自動で付ける）' : 'Cookie なし（未ログイン）');
  if (write) sends.push(c.csrfSent ? '`X-CSRF-Token`・`Origin`' : '`Origin` のみ（`X-CSRF-Token` なし）');
  let body1 = `\`fetch('${path}')\` は同一オリジン宛てなので、Cookie はブラウザが自動で付けます。画面の JavaScript は Cookie を読めず、トークンも持っていません。`;
  if (write && !c.csrfSent) body1 += ' 今回はわざと `X-CSRF-Token` を付けずに送ります（よそのサイトから送られた偽のリクエストの真似）。';
  steps.push(step({
    from: 'browser', to: 'nginx', proto: 'HTTPS',
    chip: isPublic ? `${method} ${path}` : (c.hadSession ? 'Cookie: __Host-sid' : 'Cookie なし'),
    summary: `${method} ${path}`,
    title: 'ブラウザがリクエストを送る',
    body: body1,
    kv: [['宛先', `https://app.localhost${path}`], ...(sends.length ? [['送るもの', sends.join(' / ')]] : []), ['持っていないもの', 'アクセストークン']],
  }));

  // 公開 API: auth_request なしで BFF → API
  if (isPublic) {
    steps.push(step({
      from: 'nginx', to: 'bff', proto: 'HTTP', chip: `${method} ${path}`,
      summary: 'BFF へ転送（認証の確認なし）',
      title: 'nginx が BFF へ転送する',
      body: '`location /api/public/` には `auth_request` がないので、そのまま BFF に渡します。nginx から API へはネットワーク的に届かないので、公開 API でも BFF を経由します。',
      kv: [['nginx の設定', 'location /api/public/ → proxy_pass http://bff']],
    }));
    if (status === 502) {
      steps.push(step({ from: 'bff', to: 'api', proto: 'HTTP', tone: 'err', chip: '届かない', summary: 'API に届かない', title: 'API に届かない', body: 'BFF から API へ接続できませんでした。', reject: true }));
      return steps.concat(returnPath('bff', status, data));
    }
    steps.push(step({
      from: 'bff', to: 'api', proto: 'HTTP', chip: 'Authorization なし',
      summary: 'API へ転送（トークンなし）',
      title: 'BFF が API へ転送する',
      body: '公開 API なのでトークンは付けません。API も署名の検証をせずに応答します。',
      kv: [['ネットワーク', 'backend（internal）']],
    }));
    return steps.concat(returnPath('api', status, data));
  }

  const nginxSkipped = by === 'bff' && status === 401; // auth_request を off にしている、または確認の直後に期限切れ

  // 2〜4. nginx の auth_request
  if (!nginxSkipped) {
    steps.push(step({
      from: 'nginx', to: 'bff', proto: 'HTTP', chip: 'GET /_auth',
      summary: 'auth_request でセッションを問い合わせ',
      title: 'nginx が BFF に「通してよいか」を聞く',
      body: '`location /api/` には `auth_request` があり、本体を渡す前に BFF の `/bff/session/check` へサブリクエストを送ります。本文は送らず、Cookie だけで判定させます。',
      kv: [['サブリクエスト', '/_auth → /bff/session/check'], ['本文', '送らない（proxy_pass_request_body off）']],
    }));
    steps.push(step({
      from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: c.hadSession ? 'GET sess:… → あり' : 'Cookie なし',
      summary: 'セッションの有無を確認',
      title: 'BFF が Redis でセッションを探す',
      body: c.hadSession
        ? 'Cookie のセッション ID をキーに Redis を引きます。期限（TTL）が切れていれば見つかりません。'
        : 'Cookie がないので、探すまでもなくセッションなしです。',
      kv: [['見るもの', 'セッションの存在と期限']],
    }));
    if (by === 'nginx') {
      steps.push(step({
        from: 'bff', to: 'nginx', kind: 'res', tone: 'err', proto: 'HTTP', chip: '401',
        summary: 'セッションなし → 401',
        title: 'BFF が「通さない」と答える',
        body: '`/bff/session/check` は本文なしで 401 を返すだけです。',
      }));
      steps.push(step({
        from: 'nginx', to: 'browser', kind: 'res', tone: 'err', proto: 'HTTPS', chip: '401 rejected_by: nginx', reject: true,
        summary: 'nginx が 401 を返す（第1の関門）',
        title: 'nginx で止まる（第1の関門）',
        body: 'nginx は `error_page 401 = @unauthenticated` で自分で 401 を返します。リクエスト本体は BFF にも API にも届いていません。',
        kv: responseKv(status, data),
      }));
      return steps;
    }
    steps.push(step({
      from: 'bff', to: 'nginx', kind: 'res', tone: 'ok', proto: 'HTTP', chip: '204',
      summary: 'セッションあり → 204',
      title: 'BFF が「通してよい」と答える',
      body: '204 が返ると、nginx はリクエスト本体の転送に進みます。',
    }));
  }

  // 5. 本体の転送
  steps.push(step({
    from: 'nginx', to: 'bff', proto: 'HTTP', chip: `${method} ${path}`,
    summary: '本体を BFF へ転送',
    title: nginxSkipped ? 'nginx が BFF へ転送する（関門なし）' : 'nginx が本体を BFF へ転送する',
    body: nginxSkipped
      ? 'nginx の関門で止まらずに BFF まで届きました（`NGINX_AUTH_REQUEST=off` にしているか、確認の直後にセッションが切れた）。BFF は自分でもセッションを確かめます。'
      : 'API は backend ネットワークにだけいるので、nginx から API へは届きません。必ず BFF を経由します。',
    kv: [['nginx から API', '届かない（ネットワークが別）']],
  }));

  // 6. BFF がセッションを読み直す
  steps.push(step({
    from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: 'GET sess:…',
    summary: 'セッションとトークンを読む',
    title: 'BFF がセッションを読み直す',
    body: 'nginx の確認だけに頼らず、BFF 自身もセッションを確かめます。ここでユーザー情報・アクセストークン・CSRF 値を取り出します。',
    kv: [['取り出すもの', 'ユーザー情報・アクセストークン・リフレッシュトークン・CSRF 値']],
  }));
  if (nginxSkipped) {
    steps.push(step({
      from: 'bff', to: 'bff', kind: 'self', tone: 'err', proto: 'BFF', chip: 'セッションなし', reject: true,
      summary: 'BFF がセッションなしと判断',
      title: 'BFF で止まる',
      body: 'Redis にセッションがないので 401 を返します。nginx の関門を外しても、BFF の層で止まります。',
    }));
    return steps.concat(returnPath('bff', status, data));
  }

  // 7. CSRF
  if (write) {
    const csrfFail = by === 'bff' && status === 403;
    steps.push(step({
      from: 'bff', to: 'bff', kind: 'self', tone: csrfFail ? 'err' : 'ok', proto: 'BFF',
      chip: csrfFail ? 'CSRF 不一致' : 'Origin・CSRF OK', reject: csrfFail,
      summary: csrfFail ? 'CSRF の確認で拒否' : 'Origin と CSRF トークンを確認',
      title: csrfFail ? 'BFF で止まる（CSRF）' : 'BFF が CSRF を確かめる',
      body: '状態を変えるリクエストでは、(1) `Origin` が自サイト (2) セッションに結びついた `X-CSRF-Token` が一致、の両方を求めます。'
        + (csrfFail ? ' 今回はトークンが合わないので 403 を返し、API には届けません。' : ''),
      kv: [['X-CSRF-Token', c.csrfSent ? '付いている' : '付いていない'], ['Origin', 'https://app.localhost:…']],
    }));
    if (csrfFail) return steps.concat(returnPath('bff', status, data));
  }

  // 8. リフレッシュ（呼び出しの前後でアクセストークンの期限が延びていたら）
  if (c.refreshed) {
    steps.push(step({
      from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: 'SET NX lock', tone: 'info',
      summary: '更新用のロックを取る',
      title: 'アクセストークンの期限が近い',
      body: '期限が近いので BFF がリフレッシュします。BFF が何台あっても同時に更新しないよう、Redis にロックを取ります。ブラウザは何もしません。',
    }));
    steps.push(step({
      from: 'bff', to: 'idp', proto: 'HTTP', chip: 'refresh_token', tone: 'info',
      summary: 'POST /token（refresh_token）',
      title: 'BFF がトークンを更新する',
      body: 'サーバー間で認証基盤の `/token` を呼びます。リフレッシュトークンはブラウザを一度も通りません。',
      kv: [['grant_type', 'refresh_token']],
    }));
    steps.push(step({
      from: 'idp', to: 'db', kind: 'round', proto: 'SQL', chip: '回転', tone: 'info',
      summary: 'リフレッシュトークンを回転',
      title: '認証基盤がリフレッシュトークンを回転させる',
      body: '使われたリフレッシュトークンを無効にし、同じ系統の新しいものを発行します（使い回しの検知に効く）。',
    }));
    steps.push(step({
      from: 'idp', to: 'bff', kind: 'res', tone: 'ok', proto: 'HTTP', chip: '新しいトークン',
      summary: '新しいアクセストークン',
      title: '新しいトークンが BFF に届く',
      body: '新しいアクセストークンとリフレッシュトークンを受け取ります。',
    }));
    steps.push(step({
      from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: 'SET XX', tone: 'info',
      summary: '存在するセッションだけ更新',
      title: 'セッションを更新する',
      body: '`SET XX` で「まだ存在するセッションだけ」を更新します。更新中にログアウトされていたら、消えたセッションを復活させません。',
    }));
  }

  if (status === 502 && by === 'bff') {
    steps.push(step({ from: 'bff', to: 'api', proto: 'HTTP', tone: 'err', chip: '届かない', summary: 'API に届かない', title: 'API に届かない', body: 'BFF から API へ接続できませんでした。', reject: true }));
    return steps.concat(returnPath('bff', status, data));
  }
  if (by === 'bff') { // その他の BFF での拒否（期限切れでリフレッシュに失敗した など）
    steps.push(step({
      from: 'bff', to: 'bff', kind: 'self', tone: 'err', proto: 'BFF', chip: data.error || String(status), reject: true,
      summary: 'BFF が拒否', title: 'BFF で止まる', body: `BFF が ${status} を返しました（${data.error || '理由不明'}）。`,
    }));
    return steps.concat(returnPath('bff', status, data));
  }

  // 9. Cookie → JWT の付け替え
  steps.push(step({
    from: 'bff', to: 'api', proto: 'HTTP', chip: 'Authorization: Bearer JWT',
    summary: 'JWT を付けて API へ',
    title: 'BFF が Cookie を JWT に付け替える',
    body: 'API には Cookie を渡さず、Redis から取り出したアクセストークン（JWT）を `Authorization` ヘッダに入れて送ります。トークンが通るのは Docker の内部ネットワークの中だけです。',
    kv: [['付けるもの', 'Authorization: Bearer <JWT>'], ['外すもの', 'Cookie'], ['ネットワーク', 'backend（internal）']],
  }));

  // 10. API の検証
  const apiReject = by === 'api';
  steps.push(step({
    from: 'api', to: 'api', kind: 'self', proto: 'API', tone: apiReject ? 'err' : 'ok', reject: apiReject,
    chip: apiReject ? (status === 403 ? 'ロール不足' : 'JWT 不正') : '署名・iss・aud・exp',
    summary: apiReject ? `API が拒否（${status}）` : 'JWT を検証・ロールで認可',
    title: apiReject ? `API で止まる（${status}）` : 'API が JWT を検証する',
    body: 'JWKS の公開鍵（初回に取得してキャッシュ）で署名を確かめ、発行元（iss）・宛先（aud）・期限（exp）を検証します。API は Cookie も Redis も知りません。'
      + (write ? ' 書き込みには `editor` ロールが必要です（alice は書ける、bob は 403）。' : '')
      + (apiReject && data.detail ? ` 今回の理由: ${data.detail}` : ''),
    kv: [['検証', '署名（ES256 / RS256）・iss・aud・exp'], ...(write ? [['必要なロール', 'editor']] : [])],
  }));

  return steps.concat(returnPath('api', status, data));
}

// ---- ログイン（認可コード + PKCE） ---------------------------------------------

/** @param {{user?: {name: string, username: string}, servedBy?: string}} c */
export function login(c = {}) {
  const who = c.user ? `${c.user.name}（${c.user.username}）` : 'ユーザー';
  return [
    step({
      from: 'browser', to: 'nginx', proto: 'HTTPS', chip: 'GET /bff/login',
      summary: 'ログインを始める', title: 'ログインボタンを押す',
      body: '`/bff/login` へ移動します。ログインの手続きはすべて BFF が進めます。',
    }),
    step({
      from: 'nginx', to: 'bff', proto: 'HTTP', chip: 'GET /bff/login',
      summary: 'BFF へ転送', title: 'nginx が BFF へ転送する',
      body: '`/bff/` は BFF 自身のエンドポイントです（`auth_request` なし）。',
    }),
    step({
      from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: 'state・nonce・verifier',
      summary: 'ログイン途中の状態を保存', title: 'BFF が使い捨ての値を作って保存する',
      body: '`state`（どのログインの戻りか）・`nonce`（どの ID トークンか）・PKCE の `code_verifier`（コードを交換するのは自分だと示す秘密）を作り、Redis に短い期限で保存します。',
      kv: [['state', 'コールバックとの対応'], ['nonce', 'ID トークンとの対応'], ['code_verifier', 'BFF だけが持つ。ブラウザには渡さない']],
    }),
    step({
      from: 'bff', to: 'nginx', kind: 'res', tone: 'info', proto: 'HTTP', chip: '302 + code_challenge',
      summary: '認証基盤へのリダイレクト', title: '認証基盤へ転送させる',
      body: 'ブラウザには verifier のハッシュ（`code_challenge`, S256）と `state`・`nonce` だけを付けた URL を返します。',
      kv: [['Location', 'https://auth.localhost/authorize?…'], ['付けるもの', 'code_challenge（S256）・state・nonce']],
    }),
    step({
      from: 'nginx', to: 'browser', kind: 'res', tone: 'info', proto: 'HTTPS', chip: '302 → auth.localhost',
      summary: 'auth.localhost へリダイレクト', title: 'ブラウザが認証基盤へ移動する',
      body: 'ここから先はアプリ（app.localhost）ではなく認証基盤（auth.localhost）の画面です。アプリはパスワードを一度も見ません。',
    }),
    step({
      from: 'browser', to: 'nginx', proto: 'HTTPS', chip: 'ユーザー名・パスワード',
      summary: '認証画面でパスワードを送る', title: '認証基盤の画面でログインする',
      body: 'ユーザー名とパスワードを POST します。認証基盤の SSO Cookie が残っていれば、この画面は省略されます。',
      kv: [['宛先', 'https://auth.localhost/login'], ['送る先', '認証基盤だけ（BFF・API には届かない）']],
    }),
    step({
      from: 'nginx', to: 'idp', proto: 'HTTP', chip: '/authorize・/login',
      summary: '認証基盤へ転送', title: 'nginx が Host で振り分ける',
      body: '`server_name auth.localhost` のブロックが認証基盤へ転送します。同じ nginx でも Host 名で行き先が変わります。',
    }),
    step({
      from: 'idp', to: 'db', kind: 'round', proto: 'SQL', chip: 'ハッシュ照合・コード保存',
      summary: 'パスワード照合・認可コード発行', title: '認証基盤がパスワードを確かめる',
      body: 'パスワードはハッシュと照合します。SSO セッションと認可コードを作り、DB にはそれらのハッシュだけを保存します。',
    }),
    step({
      from: 'idp', to: 'nginx', kind: 'res', tone: 'info', proto: 'HTTP', chip: '302 ?code&state',
      summary: 'コールバックへリダイレクト', title: '認可コードを持たせて戻す',
      body: 'SSO Cookie（`__Host-auth_sso`）を付け、登録済みの `redirect_uri` に認可コードと `state` を付けて戻します。登録されていない戻り先には転送しません。',
      kv: [['Location', 'https://app.localhost/bff/callback?code=…&state=…']],
    }),
    step({
      from: 'nginx', to: 'browser', kind: 'res', tone: 'info', proto: 'HTTPS', chip: '302 → /bff/callback',
      summary: 'アプリへ戻る', title: 'ブラウザがアプリへ戻る',
      body: 'ブラウザが一時的に運ぶのは **認可コード** で、API を呼ぶアクセストークンとは別物です。コードだけ盗まれても、verifier がなければ交換できません。',
    }),
    step({
      from: 'browser', to: 'nginx', proto: 'HTTPS', chip: 'code・state',
      summary: 'GET /bff/callback', title: 'コールバックを開く',
      body: 'ブラウザが認可コードと `state` を BFF のコールバックに届けます。nginx のアクセスログにはクエリの値を残しません。',
    }),
    step({
      from: 'nginx', to: 'bff', proto: 'HTTP', chip: 'code・state',
      summary: 'BFF へ転送', title: 'nginx が BFF へ転送する', body: '',
    }),
    step({
      from: 'bff', to: 'bff', kind: 'self', tone: 'ok', proto: 'BFF', chip: 'state を照合',
      summary: 'state を照合', title: 'BFF が state を確かめる',
      body: '自分が始めたログインの戻りかを `state` で確かめます。合わなければ 400 で止めます（ログイン CSRF の対策）。',
    }),
    step({
      from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: 'GETDEL',
      summary: 'ログイン途中の状態を取り出して消す', title: '使い捨ての値を取り出す',
      body: '保存しておいた `nonce` と `code_verifier` を取り出し、同時に消します。同じコールバックは二度と使えません。',
    }),
    step({
      from: 'bff', to: 'idp', proto: 'HTTP', chip: 'code + code_verifier',
      summary: 'POST /token（認可コードの交換）', title: 'BFF がコードをトークンに交換する',
      body: 'サーバー間で `/token` を呼びます。`code_verifier` と BFF のクライアント認証情報を付けるので、コードを盗んだだけの第三者は交換できません。',
      kv: [['grant_type', 'authorization_code'], ['送るもの', 'code・code_verifier・client 認証']],
    }),
    step({
      from: 'idp', to: 'db', kind: 'round', proto: 'SQL', chip: 'コードを一度だけ消費',
      summary: '認可コードを消費', title: '認証基盤がコードを検証する',
      body: 'コードは一回だけ使えます。`redirect_uri` と PKCE（`SHA256(verifier) == challenge`）も確かめます。',
    }),
    step({
      from: 'idp', to: 'bff', kind: 'res', tone: 'ok', proto: 'HTTP', chip: 'access・id・refresh',
      summary: 'トークンが BFF に届く', title: 'トークンは BFF に届く（ブラウザではない）',
      body: 'アクセストークン・ID トークン・リフレッシュトークンを受け取るのは BFF だけです。',
    }),
    step({
      from: 'bff', to: 'bff', kind: 'self', tone: 'ok', proto: 'BFF', chip: '署名・iss・aud・exp・nonce',
      summary: 'ID トークンを検証', title: 'BFF が ID トークンを検証する',
      body: '署名・発行元・宛先・期限に加え、`nonce` がさっき作ったものと一致するかを確かめます。',
    }),
    step({
      from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: 'SET sess:新ID（TTL）',
      summary: 'セッションを作る', title: '新しいセッションを Redis に作る',
      body: `${who} の情報とトークン・CSRF 値を、新しく作ったセッション ID で保存します。ログイン前の ID は使い回しません（セッション固定攻撃の対策）。`,
    }),
    step({
      from: 'bff', to: 'nginx', kind: 'res', tone: 'ok', proto: 'HTTP', chip: 'Set-Cookie: __Host-sid',
      summary: 'Cookie を付けてアプリへ', title: 'セッション Cookie を渡す',
      body: 'ブラウザに渡すのは推測できないセッション ID だけです。',
      kv: [['属性', 'HttpOnly; Secure; SameSite=Lax; Path=/'], ['応答した BFF', c.servedBy || '—']],
    }),
    step({
      from: 'nginx', to: 'browser', kind: 'res', tone: 'ok', proto: 'HTTPS', chip: '__Host-sid',
      summary: 'ログイン完了', title: 'ログイン完了',
      body: '`HttpOnly` なので画面の JavaScript からは読めません。以後の `fetch` には、ブラウザがこの Cookie を自動で付けます。',
      kv: [['ブラウザが持つもの', 'app の __Host-sid と auth の __Host-auth_sso'], ['持たないもの', 'アクセストークン・ID トークン・リフレッシュトークン']],
    }),
  ];
}

// ---- ログアウト -----------------------------------------------------------------

/**
 * @param {{status: number, data: any, csrfSent?: boolean, idp?: boolean}} c
 *   idp: 認証基盤のログアウト（SSO の終了）まで実際に行ったか
 */
export function logout(c) {
  const { status, data } = c;
  const steps = [
    step({
      from: 'browser', to: 'nginx', proto: 'HTTPS', chip: 'Cookie + X-CSRF-Token',
      summary: 'POST /bff/logout', title: 'ログアウトを送る',
      body: 'ログアウトも状態を変える操作なので、Cookie に加えて `X-CSRF-Token` と `Origin` を送ります（よそのサイトから勝手にログアウトさせられないように）。',
    }),
    step({
      from: 'nginx', to: 'bff', proto: 'HTTP', chip: 'POST /bff/logout',
      summary: 'BFF へ転送', title: 'nginx が BFF へ転送する', body: '`/bff/` には `auth_request` はありません。確認は BFF が行います。',
    }),
    step({
      from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: 'GET sess:…',
      summary: 'セッションを照会', title: 'BFF がセッションを探す', body: '',
    }),
  ];
  const by = data && data.rejected_by;
  if (status === 401) {
    steps.push(step({ from: 'bff', to: 'bff', kind: 'self', tone: 'err', proto: 'BFF', chip: 'セッションなし', reject: true, summary: 'セッションなし', title: 'BFF で止まる', body: 'ログアウトするセッションがありません。' }));
    return steps.concat(returnPath('bff', status, data));
  }
  const csrfFail = status === 403 && by === 'bff';
  steps.push(step({
    from: 'bff', to: 'bff', kind: 'self', tone: csrfFail ? 'err' : 'ok', proto: 'BFF', reject: csrfFail,
    chip: csrfFail ? 'CSRF 不一致' : 'Origin・CSRF OK',
    summary: 'Origin と CSRF を確認', title: csrfFail ? 'BFF で止まる（CSRF）' : 'BFF が CSRF を確かめる', body: '',
  }));
  if (csrfFail) return steps.concat(returnPath('bff', status, data));
  if (status !== 200) {
    steps.push(step({ from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', tone: 'err', chip: 'エラー', reject: true, summary: 'セッションを消せない', title: 'ログアウトを成功扱いにしない', body: 'Redis の削除に失敗したときは 500 を返し、Cookie の削除も返しません。' }));
    return steps.concat(returnPath('bff', status, data));
  }
  steps.push(
    step({
      from: 'bff', to: 'redis', kind: 'round', proto: 'Redis', chip: 'GETDEL sess:…',
      summary: 'セッションを取り出して消す', title: 'セッションを消す',
      body: '`GETDEL` で最新のセッションを取り出すと同時に消します。リフレッシュと重なっても、`SET XX` は消えたセッションを復活させません。',
    }),
    step({
      from: 'bff', to: 'idp', proto: 'HTTP', chip: 'POST /revoke',
      summary: 'リフレッシュトークンを失効', title: 'リフレッシュトークンを失効させる',
      body: 'BFF が持っていたリフレッシュトークンを認証基盤に失効させます。',
    }),
    step({
      from: 'idp', to: 'db', kind: 'round', proto: 'SQL', chip: '系統ごと失効',
      summary: '同じ系統を失効', title: '同じ系統のトークンを失効', body: '発行済みの JWT（アクセストークン）は期限まで有効になり得ますが、ブラウザは持っていないので、ブラウザからは使えません。',
    }),
    step({
      from: 'idp', to: 'bff', kind: 'res', tone: 'ok', proto: 'HTTP', chip: '200', summary: '失効完了', title: '失効が終わる', body: '',
    }),
    step({
      from: 'bff', to: 'nginx', kind: 'res', tone: 'ok', proto: 'HTTP', chip: 'Cookie 削除 + logout_url',
      summary: 'Cookie の削除と logout_url', title: 'アプリのセッションが終わる',
      body: '`__Host-sid` を消す `Set-Cookie` と、認証基盤のログアウト URL を返します。URL に ID トークンは付けません（ブラウザの履歴に残るため）。',
    }),
    step({
      from: 'nginx', to: 'browser', kind: 'res', tone: 'ok', proto: 'HTTPS', chip: '200 + logout_url',
      summary: 'ブラウザへ', title: 'アプリからはログアウト済み',
      body: 'ただし認証基盤の SSO Cookie はまだ残っています。このままだと、次のログインはパスワードなしで通ってしまいます。',
    }),
  );
  if (c.idp === false) return steps;
  steps.push(
    step({
      from: 'browser', to: 'nginx', proto: 'HTTPS', chip: 'GET auth…/logout',
      summary: '認証基盤のログアウトへ', title: '認証基盤からもログアウトする',
      body: '画面は返ってきた `logout_url` へ移動します。`auth.localhost` の SSO Cookie が一緒に送られます。',
    }),
    step({ from: 'nginx', to: 'idp', proto: 'HTTP', chip: 'SSO Cookie', summary: '認証基盤へ転送', title: 'nginx が認証基盤へ転送する', body: '' }),
    step({
      from: 'idp', to: 'db', kind: 'round', proto: 'SQL', chip: 'SSO 削除',
      summary: 'SSO セッションを削除', title: 'SSO セッションを消す', body: '次のログインでは、パスワード画面がもう一度出ます。',
    }),
    step({
      from: 'idp', to: 'nginx', kind: 'res', tone: 'ok', proto: 'HTTP', chip: '302 + SSO Cookie 削除',
      summary: 'アプリへ戻す', title: 'アプリへ戻す', body: '登録済みの `post_logout_redirect_uri` にだけ戻します。',
    }),
    step({
      from: 'nginx', to: 'browser', kind: 'res', tone: 'ok', proto: 'HTTPS', chip: '302 → app',
      summary: 'ログアウト完了', title: 'ログアウト完了',
      body: 'アプリのセッション（Redis）と認証基盤の SSO（PostgreSQL）の両方が終わりました。',
      kv: [['ブラウザに残るもの', 'なし（両方の Cookie を削除）']],
    }),
  );
  return steps;
}

// ---- document.cookie を読む ------------------------------------------------------

export function readCookie(value) {
  return [
    step({
      from: 'browser', to: 'browser', kind: 'self', proto: 'JS', tone: 'info', chip: 'document.cookie',
      summary: 'document.cookie を読む', title: 'JavaScript から Cookie を読んでみる',
      body: 'XSS で悪いスクリプトが入り込んだ状況の真似です。ページの JavaScript から `document.cookie` を読みます。',
      kv: [['結果', JSON.stringify(value)]],
    }),
    step({
      from: 'browser', to: 'browser', kind: 'self', proto: 'JS', tone: 'ok', chip: 'HttpOnly で見えない',
      summary: 'セッション ID は見えない', title: 'セッション ID は盗めない',
      body: '`__Host-sid` は `HttpOnly` なので JavaScript からは見えません。トークンはそもそもブラウザにありません。'
        + ' ただし XSS があれば「このページから本人として fetch する」ことはできてしまうので、CSP（`script-src \'self\'`）と `textContent` での表示で XSS そのものを防ぐのが前提です。',
      kv: [['読めないもの', '__Host-sid（HttpOnly）・トークン（ブラウザにない）'], ['防げないもの', 'XSS 中の同一オリジンからの操作']],
    }),
  ];
}
