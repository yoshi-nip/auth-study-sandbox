// 構成図（ネットワーク）とシーケンス図の描画・アニメーション。
// - innerHTML は使わず、createElementNS で組み立てる
// - style 属性は CSP（style-src 'self'）で止まるので、色や線は class と SVG の属性で指定する
'use strict';

const NS = 'http://www.w3.org/2000/svg';

export function svg(tag, attrs = {}, ...kids) {
  const el = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) if (v != null && v !== false) el.setAttribute(k, String(v));
  for (const k of kids) if (k != null) el.append(k);
  return el;
}

const NODE_W = 150;
const NODE_H = 62;

export const ACTORS = {
  browser: { name: 'ブラウザ', sub: 'Cookie だけ持つ', x: 92, y: 214, icon: 'browser' },
  nginx: { name: 'nginx', sub: '入口・TLS 終端', x: 282, y: 214, icon: 'shield' },
  bff: { name: 'BFF', sub: 'Cookie⇄トークン', x: 490, y: 214, icon: 'swap' },
  redis: { name: 'Redis', sub: 'セッション', x: 490, y: 384, icon: 'db' },
  api: { name: 'API', sub: 'JWT を検証', x: 724, y: 84, icon: 'api' },
  idp: { name: '認証基盤', sub: 'auth.localhost', x: 724, y: 314, icon: 'key' },
  db: { name: 'PostgreSQL', sub: '認証基盤の DB', x: 925, y: 314, icon: 'db' },
};
export const ACTOR_ORDER = ['browser', 'nginx', 'bff', 'redis', 'idp', 'db', 'api'];

// 線（つながっているもの）とネットワーク名
const LINKS = [
  ['browser', 'nginx', 'HTTPS'],
  ['nginx', 'bff', 'app'],
  ['nginx', 'idp', 'app'],
  ['bff', 'redis', 'bff-data'],
  ['bff', 'api', 'backend'],
  ['bff', 'idp', 'backend'],
  ['api', 'idp', 'backend（JWKS）'],
  ['idp', 'db', 'auth-data'],
];
const linkKey = (a, b) => [a, b].sort().join('|');

const prefersReducedMotion = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches;

// 文字幅のおおよその見積もり（11px。全角は約 11px、半角は約 6.4px）
function textWidth(text, size = 11) {
  let w = 0;
  for (const ch of text) w += ch.charCodeAt(0) > 0xff ? size : size * 0.58;
  return w;
}

function ease(t) { return t < 0.5 ? 2 * t * t : 1 - (-2 * t + 2) ** 2 / 2; }

// A の箱の縁から B の箱の縁までの線分
function edgePoints(a, b, pad = 6) {
  const dx = b.x - a.x; const dy = b.y - a.y;
  const hw = NODE_W / 2 + pad; const hh = NODE_H / 2 + pad;
  const t = Math.min(dx ? hw / Math.abs(dx) : Infinity, dy ? hh / Math.abs(dy) : Infinity);
  return { x1: a.x + dx * t, y1: a.y + dy * t, x2: b.x - dx * t, y2: b.y - dy * t };
}

function icon(kind) {
  const g = svg('g', { class: 'icon', transform: 'translate(-62,-12)' });
  switch (kind) {
    case 'browser':
      g.append(svg('rect', { x: 0, y: 2, width: 22, height: 18, rx: 3 }), svg('line', { x1: 0, y1: 7, x2: 22, y2: 7 }));
      break;
    case 'shield':
      g.append(svg('path', { d: 'M11 1 L21 5 V11 C21 17 16 21 11 23 C6 21 1 17 1 11 V5 Z' }));
      break;
    case 'swap':
      g.append(svg('path', { d: 'M2 7 H18 L14 3 M20 16 H4 L8 20' }));
      break;
    case 'db':
      g.append(svg('ellipse', { cx: 11, cy: 5, rx: 9, ry: 3.5 }), svg('path', { d: 'M2 5 V18 C2 20 6 21.5 11 21.5 C16 21.5 20 20 20 18 V5' }));
      break;
    case 'api':
      g.append(svg('path', { d: 'M7 2 C3 2 4 9 1 11 C4 13 3 20 7 20 M15 2 C19 2 18 9 21 11 C18 13 19 20 15 20' }));
      break;
    case 'key':
      g.append(svg('circle', { cx: 6, cy: 11, r: 5 }), svg('path', { d: 'M11 11 H22 M18 11 V15 M21 11 V14' }));
      break;
    default:
  }
  return g;
}

function chip(text, tone) {
  const w = textWidth(text) + 16;
  return svg('g', { class: `chip tone-${tone}` },
    svg('rect', { x: -w / 2, y: -11, width: w, height: 22, rx: 11 }),
    svg('text', { x: 0, y: 4, 'text-anchor': 'middle' }, text));
}

// ---- 構成図 -------------------------------------------------------------------

export class NetworkView {
  constructor(root) {
    this.root = root;
    this.gen = 0;
    this.build();
  }

  build() {
    const r = this.root;
    r.replaceChildren();
    r.setAttribute('viewBox', '0 0 1020 450');

    // ゾーン: ホスト側と Docker の内部ネットワーク
    r.append(
      svg('rect', { class: 'zone zone-host', x: 8, y: 132, width: 170, height: 164, rx: 14 }),
      svg('text', { class: 'zone-label', x: 22, y: 152 }, 'あなたの PC'),
      svg('rect', { class: 'zone zone-docker', x: 196, y: 18, width: 814, height: 424, rx: 16 }),
      svg('text', { class: 'zone-label', x: 212, y: 40 }, 'Docker（ホストに公開しているのは nginx の 127.0.0.1:8443 だけ）'),
    );

    // 届かない経路: nginx → API
    const nope = edgePoints(ACTORS.nginx, ACTORS.api, 10);
    r.append(svg('g', { class: 'nope' },
      svg('line', { x1: nope.x1, y1: nope.y1, x2: nope.x2, y2: nope.y2 }),
      svg('text', { x: (nope.x1 + nope.x2) / 2, y: (nope.y1 + nope.y2) / 2 - 12, 'text-anchor': 'middle' }, '× nginx から API へは届かない')));

    this.links = new Map();
    const linkLayer = svg('g', { class: 'links' });
    for (const [a, b, label] of LINKS) {
      const p = edgePoints(ACTORS[a], ACTORS[b], 0);
      const line = svg('line', { class: 'link', x1: p.x1, y1: p.y1, x2: p.x2, y2: p.y2 });
      const mx = (p.x1 + p.x2) / 2; const my = (p.y1 + p.y2) / 2;
      const vertical = Math.abs(p.x1 - p.x2) < 4;
      const text = svg('text', { class: 'link-label', x: vertical ? mx + 8 : mx, y: vertical ? my : my - 8, 'text-anchor': vertical ? 'start' : 'middle' }, label);
      const g = svg('g', { class: 'link-g' }, line, text);
      linkLayer.append(g);
      this.links.set(linkKey(a, b), g);
    }
    r.append(linkLayer);

    this.nodes = new Map();
    const nodeLayer = svg('g', { class: 'nodes' });
    for (const [id, a] of Object.entries(ACTORS)) {
      const ring = svg('circle', { class: 'ring', r: 48, cx: 0, cy: 0 });
      const g = svg('g', { class: 'node', transform: `translate(${a.x},${a.y})`, 'data-id': id },
        ring,
        svg('rect', { class: 'node-box', x: -NODE_W / 2, y: -NODE_H / 2, width: NODE_W, height: NODE_H, rx: 12 }),
        icon(a.icon),
        svg('text', { class: 'node-name', x: -32, y: -2 }, a.name),
        svg('text', { class: 'node-sub', x: -32, y: 15 }, a.sub),
        svg('g', { class: 'mark', transform: `translate(${NODE_W / 2 - 4},${-NODE_H / 2 + 4})` },
          svg('circle', { r: 11 }), svg('path', { d: 'M-4 -4 L4 4 M4 -4 L-4 4' })),
      );
      nodeLayer.append(g);
      this.nodes.set(id, { g, ring });
    }
    r.append(nodeLayer);

    this.packet = svg('g', { class: 'packet', visibility: 'hidden' });
    r.append(this.packet);
  }

  reset() {
    this.gen++;
    for (const { g } of this.nodes.values()) g.classList.remove('from', 'to', 'rejected', 'involved');
    for (const g of this.links.values()) g.classList.remove('active', 'tone-req', 'tone-ok', 'tone-err', 'tone-info', 'used');
    this.packet.setAttribute('visibility', 'hidden');
  }

  /**
   * steps[i] を表示する。animate なら duration ミリ秒かけてパケットを動かす。
   * @returns {Promise<void>} 動き終わったら解決（途中で別の表示に切り替わっても解決）
   */
  show(steps, i, { animate = true, duration = 900 } = {}) {
    this.reset();
    const gen = this.gen;
    if (i < 0 || !steps[i]) return Promise.resolve();
    const st = steps[i];

    // ここまでに通った役者と線を薄く残す
    for (let k = 0; k <= i; k++) {
      const s = steps[k];
      this.nodes.get(s.from)?.g.classList.add('involved');
      this.nodes.get(s.to)?.g.classList.add('involved');
      if (s.from !== s.to) this.links.get(linkKey(s.from, s.to))?.classList.add('used');
      if (s.reject) this.nodes.get(s.kind === 'res' ? s.from : s.to)?.g.classList.add('rejected');
    }

    const from = this.nodes.get(st.from); const to = this.nodes.get(st.to);
    from.g.classList.add('from');
    to.g.classList.add('to');

    this.packet.replaceChildren(
      svg('circle', { class: `dot tone-${st.tone}`, r: 7 }),
      (() => { const c = chip(st.chip, st.tone); c.setAttribute('transform', 'translate(0,-22)'); return c; })(),
    );
    this.packet.setAttribute('visibility', 'visible');

    const ms = animate && !prefersReducedMotion() ? duration : 0;

    if (st.kind === 'self') {
      const a = ACTORS[st.from];
      this.packet.setAttribute('transform', `translate(${a.x},${a.y - NODE_H / 2 - 8})`);
      this.packet.querySelector('.dot').setAttribute('visibility', 'hidden');
      from.g.classList.remove('to');
      return this.pulse(from.ring, ms, gen);
    }

    const link = this.links.get(linkKey(st.from, st.to));
    link?.classList.add('active', `tone-${st.tone}`);
    const p = edgePoints(ACTORS[st.from], ACTORS[st.to], 4);
    const place = (t) => {
      this.packet.setAttribute('transform', `translate(${p.x1 + (p.x2 - p.x1) * t},${p.y1 + (p.y2 - p.y1) * t})`);
    };
    if (st.kind === 'round') {
      // 行って戻る: 前半で行き、後半で戻る
      return tween(ms, (t) => place(t < 0.5 ? ease(t * 2) : ease((1 - t) * 2)), () => gen !== this.gen, 1);
    }
    return tween(ms, (t) => place(ease(t)), () => gen !== this.gen, 1);
  }

  pulse(ring, ms, gen) {
    return tween(ms, (t) => {
      const k = (t * 2) % 1; // 2回広がる
      ring.setAttribute('r', String(46 + 22 * k));
      ring.setAttribute('opacity', String(ms ? 1 - k : 0.6));
    }, () => gen !== this.gen, ms ? 1 : 0.3);
  }
}

// t を 0→1 で回す。cancelled() が true になったら止めて解決する
function tween(ms, frame, cancelled, endAt = 1) {
  return new Promise((resolve) => {
    if (ms <= 0) { frame(endAt); resolve(); return; }
    const t0 = performance.now();
    const tick = (now) => {
      if (cancelled()) { resolve(); return; }
      const t = Math.min(1, (now - t0) / ms);
      frame(t);
      if (t < 1) requestAnimationFrame(tick);
      else { frame(endAt); resolve(); }
    };
    requestAnimationFrame(tick);
  });
}

// ---- シーケンス図 -------------------------------------------------------------

const COL_W = 128;
const ROW_H = 46;
const HEAD_H = 64;
const LEFT = 44;

export class SequenceView {
  constructor(root, scroller) {
    this.root = root;
    this.scroller = scroller;
    this.gen = 0;
  }

  show(steps, i, { animate = true, duration = 900 } = {}) {
    this.gen++;
    const gen = this.gen;
    const r = this.root;
    r.replaceChildren();
    if (!steps.length) return Promise.resolve();

    const used = new Set();
    for (const s of steps) { used.add(s.from); used.add(s.to); }
    const cols = ACTOR_ORDER.filter((id) => used.has(id));
    const xOf = (id) => LEFT + 40 + cols.indexOf(id) * COL_W + COL_W / 2;
    const width = LEFT + 40 + cols.length * COL_W + 24;
    const height = HEAD_H + steps.length * ROW_H + 24;
    r.setAttribute('viewBox', `0 0 ${width} ${height}`);
    // 枠に収まるなら縮めて収める。縮めすぎると文字が読めないので、それ以上は横スクロールにする
    const avail = this.scroller.clientWidth || width;
    const scale = Math.min(1, Math.max(0.72, avail / width));
    r.setAttribute('width', String(Math.floor(width * scale)));
    r.setAttribute('height', String(Math.ceil(height * scale)));

    r.append(svg('defs', {},
      ...['req', 'ok', 'err', 'info'].map((t) => svg('marker', {
        id: `arrow-${t}`, viewBox: '0 0 10 10', refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse',
      }, svg('path', { class: `arrowhead tone-${t}`, d: 'M0 0 L10 5 L0 10 Z' })))));

    // 見出しとライフライン
    for (const id of cols) {
      const x = xOf(id);
      r.append(
        svg('line', { class: 'lifeline', x1: x, y1: HEAD_H - 8, x2: x, y2: height - 8 }),
        svg('rect', { class: 'seq-head', x: x - COL_W / 2 + 8, y: 10, width: COL_W - 16, height: 40, rx: 9 }),
        svg('text', { class: 'seq-head-text', x, y: 35, 'text-anchor': 'middle' }, ACTORS[id].name),
      );
    }

    let current = null;
    for (let k = 0; k <= i && k < steps.length; k++) {
      const s = steps[k];
      const y = HEAD_H + k * ROW_H + 28;
      const isCur = k === i;
      const g = svg('g', { class: `seq-row tone-${s.tone}${isCur ? ' current' : ''}${s.reject ? ' reject' : ''}`, 'data-step': k });
      g.append(
        svg('rect', { class: 'seq-band', x: 4, y: y - 30, width: width - 8, height: ROW_H - 2, rx: 8 }),
        svg('circle', { class: 'seq-num', cx: LEFT - 18, cy: y - 6, r: 11 }),
        svg('text', { class: 'seq-num-text', x: LEFT - 18, y: y - 2, 'text-anchor': 'middle' }, String(k + 1)),
      );
      const x1 = xOf(s.from); const x2 = xOf(s.to);
      let shape;
      if (s.kind === 'self') {
        shape = svg('path', { class: 'seq-arrow', d: `M${x1} ${y - 12} h34 v16 h-30`, 'marker-end': `url(#arrow-${s.tone})` });
        g.append(shape, svg('text', { class: 'seq-label', x: x1 + 42, y: y - 1 }, s.summary));
      } else {
        const dir = Math.sign(x2 - x1);
        const line = svg('line', {
          class: `seq-arrow${s.kind === 'res' ? ' dashed' : ''}`, x1: x1 + dir * 4, y1: y, x2: x2 - dir * 4, y2: y,
          'marker-end': `url(#arrow-${s.tone})`,
          'marker-start': s.kind === 'round' ? `url(#arrow-${s.tone})` : null,
        });
        shape = line;
        g.append(line, svg('text', { class: 'seq-label', x: (x1 + x2) / 2, y: y - 8, 'text-anchor': 'middle' }, s.summary));
      }
      r.append(g);
      if (isCur) current = { g, shape, s, x1, x2, y };
    }

    if (current) {
      const top = (HEAD_H + i * ROW_H - 40) * scale;
      const sc = this.scroller;
      if (top < sc.scrollTop || top + (ROW_H + 80) * scale > sc.scrollTop + sc.clientHeight) {
        sc.scrollTo({ top: Math.max(0, top - sc.clientHeight / 3), behavior: prefersReducedMotion() ? 'auto' : 'smooth' });
      }
    }

    const ms = animate && !prefersReducedMotion() ? duration : 0;
    if (!current || ms === 0) return Promise.resolve();
    const { shape, s, x1, x2 } = current;
    if (s.kind === 'self') {
      current.g.classList.add('appear');
      return new Promise((res) => setTimeout(res, ms));
    }
    const dir = Math.sign(x2 - x1);
    const start = x1 + dir * 4; const end = x2 - dir * 4;
    return tween(ms, (t) => shape.setAttribute('x2', String(start + (end - start) * ease(t))), () => gen !== this.gen, 1);
  }
}
