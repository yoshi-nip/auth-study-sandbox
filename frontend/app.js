// フロントエンド（SPA）。トークンを一切扱わないのが BFF パターンのポイント。
// - fetch は同一オリジン（app.localhost）だけ。Cookie はブラウザが自動で送る
// - 状態を変えるリクエストには、/bff/session で受け取った CSRF トークンをヘッダで付ける
// - 表示は textContent だけ（innerHTML を使わない）
'use strict';

const $ = (sel) => document.querySelector(sel);
let csrfToken = null;

async function request(method, path, { body, csrf = true } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (csrf && csrfToken && method !== 'GET') headers['X-CSRF-Token'] = csrfToken;
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: 'same-origin',
  });
  const text = await res.text();
  let data = text;
  try { data = JSON.parse(text); } catch { /* JSON でなければそのまま */ }
  return { res, data };
}

function log(method, path, status, data) {
  const li = document.createElement('li');
  const head = document.createElement('div');
  head.className = 'log-head';
  const badge = document.createElement('span');
  badge.className = `badge s${String(status)[0]}`;
  badge.textContent = status;
  const title = document.createElement('code');
  title.textContent = `${method} ${path}`;
  head.append(badge, title);
  if (data && typeof data === 'object' && data.rejected_by) {
    const by = document.createElement('span');
    by.className = 'by';
    by.textContent = `止めた層: ${data.rejected_by}`;
    head.append(by);
  }
  const pre = document.createElement('pre');
  pre.textContent = typeof data === 'string' ? data : JSON.stringify(data, null, 2);
  li.append(head, pre);
  $('#log').prepend(li);
}

function renderSession(s) {
  const dl = $('#session');
  dl.replaceChildren();
  const add = (k, v) => {
    const dt = document.createElement('dt'); dt.textContent = k;
    const dd = document.createElement('dd'); dd.textContent = v;
    dl.append(dt, dd);
  };
  if (!s) {
    $('#status').textContent = '未ログイン';
    $('#status').className = 'pill off';
    add('状態', 'ログインしていません');
  } else {
    $('#status').textContent = `${s.user.name}（${s.user.username}）`;
    $('#status').className = 'pill on';
    add('ユーザー', `${s.user.name} / ${s.user.username}`);
    add('ロール', (s.user.roles || []).join(', '));
    add('アクセストークンの期限', new Date(s.access_token_expires_at).toLocaleTimeString());
    add('応答した BFF', s.served_by);
  }
  $('#login').hidden = !!s;
  $('#logout').hidden = !s;
}

async function loadSession() {
  const { res, data } = await request('GET', '/bff/session');
  if (res.ok) {
    csrfToken = data.csrf_token;
    renderSession(data);
  } else {
    csrfToken = null;
    renderSession(null);
  }
}

async function call(method, path, opts) {
  const { res, data } = await request(method, path, opts);
  log(method, path, res.status, data);
  if (res.status === 401) await loadSession();
  else if (res.ok) await loadSession(); // トークンの期限表示を更新（リフレッシュされたか見える）
}

document.addEventListener('DOMContentLoaded', () => {
  document.querySelectorAll('[data-call]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const [method, path] = btn.dataset.call.split(' ');
      call(method, path);
    });
  });

  $('#note-form').addEventListener('submit', (e) => {
    e.preventDefault();
    call('POST', '/api/notes', { body: { text: $('#note-text').value } });
  });

  $('#no-csrf').addEventListener('click', () => {
    call('POST', '/api/notes', { body: { text: 'csrf なし' }, csrf: false });
  });

  $('#logout').addEventListener('click', async () => {
    const { res, data } = await request('POST', '/bff/logout');
    log('POST', '/bff/logout', res.status, data);
    // 認証基盤（IdP）側のログイン状態も終わらせる。終わると post_logout_redirect_uri でここに戻る
    if (res.ok && data.logout_url) window.location.assign(data.logout_url);
    else await loadSession();
  });

  loadSession();
});
