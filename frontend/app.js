// フロントエンド（SPA）。トークンを一切扱わないのが BFF パターンのポイント。
// - fetch は同一オリジン（app.localhost）だけ。Cookie はブラウザが自動で送る
// - 状態を変えるリクエストには、/bff/session で受け取った CSRF トークンをヘッダで付ける
// - 表示は textContent だけ（innerHTML を使わない）
//
// ボタンを押すと「本物のリクエスト」を送り、返ってきた結果（ステータス・rejected_by・
// トークンの更新の有無）から通信の流れを組み立てて、構成図とシーケンス図で再生する。
'use strict';

import * as flows from './scenarios.js';
import { ACTORS, NetworkView, SequenceView } from './stage.js';

const $ = (sel) => document.querySelector(sel);

function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const k of kids) if (k != null) el.append(k);
  return el;
}

// 本文の `code` と **強調** だけを解釈する（それ以外は文字列のまま）
function rich(text) {
  const frag = document.createDocumentFragment();
  for (const part of text.split(/(`[^`]+`|\*\*[^*]+\*\*)/)) {
    if (!part) continue;
    if (part.startsWith('`')) frag.append(h('code', {}, part.slice(1, -1)));
    else if (part.startsWith('**')) frag.append(h('strong', {}, part.slice(2, -2)));
    else frag.append(part);
  }
  return frag;
}

// sessionStorage は使えないこともある（プライベートモードなど）。使えなければ「振り返り」を出さないだけ
const pending = {
  set(v) { try { sessionStorage.setItem('as:pending', JSON.stringify(v)); } catch { /* 使えなくても動く */ } },
  take() {
    try {
      const v = sessionStorage.getItem('as:pending');
      sessionStorage.removeItem('as:pending');
      return v ? JSON.parse(v) : null;
    } catch { return null; }
  },
};

// ---- 通信 -------------------------------------------------------------------

let csrfToken = null;
let session = null;

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

async function loadSession() {
  const { res, data } = await request('GET', '/bff/session');
  if (res.ok) {
    csrfToken = data.csrf_token;
    session = data;
  } else {
    csrfToken = null;
    session = null;
  }
  renderSession();
}

// ---- 表示: セッション・レスポンス -------------------------------------------------

function renderSession() {
  const s = session;
  const pill = $('#status');
  if (!s) {
    pill.textContent = '未ログイン';
    pill.className = 'pill off';
  } else {
    pill.textContent = `${s.user.name}（${s.user.username}）`;
    pill.className = 'pill on';
  }
  $('#login').hidden = !!s;
  $('#logout').hidden = !s;
  if (!player.steps.length) renderOverview();
}

function sessionKv() {
  const s = session;
  if (!s) return [['状態', 'ログインしていません'], ['ブラウザの Cookie', 'なし（または期限切れ）']];
  return [
    ['ユーザー', `${s.user.name} / ${s.user.username}`],
    ['ロール', (s.user.roles || []).join(', ')],
    ['アクセストークンの期限', new Date(s.access_token_expires_at).toLocaleTimeString()],
    ['応答した BFF', s.served_by],
  ];
}

function kvList(rows) {
  const dl = h('dl', { class: 'kv' });
  for (const [k, v] of rows) dl.append(h('dt', {}, k), h('dd', {}, rich(String(v))));
  return dl;
}

function log(method, path, status, data) {
  const head = h('div', { class: 'log-head' },
    h('span', { class: `badge s${String(status)[0]}` }, String(status)),
    h('code', {}, `${method} ${path}`));
  if (data && typeof data === 'object' && data.rejected_by) head.append(h('span', { class: 'by' }, `止めた層: ${data.rejected_by}`));
  head.append(h('time', {}, new Date().toLocaleTimeString()));
  const pre = h('pre', {}, typeof data === 'string' ? data : JSON.stringify(data, null, 2));
  const list = $('#log');
  list.prepend(h('li', {}, head, pre));
  while (list.children.length > 20) list.lastElementChild.remove();
}

// ---- 再生 -------------------------------------------------------------------

const player = {
  title: '',
  steps: [],
  result: null, // { status, data } 実際の応答（あれば）
  extra: null, // 最後のステップで出す追加の要素を作る関数
  idx: -1,
  playing: false,
  gen: 0,
  speed: 1,
  view: 'network',
};

let net;
let seq;

const travelMs = () => 950 / player.speed;
const dwellMs = () => 2300 / player.speed;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function start(title, steps, { result = null, extra = null } = {}) {
  player.gen++;
  Object.assign(player, { title, steps, result, extra, idx: -1, playing: false });
  $('#scenario-title').textContent = title;
  $('#stage').classList.add('has-scenario');
  net.reset();
  seq.show(steps, -1);
  renderHops();
  renderDetail();
  updateControls();
  if (window.matchMedia('(max-width: 899px)').matches) $('#stage').scrollIntoView({ behavior: 'smooth', block: 'start' });
  play();
}

async function play() {
  if (!player.steps.length) return;
  if (player.idx >= player.steps.length - 1) player.idx = -1;
  const g = ++player.gen;
  player.playing = true;
  updateControls();
  while (player.gen === g && player.idx < player.steps.length - 1) {
    await goto(player.idx + 1, true);
    if (player.gen !== g) return;
    if (player.idx < player.steps.length - 1) await sleep(dwellMs());
  }
  if (player.gen === g) {
    player.playing = false;
    updateControls();
  }
}

function pause() {
  player.gen++;
  player.playing = false;
  updateControls();
}

function stepBy(d) {
  pause();
  const i = Math.max(0, Math.min(player.steps.length - 1, player.idx + d));
  if (i !== player.idx) goto(i, true);
}

function goto(i, animate) {
  player.idx = i;
  renderHops();
  renderDetail();
  updateControls();
  const opts = { animate, duration: travelMs() };
  const a = net.show(player.steps, i, player.view === 'network' ? opts : { animate: false });
  const b = seq.show(player.steps, i, player.view === 'sequence' ? opts : { animate: false });
  return player.view === 'network' ? a : b;
}

function setView(v) {
  player.view = v;
  for (const btn of document.querySelectorAll('[data-view]')) btn.setAttribute('aria-selected', String(btn.dataset.view === v));
  $('#view-network').hidden = v !== 'network';
  $('#view-sequence').hidden = v !== 'sequence';
  if (player.idx >= 0) {
    net.show(player.steps, player.idx, { animate: false });
    seq.show(player.steps, player.idx, { animate: false });
  }
}

function updateControls() {
  const n = player.steps.length;
  const i = player.idx;
  $('#play').disabled = !n;
  $('#play-label').textContent = player.playing ? '一時停止' : (i >= n - 1 && n ? 'もう一度' : '再生');
  $('#play').classList.toggle('is-playing', player.playing);
  $('#prev').disabled = !n || i <= 0;
  $('#next').disabled = !n || i >= n - 1;
  $('#progress-bar').style.width = n ? `${((i + 1) / n) * 100}%` : '0%';
  $('#step-count').textContent = n ? `${Math.max(0, i + 1)} / ${n}` : '';
}

// ---- 表示: 解説パネル・通信の一覧 -----------------------------------------------

const PROTO_NOTE = {
  HTTPS: 'ブラウザ ⇄ nginx（TLS）',
  HTTP: 'Docker 内部ネットワーク',
  Redis: 'BFF ⇄ Redis（bff-data）',
  SQL: '認証基盤 ⇄ PostgreSQL（auth-data）',
  BFF: 'BFF の中での判定',
  API: 'API の中での判定',
  JS: 'ブラウザの中',
};

function renderOverview() {
  const d = $('#detail');
  d.replaceChildren(
    h('p', { class: 'eyebrow' }, 'はじめに'),
    h('h2', {}, '上のボタンを押してみてください'),
    h('p', { class: 'lead' }, rich('ボタンを押すと本物のリクエストを送り、返ってきた結果から「どこを通って、どこで止まったか」を再生します。')),
    h('ul', { class: 'holds' },
      h('li', {}, h('strong', {}, 'ブラウザ'), rich(' … `HttpOnly` のセッション Cookie だけ。トークンは持たない')),
      h('li', {}, h('strong', {}, 'nginx'), ' … 入口。未ログインの API 呼び出しはここで止める'),
      h('li', {}, h('strong', {}, 'BFF'), ' … Cookie をトークンに付け替える。CSRF もここで確認'),
      h('li', {}, h('strong', {}, 'API'), ' … JWT を検証し、ロールで認可する')),
    h('h3', {}, 'いまのセッション'),
    kvList(sessionKv()),
    h('p', { class: 'hint' }, rich('キーボード: `Space` 再生・停止 / `←` `→` 1ステップずつ')),
  );
}

function renderDetail() {
  const d = $('#detail');
  const { steps, idx } = player;
  if (idx < 0 || !steps[idx]) {
    d.replaceChildren(
      h('p', { class: 'eyebrow' }, player.title),
      h('h2', {}, '準備中…'),
      h('p', { class: 'lead' }, `${steps.length} ステップの通信を再生します。`),
    );
    return;
  }
  const s = steps[idx];
  const route = s.from === s.to
    ? `${ACTORS[s.from].name} の中`
    : `${ACTORS[s.from].name} → ${ACTORS[s.to].name}${s.kind === 'round' ? ' → ' + ACTORS[s.from].name : ''}`;
  const kids = [
    h('p', { class: 'eyebrow' }, `ステップ ${idx + 1} / ${steps.length}`),
    h('h2', { class: `tone-text-${s.tone}` }, s.title),
    h('p', { class: 'route' },
      h('span', { class: `proto proto-${s.proto}` }, s.proto),
      h('span', {}, route),
      h('span', { class: 'muted' }, PROTO_NOTE[s.proto] || '')),
  ];
  if (s.body) kids.push(h('p', { class: 'lead' }, rich(s.body)));
  kids.push(h('div', { class: `carry tone-${s.tone}` }, h('span', { class: 'carry-label' }, '運ぶもの'), h('code', {}, s.chip)));
  if (s.kv.length) kids.push(kvList(s.kv));

  if (idx === steps.length - 1) {
    const box = h('div', { class: 'result' });
    if (player.result) {
      const { status, data } = player.result;
      box.append(h('div', { class: 'result-head' },
        h('span', { class: `badge s${String(status)[0]}` }, String(status)),
        h('strong', {}, data && data.rejected_by ? `止めた層: ${data.rejected_by}` : (status < 400 ? '最後まで通った' : 'エラー'))));
      box.append(h('details', {}, h('summary', {}, '実際のレスポンス'),
        h('pre', {}, typeof data === 'string' ? data : JSON.stringify(data, null, 2))));
    }
    if (player.extra) box.append(player.extra());
    if (box.childNodes.length) kids.push(box);
  }
  d.replaceChildren(...kids);
}

function renderHops() {
  const tb = $('#hops');
  const { steps, idx } = player;
  if (idx < 0) {
    tb.replaceChildren(h('tr', { class: 'empty' }, h('td', { colspan: 4 },
      steps.length ? '再生すると、ここに通信が1本ずつ並びます。' : 'まだ何も流れていません。上のボタンで操作してみてください。')));
    return;
  }
  const rows = [];
  for (let k = 0; k <= idx; k++) {
    const s = steps[k];
    const route = s.from === s.to ? `${ACTORS[s.from].name}（内部）` : `${ACTORS[s.from].name} ${s.kind === 'round' ? '⇄' : '→'} ${ACTORS[s.to].name}`;
    const tr = h('tr', {
      class: `tone-${s.tone}${k === idx ? ' current' : ''}${s.reject ? ' reject' : ''}`,
      tabindex: 0,
      onclick: () => { pause(); goto(k, true); },
      onkeydown: (e) => { if (e.key === 'Enter') { pause(); goto(k, true); } },
    },
    h('td', { class: 'num' }, h('span', { class: 'num-dot' }, String(k + 1))),
    h('td', {}, route),
    h('td', {}, h('span', { class: `proto proto-${s.proto}` }, s.proto)),
    h('td', { class: 'info' }, s.summary, h('code', { class: 'chip-inline' }, s.chip)));
    rows.push(tr);
  }
  rows[idx].classList.add('fresh');
  tb.replaceChildren(...rows);
  // ページ全体ではなく、一覧の枠の中だけをスクロールする
  const wrap = tb.closest('.hops-wrap');
  const wr = wrap.getBoundingClientRect();
  const rr = rows[idx].getBoundingClientRect();
  const headH = wrap.querySelector('thead').getBoundingClientRect().height;
  if (rr.bottom > wr.bottom) wrap.scrollTop += rr.bottom - wr.bottom;
  else if (rr.top < wr.top + headH) wrap.scrollTop -= wr.top + headH - rr.top;
}

// ---- 操作 -------------------------------------------------------------------

async function runApi(method, path, { body, csrf = true } = {}) {
  const hadSession = !!session;
  const before = session && session.access_token_expires_at;
  const { res, data } = await request(method, path, { body, csrf });
  log(method, path, res.status, data);
  await loadSession(); // トークンの期限表示を更新（リフレッシュされたか見える）
  const refreshed = !!(hadSession && session && before && session.access_token_expires_at !== before);
  start(`${method} ${path}${csrf ? '' : '（CSRF トークンなし）'}`,
    flows.apiCall({ method, path, status: res.status, data, hadSession, csrfSent: csrf && !!csrfToken, refreshed }),
    { result: { status: res.status, data } });
}

async function runLogout() {
  const { res, data } = await request('POST', '/bff/logout');
  log('POST', '/bff/logout', res.status, data);
  // 認証基盤（IdP）側のログイン状態も終わらせる。終わると post_logout_redirect_uri でここに戻り、流れを再生する
  if (res.ok && data.logout_url) {
    pending.set({ kind: 'logout', status: res.status });
    window.location.assign(data.logout_url);
    return;
  }
  await loadSession();
  start('ログアウト', flows.logout({ status: res.status, data, idp: false }), { result: { status: res.status, data } });
}

function loginDemo() {
  start('ログインの流れ（認可コード + PKCE）', flows.login({ user: session && session.user, servedBy: session && session.served_by }), {
    extra: () => (session
      ? h('p', { class: 'muted' }, 'いまはログイン済みです。一度ログアウトすると、実際にこの流れを通れます。')
      : h('a', { class: 'button primary', href: '/bff/login', onclick: () => pending.set({ kind: 'login' }) }, 'この流れで実際にログインする')),
  });
}

document.addEventListener('DOMContentLoaded', async () => {
  net = new NetworkView($('#net'));
  seq = new SequenceView($('#seq'), $('#seq-scroll'));
  renderHops();
  updateControls();

  for (const btn of document.querySelectorAll('[data-call]')) {
    btn.addEventListener('click', () => {
      const [method, path] = btn.dataset.call.split(' ');
      runApi(method, path);
    });
  }
  $('#note-form').addEventListener('submit', (e) => {
    e.preventDefault();
    runApi('POST', '/api/notes', { body: { text: $('#note-text').value } });
  });
  $('#no-csrf').addEventListener('click', () => runApi('POST', '/api/notes', { body: { text: 'csrf なし' }, csrf: false }));
  $('#read-cookie').addEventListener('click', () => start('document.cookie を読む', flows.readCookie(document.cookie)));
  $('#demo-login').addEventListener('click', loginDemo);
  $('#login').addEventListener('click', () => pending.set({ kind: 'login' }));
  $('#logout').addEventListener('click', runLogout);

  $('#play').addEventListener('click', () => (player.playing ? pause() : play()));
  $('#prev').addEventListener('click', () => stepBy(-1));
  $('#next').addEventListener('click', () => stepBy(1));
  $('#speed').addEventListener('change', (e) => { player.speed = Number(e.target.value) || 1; });
  for (const btn of document.querySelectorAll('[data-view]')) btn.addEventListener('click', () => setView(btn.dataset.view));

  document.addEventListener('keydown', (e) => {
    if (e.target.closest('input, textarea, select') || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === ' ') {
      if (e.target.closest('button, a, summary, [tabindex]')) return; // それ自体の操作を優先する
      e.preventDefault();
      if (player.playing) pause(); else play();
    }
    else if (e.key === 'ArrowRight') stepBy(1);
    else if (e.key === 'ArrowLeft') stepBy(-1);
  });

  await loadSession();

  // ページ遷移をまたいだ操作（ログイン・ログアウト）は、戻ってきたところで「いま起きたこと」を再生する
  const p = pending.take();
  if (p && p.kind === 'login' && session) {
    start('ログイン（いま起きたこと）', flows.login({ user: session.user, servedBy: session.served_by }));
  } else if (p && p.kind === 'logout' && !session) {
    start('ログアウト（いま起きたこと）', flows.logout({ status: p.status, data: {}, idp: true }));
  }
});
