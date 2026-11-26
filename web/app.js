(() => {
  'use strict';

  const DEFAULT_POLL_SECONDS = 15;
  const STORAGE_THEME_PREF = 'psb_theme_pref';
  const STORAGE_VIEW = 'psb_view_mode';
  const STORAGE_PASS = 'psb_view_passcode';
  const STORAGE_COLLAPSED = 'psb_collapsed_trends';
  const STORAGE_AIS_METRIC = 'psb_ais_hourly_metric';
  const STORAGE_AIS_TTFT = 'psb_ais_show_ttft';

  const SVG_NS = 'http://www.w3.org/2000/svg';
  const WINDOW_SECONDS = { '5h': 5 * 3600, '7d': 7 * 86400 };
  const RANGES = {
    '5h': [
      { key: '3h', label: '3h', ahead: false },
      { key: '6h', label: '6h', ahead: true },
      { key: '12h', label: '12h', ahead: true },
      { key: '24h', label: '24h', ahead: true },
    ],
    '7d': [
      { key: '24h', label: '24h', ahead: false },
      { key: '3d', label: '3天', ahead: false },
      { key: '8d', label: '8天', ahead: true },
      { key: '15d', label: '15天', ahead: true },
      { key: '30d', label: '30天', ahead: true },
    ],
  };

  // Lamplighter math constants (tuned for 0.01% float precision)
  const LEVEL_EPSILON = 1e-6;
  const RESET_JUMP = 5;
  const RESET_DRIFT = 60;
  const QUANTUM = 0.4;
  const HOLD_AFTER = 120;
  const CHAR_W = 6.4;

  // CPAMP 3-stop success rate RGB gradient
  const RATE_COLOR_STOPS = [
    { r: 239, g: 68, b: 68 },   // 0%
    { r: 250, g: 204, b: 21 },  // 50%
    { r: 34, g: 197, b: 94 },   // 100%
  ];

  // Theme icons (inline SVG, zero CDN)
  const THEME_ICONS = {
    system: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="2" y="3" width="20" height="14" rx="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>',
    light: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/></svg>',
    dark: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/></svg>',
  };
  const THEME_LABELS = { system: '自动', light: '浅色', dark: '深色' };
  const THEME_TITLES = {
    system: '当前主题：跟随系统（点击切换浅色）',
    light: '当前主题：浅色（点击切换深色）',
    dark: '当前主题：深色（点击切换跟随系统）',
  };

  function safeStorageGet(key, fallback = '') {
    try {
      const v = localStorage.getItem(key);
      return v == null ? fallback : v;
    } catch {
      return fallback;
    }
  }

  function safeStorageSet(key, val) {
    try { localStorage.setItem(key, val); } catch {}
  }

  function safeStorageRemove(key) {
    try { localStorage.removeItem(key); } catch {}
  }

  function loadCollapsedSet() {
    try {
      const raw = JSON.parse(safeStorageGet(STORAGE_COLLAPSED, '[]'));
      return new Set(Array.isArray(raw) ? raw : []);
    } catch {
      return new Set();
    }
  }

  function saveCollapsedSet(set) {
    safeStorageSet(STORAGE_COLLAPSED, JSON.stringify([...set]));
  }

  const savedThemePref = safeStorageGet(STORAGE_THEME_PREF, 'system');
  const state = {
    meta: { passcode_required: false, refresh_interval_seconds: DEFAULT_POLL_SECONDS },
    snapshot: null,
    countdown: DEFAULT_POLL_SECONDS,
    viewMode: safeStorageGet(STORAGE_VIEW, 'full') === 'summary' ? 'summary' : 'full',
    themePref: ['system', 'light', 'dark'].includes(savedThemePref) ? savedThemePref : 'system',
    selectedModel: '',
    selectedTier: 'ALL',
    aisModelGroup: 'ALL',     // ALL | USED | G4 | G3 | IMAGE
    aisModelSort: 'default',  // default | remaining_asc
    aisAccountSort: 'default', // default | usage | issues
    aisExpandAllFull: false,
    expandedAisAccounts: new Set(),
    collapsedTrends: loadCollapsedSet(),
    chartPrefs: new Map(),    // accId -> { window, range5h, range7d, selectedGroup, userPickedGroup }
    historyCache: new Map(),  // "accId|win|range" -> { data, etag, fetchedAt, fingerprint, loading, error }
    snapshotEtags: { full: '', summary: '' },
    snapshotView: '',         // 'full' | 'summary' | ''
    lastSyncWallMs: 0,
    agFingerprints: new Map(), // accId -> sampling fingerprint
    pendingFullFetch: false,
    aisHourlyMetric: safeStorageGet(STORAGE_AIS_METRIC, 'requests') === 'tokens' ? 'tokens' : 'requests',
    aisShowTtft: safeStorageGet(STORAGE_AIS_TTFT, '1') !== '0',
    passcode: safeStorageGet(STORAGE_PASS, ''),
    fetching: false,
    gateOpen: false,
    justSyncedTimer: null,
  };

  const scrubClearers = new Map();
  let modelLabelMap = new Map();

  // ---------- DOM Helpers ----------
  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function svgEl(tag, attrs = {}) {
    const node = document.createElementNS(SVG_NS, tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (v !== undefined && v !== null) node.setAttribute(k, String(v));
    }
    return node;
  }

  function svgText(attrs, text) {
    const node = svgEl('text', attrs);
    node.textContent = text;
    return node;
  }

  function esc(s) {
    return String(s ?? '')
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  // ---------- Unified 5-Level Semantic Health Color Scale ----------
  // 100%: full (sky blue), >60%: high (emerald), 25-60%: medium (amber), 1-25%: low (coral red), 0%: empty (deep red + stripe)
  function quotaLevel(remaining) {
    const v = Number(remaining);
    if (!Number.isFinite(v) || v <= LEVEL_EPSILON) return 'empty';
    if (v >= 100 - LEVEL_EPSILON) return 'full';
    if (v > 60) return 'high';
    if (v >= 25) return 'medium';
    return 'low';
  }

  function levelColor(level) {
    switch (level) {
      case 'full': return cssVar('--q-full');
      case 'high': return cssVar('--q-high');
      case 'medium': return cssVar('--q-medium');
      case 'low': return cssVar('--q-low');
      case 'empty': return cssVar('--q-empty');
      default: return cssVar('--border-strong');
    }
  }

  function barColorClass(pct) {
    return 'bar-' + quotaLevel(pct);
  }

  function pctTextColor(pct) {
    const lvl = quotaLevel(pct);
    switch (lvl) {
      case 'full': return 'var(--q-full)';
      case 'high': return 'var(--q-high)';
      case 'medium': return 'var(--q-medium)';
      case 'low': return 'var(--q-low)';
      case 'empty': return 'var(--q-empty)';
      default: return 'var(--text-2)';
    }
  }

  // Dedicated cache-efficiency color scale (low hit rate is not an outage, never red)
  function cacheHitColor(pct) {
    if (pct == null || !Number.isFinite(pct)) return 'var(--text-3)';
    if (pct >= 80) return 'var(--q-high)';
    if (pct >= 50) return 'var(--q-full)';
    if (pct >= 20) return 'var(--q-medium)';
    return 'var(--text-2)';
  }

  function buildCacheHitTitle(h) {
    if (!h) return '';
    const parts = [];
    if (h.cache_hit_rate_24h != null && Number.isFinite(h.cache_hit_rate_24h)) {
      parts.push(`近 24h 命中率 ${fmtPct(h.cache_hit_rate_24h, 1)}`);
    }
    if (h.input_tokens > 0) {
      parts.push(`累计命中 ${fmtCompact(h.cached_tokens || 0)} / 总输入 ${fmtCompact(h.input_tokens)}`);
    }
    if (h.cache_saved_cost > 0) {
      let saved = `缓存约省 ${fmtCost(h.cache_saved_cost)}`;
      if (h.cache_saved_cost_24h > 0) {
        saved += `（24h 省 ${fmtCost(h.cache_saved_cost_24h)}）`;
      }
      parts.push(saved);
    }
    if (h.success_rate != null && Number.isFinite(h.success_rate)) {
      parts.push(`累计成功率 ${fmtPct(h.success_rate, 1)}`);
    }
    return parts.join(' · ');
  }

  function buildCostValueTitle(h) {
    if (!h || !(h.cache_saved_cost > 0)) return '';
    return `按官方 API 单价折算计费 ${fmtCost(h.total_cost)} · 缓存命中累计约省 ${fmtCost(h.cache_saved_cost)}`;
  }

  function buildPoolCacheKpiTitle(summary) {
    if (!summary) return '';
    const parts = [];
    if (summary.weighted_cache_hit_rate_24h != null && Number.isFinite(summary.weighted_cache_hit_rate_24h)) {
      parts.push(`近 24h 加权缓存命中率 ${fmtPct(summary.weighted_cache_hit_rate_24h, 1)}`);
    }
    if (summary.total_cache_saved_cost > 0) {
      parts.push(`缓存命中累计约省 ${fmtCost(summary.total_cache_saved_cost)}`);
    }
    if (summary.weighted_success_rate != null && Number.isFinite(summary.weighted_success_rate)) {
      parts.push(`加权总成功率 ${fmtPct(summary.weighted_success_rate, 1)}`);
    }
    return parts.join(' · ');
  }

  function healthBadgeClass(pct) {
    const lvl = quotaLevel(pct);
    switch (lvl) {
      case 'full': return 'badge-full';
      case 'high': return 'badge-green';
      case 'medium': return 'badge-amber';
      case 'low': return 'badge-red';
      case 'empty': return 'badge-empty';
      default: return 'badge-slate';
    }
  }

  function familyPalette(group) {
    const g = String(group || '').toLowerCase();
    if (g === 'claude') {
      return {
        a: cssVar('--fam-claude-a') || '#7c3aed',
        b: cssVar('--fam-claude-b') || '#db2777',
        area: cssVar('--fam-claude-area') || 'rgba(139,92,246,0.22)',
      };
    }
    if (g === 'argon') {
      return {
        a: cssVar('--fam-argon-a') || '#0d9488',
        b: cssVar('--fam-argon-b') || '#059669',
        area: cssVar('--fam-argon-area') || 'rgba(20,184,166,0.22)',
      };
    }
    return {
      a: cssVar('--fam-gemini-a') || '#0284c7',
      b: cssVar('--fam-gemini-b') || '#2563eb',
      area: cssVar('--fam-gemini-area') || 'rgba(14,165,233,0.24)',
    };
  }

  function windowDotColor(w) {
    const g = String(w.group || '').toLowerCase();
    if (g === 'claude') return 'var(--fam-claude-a)';
    if (g === 'argon') return 'var(--fam-argon-a)';
    return 'var(--fam-gemini-a)';
  }

  // ---------- 3-State Theme System + Circular View Transition ----------
  const colorSchemeMedia = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;

  function resolveTheme(pref) {
    if (pref === 'dark') return 'dark';
    if (pref === 'light') return 'light';
    return (colorSchemeMedia && colorSchemeMedia.matches) ? 'dark' : 'light';
  }

  function syncThemeButtonsUI() {
    const pref = state.themePref;
    document.querySelectorAll('[data-theme-icon]').forEach(el => {
      el.innerHTML = THEME_ICONS[pref] || THEME_ICONS.system;
    });
    document.querySelectorAll('[data-theme-label]').forEach(el => {
      el.textContent = THEME_LABELS[pref] || '自动';
    });
    ['btn-theme', 'btn-theme-gate'].forEach(id => {
      const btn = document.getElementById(id);
      if (!btn) return;
      const tip = THEME_TITLES[pref] || THEME_TITLES.system;
      btn.setAttribute('title', tip);
      btn.setAttribute('aria-label', tip);
    });
  }

  function applyThemePref(pref, redraw = true) {
    state.themePref = pref;
    const resolved = resolveTheme(pref);
    const root = document.documentElement;
    root.dataset.theme = resolved;
    root.dataset.themePref = pref;
    root.style.colorScheme = resolved;
    safeStorageSet(STORAGE_THEME_PREF, pref);
    const meta = document.getElementById('meta-theme-color');
    if (meta) meta.setAttribute('content', resolved === 'dark' ? '#090d16' : '#f1f5f9');
    syncThemeButtonsUI();
    if (redraw && state.snapshot && !state.gateOpen) {
      redrawVisibleCharts();
    }
  }

  function cycleThemeWithTransition(btnEl) {
    const order = ['system', 'light', 'dark'];
    const nextPref = order[(order.indexOf(state.themePref) + 1) % order.length];
    const prevResolved = resolveTheme(state.themePref);
    const nextResolved = resolveTheme(nextPref);
    const reducedMotion = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    if (prevResolved !== nextResolved && typeof document.startViewTransition === 'function' && !reducedMotion && btnEl) {
      const rect = btnEl.getBoundingClientRect();
      const cx = rect.left + rect.width / 2;
      const cy = rect.top + rect.height / 2;
      const maxR = Math.hypot(Math.max(cx, window.innerWidth - cx), Math.max(cy, window.innerHeight - cy));
      try {
        const vt = document.startViewTransition(() => {
          applyThemePref(nextPref, true);
        });
        vt.ready.then(() => {
          document.documentElement.animate(
            { clipPath: [`circle(0px at ${cx}px ${cy}px)`, `circle(${maxR}px at ${cx}px ${cy}px)`] },
            { duration: 420, easing: 'cubic-bezier(0.22, 1, 0.36, 1)', pseudoElement: '::view-transition-new(root)' }
          );
        }).catch(() => {});
        return;
      } catch {}
    }
    applyThemePref(nextPref, true);
  }

  if (colorSchemeMedia) {
    const onSchemeChange = () => {
      if (state.themePref === 'system') applyThemePref('system', true);
    };
    if (typeof colorSchemeMedia.addEventListener === 'function') {
      colorSchemeMedia.addEventListener('change', onSchemeChange);
    } else if (typeof colorSchemeMedia.addListener === 'function') {
      colorSchemeMedia.addListener(onSchemeChange);
    }
  }

  // ---------- Formatters ----------
  function fmtCompact(n) {
    if (n == null || !Number.isFinite(n)) return '—';
    const abs = Math.abs(n);
    if (abs >= 1e9) return (n / 1e9).toFixed(2).replace(/\.?0+$/, '') + 'B';
    if (abs >= 1e6) return (n / 1e6).toFixed(abs >= 1e8 ? 1 : 2).replace(/\.?0+$/, '') + 'M';
    if (abs >= 1e3) return (n / 1e3).toFixed(abs >= 1e5 ? 0 : 1).replace(/\.?0+$/, '') + 'K';
    return Math.round(n).toLocaleString('en-US');
  }

  function fmtInt(n) {
    if (n == null || !Number.isFinite(n)) return '—';
    return Math.round(n).toLocaleString('en-US');
  }

  function fmtCost(v) {
    if (v == null || !Number.isFinite(v)) return '—';
    if (v >= 1000) return '$' + (v / 1000).toFixed(2) + 'K';
    return '$' + v.toFixed(2);
  }

  function fmtPct(v, decimals = 2) {
    if (v == null || !Number.isFinite(v)) return '—';
    if (v >= 100) return '100%';
    if (v <= 0) return '0%';
    if (v > 99.99 && v < 100) return '99.99%';
    return v.toFixed(decimals).replace(/\.?0+$/, '') + '%';
  }

  function fmtCpampRate(v) {
    if (v == null || !Number.isFinite(v)) return '--';
    const rounded = v.toFixed(1);
    return (rounded.endsWith('.0') ? rounded.slice(0, -2) : rounded) + '%';
  }

  function fmtRoundPct(v) {
    return v == null || !Number.isFinite(v) ? '—' : Math.round(v) + '%';
  }

  function fmtDiffPct(d) {
    if (d == null || !Number.isFinite(d)) return '—';
    if (d > 0) return '+' + Math.abs(d) + '%';
    if (d < 0) return '−' + Math.abs(d) + '%';
    return '0%';
  }

  function fmtMs(ms) {
    if (!ms || ms <= 0) return '—';
    if (ms >= 10000) return (ms / 1000).toFixed(1) + 's';
    if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
    return Math.round(ms) + 'ms';
  }

  function pad2(n) { return String(n).padStart(2, '0'); }

  function toShanghaiParts(ms) {
    const d = new Date(ms + 8 * 3600 * 1000);
    return {
      M: d.getUTCMonth() + 1,
      D: d.getUTCDate(),
      h: d.getUTCHours(),
      m: d.getUTCMinutes(),
      s: d.getUTCSeconds(),
    };
  }

  function fmtClock(ms) {
    if (!ms) return '—';
    const p = toShanghaiParts(ms);
    return `${pad2(p.h)}:${pad2(p.m)}:${pad2(p.s)}`;
  }

  function fmtHM(ms) {
    if (!ms) return '—';
    const p = toShanghaiParts(ms);
    return `${pad2(p.h)}:${pad2(p.m)}`;
  }

  function fmtDay(ms) {
    if (!ms) return '—';
    const p = toShanghaiParts(ms);
    return `${pad2(p.M)}/${pad2(p.D)}`;
  }

  function fmtDateTime(ms) {
    if (!ms) return '—';
    const p = toShanghaiParts(ms);
    return `${pad2(p.M)}-${pad2(p.D)} ${pad2(p.h)}:${pad2(p.m)}`;
  }

  function pollIntervalSeconds() {
    const raw = Number(state.snapshot?.refresh_interval_seconds || state.meta?.refresh_interval_seconds || DEFAULT_POLL_SECONDS);
    if (!Number.isFinite(raw) || raw < 5) return DEFAULT_POLL_SECONDS;
    return Math.min(3600, Math.round(raw));
  }

  function fmtSampleAge(observedMs) {
    if (!observedMs || observedMs <= 0) return '';
    const diffSec = Math.max(0, Math.floor((Date.now() - observedMs) / 1000));
    if (diffSec < 60) return '刚刚采样';
    const mins = Math.floor(diffSec / 60);
    if (mins < 60) return `采样 ${mins}m 前`;
    const h = Math.floor(mins / 60);
    const rm = mins % 60;
    if (h < 24) return rm > 0 ? `采样 ${h}h${rm}m 前` : `采样 ${h}h 前`;
    return `采样 ${Math.floor(h / 24)}d 前`;
  }

  function fmtCompactCountdown(targetMs) {
    if (!targetMs || targetMs <= 0) return '—';
    const diffMs = targetMs - Date.now();
    if (diffMs <= 0) return '即将重置';
    const mins = Math.round(diffMs / 60000);
    if (mins < 60) return `${mins}m`;
    const h = Math.floor(mins / 60);
    if (h < 24) return `${h}h ${pad2(mins % 60)}m`;
    return `${Math.floor(h / 24)}d ${h % 24}h`;
  }

  function fmtCountdown(targetMs, suffix = '后重置') {
    if (!targetMs || targetMs <= 0) return '待采样';
    const diff = Math.floor((targetMs - Date.now()) / 1000);
    if (diff <= 0) return '即将重置';
    const d = Math.floor(diff / 86400);
    const h = Math.floor((diff % 86400) / 3600);
    const m = Math.floor((diff % 3600) / 60);
    const s = diff % 60;
    if (d > 0) return `${d}天 ${h}小时${suffix}`;
    if (h > 0) return `${h}小时 ${pad2(m)}分${suffix}`;
    if (m > 0) return `${m}分 ${pad2(s)}秒${suffix}`;
    return `${s}秒${suffix}`;
  }

  function fmtHMS(targetMs) {
    if (!targetMs || targetMs <= 0) return '--:--:--';
    const diff = Math.max(0, Math.floor((targetMs - Date.now()) / 1000));
    const h = Math.floor(diff / 3600);
    const m = Math.floor((diff % 3600) / 60);
    const s = diff % 60;
    return `${pad2(h)}:${pad2(m)}:${pad2(s)}`;
  }

  function rateToColor(rate) {
    const t = Math.max(0, Math.min(1, rate));
    const seg = t < 0.5 ? 0 : 1;
    const localT = seg === 0 ? t * 2 : (t - 0.5) * 2;
    const from = RATE_COLOR_STOPS[seg];
    const to = RATE_COLOR_STOPS[seg + 1];
    const r = Math.round(from.r + (to.r - from.r) * localT);
    const g = Math.round(from.g + (to.g - from.g) * localT);
    const b = Math.round(from.b + (to.b - from.b) * localT);
    return {
      bg: `rgb(${r}, ${g}, ${b})`,
      glow: `rgba(${r}, ${g}, ${b}, 0.5)`,
    };
  }

  function tierBadgeClass(tier) {
    const t = String(tier || '').toLowerCase();
    if (t === 'ultra') return 'badge-ultra';
    if (t === 'pro') return 'badge-pro';
    return 'badge-free';
  }

  function statusBadgeClass(st) {
    switch (st) {
      case 'available':
      case 'ready':
        return 'badge-green';
      case 'busy':
        return 'badge-blue';
      case 'limited':
      case 'cooldown':
        return 'badge-amber';
      case 'exhausted':
      case 'exhausted_requests':
      case 'exhausted_tokens':
      case 'auth_required':
      case 'unavailable':
      case 'restricted':
        return 'badge-red';
      default:
        return 'badge-slate';
    }
  }

  // ---------- Gate & Shell Visibility + Shimmer Skeleton ----------
  function showGate(errMsg = '') {
    state.gateOpen = true;
    const gate = document.getElementById('passcode-gate');
    const shell = document.getElementById('app-shell');
    const errEl = document.getElementById('passcode-err');
    const input = document.getElementById('passcode-input');
    const wrap = document.getElementById('gate-input-wrap');
    if (shell) shell.hidden = true;
    if (gate) gate.hidden = false;
    if (errEl) errEl.textContent = errMsg;
    if (wrap) wrap.classList.toggle('has-error', !!errMsg);
    if (input) setTimeout(() => input.focus(), 40);
  }

  function showShell() {
    state.gateOpen = false;
    const gate = document.getElementById('passcode-gate');
    const shell = document.getElementById('app-shell');
    if (gate) gate.hidden = true;
    if (shell) shell.hidden = false;
  }

  function triggerGateShake(msg) {
    const card = document.getElementById('gate-card');
    const wrap = document.getElementById('gate-input-wrap');
    const errEl = document.getElementById('passcode-err');
    if (errEl) errEl.textContent = msg;
    if (wrap) wrap.classList.add('has-error');
    if (card) {
      card.classList.remove('shake');
      void card.offsetWidth;
      card.classList.add('shake');
    }
  }

  function setGateLoading(loading) {
    const btn = document.getElementById('btn-passcode-submit');
    if (!btn) return;
    btn.disabled = loading;
    const spinner = btn.querySelector('.gate-spinner');
    const text = btn.querySelector('.gate-submit-text');
    if (spinner) spinner.hidden = !loading;
    if (text) text.textContent = loading ? '正在验证…' : '验证并进入看板';
  }

  function renderSkeleton() {
    const root = document.getElementById('app-root');
    if (!root) return;
    root.innerHTML = `
      <section class="section" aria-busy="true" aria-label="加载中">
        <div class="section-head">
          <div class="skel-block" style="width:210px;height:22px;"></div>
          <div class="skel-block" style="width:100px;height:18px;"></div>
        </div>
        <div class="skel-card" style="margin-bottom:14px;display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap;">
          <div style="display:flex;align-items:center;gap:12px;">
            <div class="skel-block" style="width:38px;height:38px;border-radius:11px;"></div>
            <div>
              <div class="skel-block" style="width:120px;height:16px;margin-bottom:6px;"></div>
              <div class="skel-block" style="width:150px;height:13px;"></div>
            </div>
          </div>
          <div class="skel-block" style="width:320px;max-width:100%;height:38px;border-radius:10px;"></div>
        </div>
        <div class="ag-bento">
          <div class="ag-bento-left">
            <div class="skel-card"><div class="skel-block" style="height:82px;"></div></div>
            <div class="skel-card"><div class="skel-block" style="height:82px;"></div></div>
            <div class="skel-card"><div class="skel-block" style="height:54px;"></div></div>
          </div>
          <div class="ag-bento-right">
            <div class="skel-card" style="height:100%;min-height:290px;">
              <div class="skel-block" style="height:28px;margin-bottom:14px;width:65%;"></div>
              <div class="skel-block" style="height:220px;"></div>
            </div>
          </div>
        </div>
      </section>
      <section class="section" aria-busy="true">
        <div class="section-head">
          <div class="skel-block" style="width:230px;height:22px;"></div>
          <div class="skel-block" style="width:140px;height:22px;border-radius:999px;"></div>
        </div>
        <div class="kpi-grid kpi-grid-3">
          <div class="skel-card"><div class="skel-block" style="height:78px;"></div></div>
          <div class="skel-card"><div class="skel-block" style="height:78px;"></div></div>
          <div class="skel-card"><div class="skel-block" style="height:78px;"></div></div>
        </div>
        <div class="skel-card" style="margin-bottom:14px;"><div class="skel-block" style="height:128px;"></div></div>
        <div class="skel-card"><div class="skel-block" style="height:220px;"></div></div>
      </section>
    `;
  }

  // ---------- Chart Prefs & History Cache ----------
  function getChartPref(accId) {
    if (!state.chartPrefs.has(accId)) {
      state.chartPrefs.set(accId, {
        window: '5h',
        range5h: '24h',
        range7d: '8d',
        selectedGroup: 'gemini',
        userPickedGroup: false,
      });
    }
    return state.chartPrefs.get(accId);
  }

  function currentRangeOf(pref) {
    return pref.window === '7d' ? pref.range7d : pref.range5h;
  }

  function historyCacheKey(accId, win, range) {
    return `${accId}|${win}|${range}`;
  }

  // ---------- API Layer & Flicker-Free Polling ----------
  function authHeaders() {
    const h = {};
    if (state.passcode) h['X-View-Passcode'] = state.passcode;
    return h;
  }

  function setRefreshSpinning(spinning) {
    const svg = document.querySelector('#btn-refresh .refresh-svg');
    if (svg) svg.classList.toggle('is-spinning', spinning);
  }

  function flashJustSynced() {
    const el = document.getElementById('sync-meta');
    if (!el) return;
    el.classList.add('just-synced');
    clearTimeout(state.justSyncedTimer);
    state.justSyncedTimer = setTimeout(() => el.classList.remove('just-synced'), 1400);
  }

  function updateSyncMeta() {
    const el = document.getElementById('sync-meta');
    if (!el) return;
    if (!state.snapshot) {
      el.textContent = '同步中…';
      return;
    }
    const clock = fmtClock(state.snapshot.generated_at_ms);
    const interval = pollIntervalSeconds();
    el.textContent = `${clock} · ${state.countdown}s`;
    el.setAttribute('title', `快照同步于 ${clock} · 每 ${interval} 秒静默刷新（约 ${state.countdown} 秒后）`);
  }

  const HISTORY_CACHE_TTL_MS = 5 * 60 * 1000;

  function desiredSnapshotView() {
    return state.viewMode === 'summary' ? 'summary' : 'full';
  }

  function needsFullAccountsFetch() {
    return state.viewMode === 'full'
      && !!state.snapshot?.aistudio?.online
      && !Array.isArray(state.snapshot.aistudio.accounts);
  }

  function accountHistoryFingerprint(acc) {
    if (!acc) return '';
    const wins = (acc.windows || [])
      .map(w => `${w.key}:${w.observed_at_ms || 0}:${w.reset_at_ms || 0}:${w.remaining_percent ?? ''}:${w.stale ? 1 : 0}`)
      .sort()
      .join('|');
    return `${acc.id}|${acc.has_history ? 1 : 0}|${wins}`;
  }

  function invalidateAccountOtherHistoryEntries(accId, keepKey) {
    const prefix = `${accId}|`;
    for (const [key, entry] of state.historyCache.entries()) {
      if (key.startsWith(prefix) && key !== keepKey && entry) {
        entry.fetchedAt = 0;
      }
    }
  }

  function syncCachedHistoriesFromSnapshot(snap, onlyTimeBounds = false) {
    if (!snap) return;
    const toSec = Math.floor((snap.generated_at_ms || Date.now()) / 1000);
    if (!(toSec > 0)) return;

    const accountsById = new Map();
    if (!onlyTimeBounds) {
      for (const acc of (snap.antigravity?.accounts || [])) {
        accountsById.set(acc.id, acc);
      }
    }

    for (const entry of state.historyCache.values()) {
      if (!entry || !entry.data) continue;
      const data = entry.data;
      const span = Math.max(3600, (data.to_sec || 0) - (data.from_sec || 0));
      if (toSec > (data.to_sec || 0)) {
        data.to_sec = toSec;
        data.from_sec = toSec - span;
      }
      const acc = !onlyTimeBounds ? accountsById.get(data.account_id) : null;
      const winByKey = new Map();
      if (acc) {
        for (const w of (acc.windows || [])) {
          winByKey.set(w.key, w);
        }
      }

      for (const s of (data.series || [])) {
        const pts = s.points || [];
        const lastPt = pts.length > 0 ? pts[pts.length - 1] : null;
        if (onlyTimeBounds) {
          if (Array.isArray(s.estimated_point) && s.estimated_point.length >= 2) {
            if (lastPt && toSec > lastPt[0] + 20 && (s.reset_sec === 0 || toSec < s.reset_sec)) {
              s.estimated_point = [toSec, s.estimated_point[1]];
            }
          }
          continue;
        }
        const w = winByKey.get(s.key);
        if (!w) continue;
        if (typeof w.remaining_percent === 'number' && Number.isFinite(w.remaining_percent)) {
          s.remaining = w.remaining_percent;
        }
        if (w.observed_at_ms > 0) {
          s.observed_sec = Math.floor(w.observed_at_ms / 1000);
        }
        const postReqs = Number(w.post_sample_requests || 0);
        const postCost = Number(w.post_sample_cost_usd || 0);
        const postTok = Number(w.post_sample_tokens || 0);
        const hasEstRem = typeof w.estimated_remaining_percent === 'number' && Number.isFinite(w.estimated_remaining_percent);

        if (!w.stale && lastPt && toSec > lastPt[0] + 20 && lastPt[1] > 0 && (s.reset_sec === 0 || toSec < s.reset_sec)) {
          if (hasEstRem && w.estimated_remaining_percent < lastPt[1]) {
            s.estimated_point = [toSec, Math.round(w.estimated_remaining_percent * 1000) / 1000];
            s.post_sample_requests = postReqs;
          } else if (postReqs === 0 && postCost === 0 && postTok === 0) {
            if (s.post_sample_requests != null || snap.antigravity?.cpamp_status === 'ok') {
              s.post_sample_requests = 0;
              s.estimated_point = [toSec, lastPt[1]];
            }
          } else if (postReqs > 0) {
            s.post_sample_requests = postReqs;
            delete s.estimated_point;
          }
        } else if (w.stale) {
          delete s.estimated_point;
        }
      }
    }
  }

  async function fetchSnapshot(manual = false, fresh = false, opts = {}) {
    if (state.fetching) {
      if (opts.forceView === 'full') state.pendingFullFetch = true;
      return { ok: false, status: 0 };
    }
    state.fetching = true;
    if (manual) setRefreshSpinning(true);

    const view = opts.forceView || desiredSnapshotView();

    try {
      const qs = new URLSearchParams();
      if (view === 'summary') qs.set('view', 'summary');
      if (fresh) qs.set('fresh', '1');
      const query = qs.toString();
      const url = query ? `/api/snapshot?${query}` : '/api/snapshot';

      const headers = authHeaders();
      const canUseEtag = !opts.bypassIfNoneMatch && !!state.snapshot && state.snapshotView === view && !!state.snapshotEtags[view];
      if (canUseEtag) {
        headers['If-None-Match'] = state.snapshotEtags[view];
      }

      const resp = await fetch(url, { headers, cache: 'no-store' });
      if (resp.status === 401) {
        state.snapshotEtags = { full: '', summary: '' };
        state.snapshotView = '';
        const hadSaved = !!safeStorageGet(STORAGE_PASS, '');
        if (!manual && hadSaved) {
          safeStorageRemove(STORAGE_PASS);
          state.passcode = '';
          showGate('本地保存的访问口令已失效，请重新输入');
        } else if (!state.gateOpen) {
          showGate('');
        }
        return { ok: false, status: 401 };
      }

      if (resp.status === 304 && state.snapshot) {
        const nextEtag = resp.headers.get('ETag');
        if (nextEtag) state.snapshotEtags[view] = nextEtag;
        const headerGenMs = Number(resp.headers.get('X-Generated-At-Ms'));
        if (Number.isFinite(headerGenMs) && headerGenMs > 0) {
          state.snapshot.generated_at_ms = headerGenMs;
        }
        state.lastSyncWallMs = Date.now();
        state.countdown = pollIntervalSeconds();
        syncCachedHistoriesFromSnapshot(state.snapshot, true);
        if (manual) {
          await refreshVisibleHistories(true, { skipRender: false });
          flashJustSynced();
        }
        return { ok: true, status: 304 };
      }

      if (!resp.ok) throw new Error('HTTP ' + resp.status);

      const nextEtag = resp.headers.get('ETag') || '';
      const nextSnap = await resp.json();
      const hadPrev = !!state.snapshot;

      const changedHistoryAccIds = new Set();
      const nextFingerprints = new Map();
      for (const acc of (nextSnap.antigravity?.accounts || [])) {
        const fp = accountHistoryFingerprint(acc);
        nextFingerprints.set(acc.id, fp);
        if (hadPrev && state.agFingerprints.get(acc.id) !== fp) {
          changedHistoryAccIds.add(acc.id);
        }
      }
      state.agFingerprints = nextFingerprints;

      state.snapshot = nextSnap;
      state.snapshotView = view;
      if (nextEtag) state.snapshotEtags[view] = nextEtag;
      state.lastSyncWallMs = Date.now();
      if (nextSnap.refresh_interval_seconds > 0) {
        state.meta.refresh_interval_seconds = nextSnap.refresh_interval_seconds;
      }
      state.countdown = pollIntervalSeconds();

      // Build model friendly name lookup map
      modelLabelMap = new Map();
      for (const m of (nextSnap.aistudio?.models || [])) {
        modelLabelMap.set(m.id, m.label || m.id);
      }

      if (state.gateOpen) showShell();

      // Mark cached secondary ranges stale only for accounts whose sampling fingerprint changed
      for (const accId of changedHistoryAccIds) {
        const pref = getChartPref(accId);
        invalidateAccountOtherHistoryEntries(accId, historyCacheKey(accId, pref.window, currentRangeOf(pref)));
      }

      // Sync post-sample live estimates & time bounds into cached history without extra HTTP requests
      syncCachedHistoriesFromSnapshot(nextSnap, false);

      // Only refetch /api/antigravity/history on manual refresh or real sampling fingerprint change
      if (hadPrev) {
        if (manual) {
          await refreshVisibleHistories(true, { skipRender: true });
        } else if (changedHistoryAccIds.size > 0) {
          await refreshVisibleHistories(true, { skipRender: true, onlyAccIds: changedHistoryAccIds });
        }
      }

      const savedScrollY = window.scrollY;
      render();
      if (Math.abs(window.scrollY - savedScrollY) > 2) {
        window.scrollTo(0, savedScrollY);
      }

      if (manual) flashJustSynced();
      return { ok: true, status: 200 };
    } catch {
      if (!state.snapshot && !state.gateOpen) {
        const root = document.getElementById('app-root');
        if (root) {
          root.innerHTML = `<div class="empty-box">暂时无法获取实时状态快照，将在稍后自动重试…</div>`;
        }
      }
      return { ok: false, status: -1 };
    } finally {
      state.fetching = false;
      setRefreshSpinning(false);
      updateSyncMeta();
      if (state.pendingFullFetch) {
        state.pendingFullFetch = false;
        if (needsFullAccountsFetch()) {
          setTimeout(() => fetchSnapshot(false, false, { forceView: 'full', bypassIfNoneMatch: true }), 0);
        }
      }
    }
  }

  async function fetchAccountHistory(accId, win, range, force = false, opts = {}) {
    const key = historyCacheKey(accId, win, range);
    const cached = state.historyCache.get(key);
    const now = Date.now();
    const curFp = state.agFingerprints.get(accId) || '';
    if (
      !force
      && cached
      && cached.data
      && cached.fetchedAt > 0
      && now - cached.fetchedAt < HISTORY_CACHE_TTL_MS
      && cached.fingerprint === curFp
    ) {
      return cached.data;
    }
    if (!force && cached && cached.loading) return cached.data || null;

    state.historyCache.set(key, {
      data: cached?.data || null,
      etag: cached?.etag || '',
      fetchedAt: cached?.fetchedAt || 0,
      fingerprint: cached?.fingerprint || '',
      loading: true,
      error: null,
    });

    try {
      const qs = new URLSearchParams({ account: accId, window: win, range });
      const headers = authHeaders();
      if (cached?.etag && cached?.data) {
        headers['If-None-Match'] = cached.etag;
      }
      const resp = await fetch(`/api/antigravity/history?${qs.toString()}`, {
        headers,
        cache: 'no-store',
      });
      if (resp.status === 401) {
        showGate('访问口令验证已过期，请重新输入');
        state.historyCache.set(key, {
          data: cached?.data || null,
          etag: '',
          fetchedAt: 0,
          fingerprint: '',
          loading: false,
          error: '需要口令',
        });
        return null;
      }
      if (resp.status === 304 && cached?.data) {
        const nextEtag = resp.headers.get('ETag') || cached.etag || '';
        const toSec = Number(resp.headers.get('X-History-To-Sec'));
        if (Number.isFinite(toSec) && toSec > 0) {
          const span = Math.max(3600, (cached.data.to_sec || 0) - (cached.data.from_sec || 0));
          cached.data.to_sec = toSec;
          cached.data.from_sec = toSec - span;
          for (const s of (cached.data.series || [])) {
            if (Array.isArray(s.estimated_point) && s.estimated_point.length >= 2) {
              s.estimated_point = [toSec, s.estimated_point[1]];
            }
          }
        }
        state.historyCache.set(key, {
          data: cached.data,
          etag: nextEtag,
          fetchedAt: Date.now(),
          fingerprint: curFp,
          loading: false,
          error: null,
        });
        if (state.snapshot) syncCachedHistoriesFromSnapshot(state.snapshot, false);
        if (!opts.skipRender) renderAccountHistoryPanel(accId);
        return cached.data;
      }
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      const nextEtag = resp.headers.get('ETag') || '';
      const data = await resp.json();
      state.historyCache.set(key, {
        data,
        etag: nextEtag,
        fetchedAt: Date.now(),
        fingerprint: curFp,
        loading: false,
        error: null,
      });
      if (!opts.skipRender) renderAccountHistoryPanel(accId);
      return data;
    } catch {
      state.historyCache.set(key, {
        data: cached?.data || null,
        etag: cached?.etag || '',
        fetchedAt: cached?.fetchedAt || 0,
        fingerprint: cached?.fingerprint || '',
        loading: false,
        error: '历史数据暂不可用',
      });
      if (!opts.skipRender) renderAccountHistoryPanel(accId);
      return null;
    }
  }

  async function refreshVisibleHistories(force = false, opts = {}) {
    const ag = state.snapshot?.antigravity;
    if (!ag || !ag.accounts) return;
    const isSingle = ag.accounts.length === 1;
    const onlyAccIds = opts.onlyAccIds || null;
    const jobs = [];
    for (const acc of ag.accounts) {
      if (!acc.has_history) continue;
      if (onlyAccIds && !onlyAccIds.has(acc.id)) continue;
      if (!isSingle && state.collapsedTrends.has(acc.id)) continue;
      const pref = getChartPref(acc.id);
      jobs.push(fetchAccountHistory(acc.id, pref.window, currentRangeOf(pref), force, opts));
    }
    if (jobs.length) await Promise.allSettled(jobs);
  }

  // ---------- Top Bar Badges ----------
  function renderTopBadges() {
    const snap = state.snapshot;
    const container = document.getElementById('pool-badges');
    if (!snap || !container) return;
    const ag = snap.antigravity;
    const ais = snap.aistudio;
    const agLabel = ag.status === 'online' ? '在线' : ag.status === 'degraded' ? '降级' : '离线';
    const aisLabel = ais.status === 'online' ? '在线' : ais.status === 'degraded' ? '降级' : '离线';

    container.innerHTML = `
      <a class="status-pill ${esc(ag.status)}" href="#section-antigravity" title="跳转至 Antigravity 号池（${esc(ag.cpamp_message || agLabel)}）">
        <span class="dot"></span><span>Antigravity</span>
      </a>
      <a class="status-pill ${esc(ais.status)}" href="#section-aistudio" title="跳转至 AIStudio2API 号池（${esc(ais.status_message || aisLabel)}）">
        <span class="dot"></span><span>AIStudio2API</span>
      </a>
    `;
  }

  // ---------- Render Antigravity Section ----------
  function filterAccountWindows(acc) {
    const isUltra = String(acc.tier || '').toLowerCase() === 'ultra';
    return (acc.windows || []).filter(w => {
      if (isUltra && (w.group === 'argon' || String(w.key || '').includes('argon'))) return false;
      return true;
    });
  }

  function isDormantWindow(w) {
    return (
      w.remaining_percent >= 100 - LEVEL_EPSILON &&
      !w.has_current_usage &&
      !(w.current_requests > 0) &&
      !(w.current_tokens > 0) &&
      !(w.current_cost > 0)
    );
  }

  function renderWindowsSmart(acc) {
    const windows = filterAccountWindows(acc);
    if (!windows.length) {
      return `<div class="empty-box" style="padding:18px;">暂无配额采样数据</div>`;
    }

    const activeWins = [];
    const dormantWins = [];
    for (const w of windows) {
      if (isDormantWindow(w)) dormantWins.push(w);
      else activeWins.push(w);
    }

    const activeHtml = activeWins.map(w => {
      const pct = Math.max(0, Math.min(100, w.remaining_percent));
      const isEmpty = quotaLevel(w.remaining_percent) === 'empty';
      const dotCol = windowDotColor(w);
      const obsTag = w.observed_at_ms > 0
        ? `<span class="win-sample-age age-target num" data-obs-ts="${w.observed_at_ms}" title="官方采样时刻: ${esc(fmtDateTime(w.observed_at_ms))}">${esc(fmtSampleAge(w.observed_at_ms))}</span>`
        : '';
      const resetHtml = w.stale
        ? `<span class="win-reset-pill">等待采样</span>`
        : `<span class="win-reset-pill" title="重置时刻: ${esc(fmtDateTime(w.reset_at_ms))}">↻ <span class="cd-target num" data-ts="${w.reset_at_ms}" data-compact="1">${esc(fmtCompactCountdown(w.reset_at_ms))}</span></span>`;

      const estRem = (w.estimated_remaining_percent != null && Number.isFinite(w.estimated_remaining_percent))
        ? Math.max(0, Math.min(pct, w.estimated_remaining_percent))
        : null;
      const estDelta = estRem != null ? (pct - estRem) : 0;
      const showLiveEst = estRem != null && estDelta >= 0.25;
      const estChip = showLiveEst
        ? `<span class="win-est-chip num" title="采样后新增 ${fmtInt(w.post_sample_requests || 0)} 次请求（${fmtCost(w.post_sample_cost_usd || 0)} · ${fmtCompact(w.post_sample_tokens || 0)}），实时估算余量约 ${fmtPct(estRem, 1)}">实时估算 ~${fmtPct(estRem, 1)}</span>`
        : '';

      const reqBadge = w.current_requests > 0
        ? `<span class="win-req-tag num">${fmtInt(w.current_requests)} 次请求</span>`
        : '';

      const usageParts = [];
      if (w.has_current_usage) {
        usageParts.push(`<span class="win-foot-item num">本窗已用 <strong>${fmtCost(w.current_cost)}</strong> · ${fmtCompact(w.current_tokens)}</span>`);
      }
      if (w.has_forecast) {
        const tag = w.forecast_basis === 'previous' ? '上期参考' : '满额预测';
        usageParts.push(`<span class="win-foot-forecast num">${tag} <strong>${fmtCost(w.forecast_cost)}</strong> · ${fmtCompact(w.forecast_tokens)}</span>`);
      }

      const barInnerHtml = showLiveEst
        ? `<div class="bar-fill ${barColorClass(estRem)}" style="width:${estRem.toFixed(2)}%"></div>
           <div class="bar-fill-delta" style="left:${estRem.toFixed(2)}%;width:${estDelta.toFixed(2)}%" title="采样后实时估算消耗约 ${estDelta.toFixed(1)}%"></div>`
        : `<div class="bar-fill ${barColorClass(w.remaining_percent)}" style="width:${pct}%"></div>`;

      return `
        <div class="win-active-card">
          <div class="win-top">
            <div class="win-title-group">
              <span class="win-dot" style="background:${dotCol}"></span>
              <span class="win-title">${esc(w.label)}</span>
              ${obsTag}
            </div>
            ${resetHtml}
          </div>
          <div class="win-main-metric">
            <div class="win-pct-group">
              <span class="win-pct-big num" style="color:${pctTextColor(w.remaining_percent)}">${fmtPct(w.remaining_percent, 2)}</span>
              ${estChip}
            </div>
            ${reqBadge}
          </div>
          <div class="bar-track ${isEmpty ? 'is-empty' : ''}">
            ${barInnerHtml}
          </div>
          ${usageParts.length ? `<div class="win-foot-grid">${usageParts.join('')}</div>` : ''}
        </div>
      `;
    }).join('');

    const dormantHtml = dormantWins.length ? `
      <div class="win-dormant-grid">
        ${dormantWins.map(w => {
          const dotCol = windowDotColor(w);
          const hasReset = !w.stale && w.reset_at_ms > Date.now();
          const resetSub = hasReset
            ? `<span class="win-dormant-reset num" title="重置时刻: ${esc(fmtDateTime(w.reset_at_ms))}">↻ <span class="cd-target" data-ts="${w.reset_at_ms}" data-compact="1">${esc(fmtCompactCountdown(w.reset_at_ms))}</span></span>`
            : `<span class="win-dormant-reset">本窗零消耗</span>`;
          return `
            <div class="win-dormant-pill" title="${esc(w.label)}：100% 满血未消耗">
              <div class="win-dormant-left">
                <span class="win-dot" style="background:${dotCol}"></span>
                <div class="win-dormant-info">
                  <span class="win-dormant-name">${esc(w.label)}</span>
                  ${resetSub}
                </div>
              </div>
              <span class="win-dormant-badge num">100% 满血</span>
            </div>
          `;
        }).join('')}
      </div>
    ` : '';

    return `<div class="win-stack">${activeHtml}${dormantHtml}</div>`;
  }

  function renderRecentStatusBlock(h) {
    if (h.has_recent_buckets && Array.isArray(h.recent_buckets) && h.recent_buckets.length > 0) {
      const buckets = h.recent_buckets;
      const totalOk = h.recent_window_success || 0;
      const totalFail = h.recent_window_failure || 0;
      const hasData = totalOk + totalFail > 0;
      const rate = h.recent_window_rate;
      const rateCls = !hasData ? '' : rate >= 90 ? 'high' : rate >= 50 ? 'medium' : 'low';
      const nowMs = state.snapshot?.generated_at_ms || Date.now();
      const bucketMs = 10 * 60 * 1000;
      const windowStartMs = nowMs - buckets.length * bucketMs;

      const blocksHtml = buckets.map((b, idx) => {
        const ok = b.success || 0;
        const fail = b.failed || 0;
        const tot = ok + fail;
        const isIdle = tot === 0;
        const bRate = isIdle ? -1 : ok / tot;
        const col = isIdle ? null : rateToColor(bRate);
        const styleAttr = isIdle ? '' : `style="background-color:${col.bg};--block-glow:${col.glow};"`;
        const posCls = idx <= 2 ? 'tip-left' : idx >= buckets.length - 3 ? 'tip-right' : '';
        const timeRange = b.time || `${fmtHM(windowStartMs + idx * bucketMs)}-${fmtHM(windowStartMs + (idx + 1) * bucketMs)}`;
        const statsHtml = tot > 0
          ? `<span class="cpamp-tip-stats num">
               <span style="color:var(--green-text);font-weight:700;">成功 ${fmtInt(ok)}</span>
               <span style="color:${fail > 0 ? 'var(--red-text)' : 'var(--text-3)'};font-weight:700;">失败 ${fmtInt(fail)}</span>
               <span style="color:var(--text-3);">(${(bRate * 100).toFixed(1).replace(/\.0$/, '')}%)</span>
             </span>`
          : `<span style="color:var(--text-3);">无请求</span>`;

        return `
          <div class="cpamp-block-wrap" data-cpamp-block="1">
            <div class="cpamp-block ${isIdle ? 'idle' : ''}" ${styleAttr}></div>
            <div class="cpamp-tip ${posCls}">
              <span class="cpamp-tip-time">${esc(timeRange)}</span>
              ${statsHtml}
            </div>
          </div>
        `;
      }).join('');

      const latestHint = h.latest_request_at_ms ? ` · 最近 ${fmtHM(h.latest_request_at_ms)}` : '';

      return `
        <div class="recent-box">
          <div class="recent-head">
            <div class="recent-title-group">
              <span class="recent-title">近期状态</span>
              <span class="recent-sub num">近 ${h.recent_window_minutes || 200}m · 10m/格${esc(latestHint)}</span>
            </div>
            <div class="recent-metrics num">
              <span class="recent-pill-ok" title="近 200 分钟成功请求数"><span class="recent-pill-dot"></span><strong>${fmtInt(totalOk)}</strong></span>
              <span class="recent-pill-fail ${totalFail > 0 ? 'has-fail' : ''}" title="近 200 分钟失败请求数"><span class="recent-pill-dot"></span><strong>${fmtInt(totalFail)}</strong></span>
            </div>
          </div>
          <div class="cpamp-status-bar">
            <div class="cpamp-blocks">${blocksHtml}</div>
            <span class="cpamp-rate ${rateCls} num">${hasData ? fmtCpampRate(rate) : '--'}</span>
          </div>
        </div>
      `;
    }

    const bars = h.recent_bars || [];
    if (bars.length > 0) {
      const barNodes = bars.map(b => {
        const tip = `${b.ok ? '成功' : '失败'}${b.status_code ? ' · HTTP ' + b.status_code : ''}${b.timestamp_ms ? ' · ' + fmtDateTime(b.timestamp_ms) : ''}`;
        return `<div class="recent-bar ${b.ok ? 'ok' : 'fail'}" title="${esc(tip)}"></div>`;
      }).join('');
      return `
        <div class="recent-box">
          <div class="recent-head">
            <span class="recent-title">近期状态（近 ${bars.length} 次）</span>
            <span class="num" style="font-weight:700;color:${pctTextColor(h.recent_success_rate ?? 100)}">${h.recent_success_count}/${bars.length} · ${fmtPct(h.recent_success_rate, 1)}</span>
          </div>
          <div class="recent-bars">${barNodes}</div>
        </div>
      `;
    }

    return `
      <div class="recent-box" style="display:flex;align-items:center;justify-content:space-between;">
        <span class="recent-title">近期状态</span>
        <span class="badge badge-amber">${esc(h.status_note || '暂无记录')}</span>
      </div>
    `;
  }

  function renderAGSingleBento(acc, ag) {
    const tierLower = String(acc.tier || 'free').toLowerCase();
    const tierCls = 'tier-' + tierLower;
    const initials = (acc.tier ? acc.tier[0].toUpperCase() : 'A') + (acc.code_name.match(/#(\d+)/)?.[1] || '1');
    const h = acc.history || {};
    const tierBadge = tierLower === 'ultra'
      ? `<span class="badge badge-ultra-hero">ULTRA</span>`
      : `<span class="badge ${tierBadgeClass(acc.tier)}">${esc(acc.tier)}</span>`;

    const hasCacheHit = h.cache_hit_rate != null && Number.isFinite(h.cache_hit_rate);
    const cacheHitText = hasCacheHit ? fmtPct(h.cache_hit_rate, 1) : '—';
    const cacheHitCol = cacheHitColor(hasCacheHit ? h.cache_hit_rate : null);
    const cacheHitTitle = buildCacheHitTitle(h);
    const costTitle = buildCostValueTitle(h);

    const metricsHtml = h.available ? `
      <div class="ag-hero-metrics">
        <div class="ag-hero-stat">
          <span class="ag-hero-stat-lbl">累计请求</span>
          <span class="ag-hero-stat-val num">${fmtCompact(h.total_requests)}</span>
        </div>
        <div class="ag-hero-stat">
          <span class="ag-hero-stat-lbl">累计 Token</span>
          <span class="ag-hero-stat-val num">${fmtCompact(h.total_tokens)}</span>
        </div>
        <div class="ag-hero-stat${costTitle ? ' has-tip' : ''}"${costTitle ? ` title="${esc(costTitle)}"` : ''}>
          <span class="ag-hero-stat-lbl">估算价值</span>
          <span class="ag-hero-stat-val num">${fmtCost(h.total_cost)}</span>
        </div>
        <div class="ag-hero-stat${cacheHitTitle ? ' has-tip' : ''}"${cacheHitTitle ? ` title="${esc(cacheHitTitle)}"` : ''}>
          <span class="ag-hero-stat-lbl">缓存命中</span>
          <span class="ag-hero-stat-val num" style="color:${cacheHitCol}">${cacheHitText}</span>
        </div>
      </div>
    ` : `<span class="badge badge-amber">${esc(h.status_note || ag.cpamp_message || '历史统计待同步')}</span>`;

    const recentHtml = (h.available || h.has_recent_buckets)
      ? renderRecentStatusBlock(h)
      : `
        <div class="recent-box" style="display:flex;align-items:center;justify-content:space-between;">
          <span class="recent-title">近期状态</span>
          <span class="badge badge-amber">${esc(h.status_note || '待接入密钥')}</span>
        </div>
      `;

    const rightChartHtml = acc.has_history
      ? `<div class="lamp-panel" data-lamp-panel="${esc(acc.id)}" data-permanent="1"></div>`
      : `<div class="lamp-panel"><div class="lamp-loading">暂无周期燃烧历史采样数据</div></div>`;

    return `
      <div class="ag-single-shell">
        <div class="ag-hero-bar">
          <div class="ag-identity">
            <div class="ag-avatar ${tierCls}">${esc(initials)}</div>
            <div class="ag-identity-text">
              <div class="ag-name-row">
                <span class="ag-name">${esc(acc.code_name)}</span>
                ${tierBadge}
                <span class="badge ${statusBadgeClass(acc.status)}">${esc(acc.status_label)}</span>
              </div>
            </div>
          </div>
          ${metricsHtml}
        </div>
        <div class="ag-bento">
          <div class="ag-bento-left">
            ${renderWindowsSmart(acc)}
            ${recentHtml}
          </div>
          <div class="ag-bento-right">
            ${rightChartHtml}
          </div>
        </div>
      </div>
    `;
  }

  function renderAGMultiCard(acc) {
    const tierLower = String(acc.tier || 'free').toLowerCase();
    const tierCls = 'tier-' + tierLower;
    const initials = (acc.tier ? acc.tier[0].toUpperCase() : 'A') + (acc.code_name.match(/#(\d+)/)?.[1] || '1');
    const h = acc.history || {};
    const tierBadge = tierLower === 'ultra'
      ? `<span class="badge badge-ultra-hero">ULTRA</span>`
      : `<span class="badge ${tierBadgeClass(acc.tier)}">${esc(acc.tier)}</span>`;

    const hasCacheHit = h.cache_hit_rate != null && Number.isFinite(h.cache_hit_rate);
    const cacheHitText = hasCacheHit ? fmtPct(h.cache_hit_rate, 1) : '—';
    const cacheHitCol = cacheHitColor(hasCacheHit ? h.cache_hit_rate : null);
    const cacheHitTitle = buildCacheHitTitle(h);
    const costTitle = buildCostValueTitle(h);

    const statsBlock = h.available ? `
      <div class="ag-stats-grid">
        <div class="ag-stat-box">
          <div class="ag-stat-label">累计请求</div>
          <div class="ag-stat-val num">${fmtCompact(h.total_requests)}</div>
        </div>
        <div class="ag-stat-box">
          <div class="ag-stat-label">累计 Token</div>
          <div class="ag-stat-val num">${fmtCompact(h.total_tokens)}</div>
        </div>
        <div class="ag-stat-box${costTitle ? ' has-tip' : ''}"${costTitle ? ` title="${esc(costTitle)}"` : ''}>
          <div class="ag-stat-label">估算价值</div>
          <div class="ag-stat-val num">${fmtCost(h.total_cost)}</div>
        </div>
        <div class="ag-stat-box${cacheHitTitle ? ' has-tip' : ''}"${cacheHitTitle ? ` title="${esc(cacheHitTitle)}"` : ''}>
          <div class="ag-stat-label">缓存命中</div>
          <div class="ag-stat-val num" style="color:${cacheHitCol}">${cacheHitText}</div>
        </div>
      </div>
    ` : '';

    const recentBlock = (h.available || h.has_recent_buckets)
      ? renderRecentStatusBlock(h)
      : `
        <div class="recent-box" style="display:flex;align-items:center;justify-content:space-between;">
          <span class="recent-title">近期状态</span>
          <span class="badge badge-amber">${esc(h.status_note || '待接入密钥')}</span>
        </div>
      `;

    const isCollapsed = state.collapsedTrends.has(acc.id);
    const trendHtml = acc.has_history ? `
      <div>
        ${isCollapsed
          ? `<button type="button" class="trend-toggle" data-trend-toggle="${esc(acc.id)}">展开周期燃烧分析 ▼</button>`
          : `<div class="lamp-panel" data-lamp-panel="${esc(acc.id)}"></div>`
        }
      </div>
    ` : '';

    return `
      <article class="ag-card">
        <div class="ag-card-head">
          <div class="ag-identity">
            <div class="ag-avatar ${tierCls}">${esc(initials)}</div>
            <div class="ag-identity-text">
              <div class="ag-name-row">
                <span class="ag-name">${esc(acc.code_name)}</span>
                ${tierBadge}
                <span class="badge ${statusBadgeClass(acc.status)}">${esc(acc.status_label)}</span>
              </div>
            </div>
          </div>
          <span class="section-sub num">${filterAccountWindows(acc).length} 个配额窗口</span>
        </div>
        ${statsBlock}
        ${renderWindowsSmart(acc)}
        ${recentBlock}
        ${trendHtml}
      </article>
    `;
  }

  function renderAntigravity(ag) {
    const tierStr = ['Ultra', 'Pro', 'Free', 'Unknown']
      .filter(t => (ag.summary.tier_counts || {})[t] > 0)
      .map(t => `<span class="badge ${tierBadgeClass(t)}">${esc(t)} ×${ag.summary.tier_counts[t]}</span>`)
      .join('');

    const cpampBadge = ag.cpamp_status === 'ok'
      ? `<span class="badge badge-green">CPAMP 已同步</span>`
      : `<span class="badge badge-amber">${esc(ag.cpamp_message || '待接入密钥')}</span>`;

    const accounts = ag.accounts || [];
    const isSingle = accounts.length === 1;

    // Multi-account mode keeps top 4-card summary strip
    let summaryHtml = '';
    if (!isSingle && accounts.length > 0) {
      const avgPills = (ag.summary.window_averages || []).map(w => `
        <span class="badge ${healthBadgeClass(w.avg_remaining_percent)} num">
          ${esc(w.label)} ${fmtPct(w.avg_remaining_percent, 1)}
        </span>
      `).join('');

      const hasPoolCacheHit = ag.summary.weighted_cache_hit_rate != null && Number.isFinite(ag.summary.weighted_cache_hit_rate);
      const poolCacheKpiTitle = buildPoolCacheKpiTitle(ag.summary);
      const poolCacheSubHtml = hasPoolCacheHit
        ? `<span>缓存命中 <strong style="color:${cacheHitColor(ag.summary.weighted_cache_hit_rate)}">${fmtPct(ag.summary.weighted_cache_hit_rate, 1)}</strong>${ag.summary.total_cache_saved_cost > 0 ? ' · 省 ' + fmtCost(ag.summary.total_cache_saved_cost) : ''}</span>`
        : '—';

      summaryHtml = `
        <div class="kpi-grid">
          <div class="kpi-card">
            <div class="kpi-label">活跃启用账号</div>
            <div class="kpi-val num">${fmtInt(ag.summary.active_accounts)}</div>
            <div class="kpi-sub">${tierStr || '无启用账号'}</div>
          </div>
          <div class="kpi-card">
            <div class="kpi-label">累计请求 / Token</div>
            <div class="kpi-val num">${ag.cpamp_status === 'ok' ? fmtCompact(ag.summary.total_requests) : '待接入'}</div>
            <div class="kpi-sub num">${ag.cpamp_status === 'ok' ? '总 Token ' + fmtCompact(ag.summary.total_tokens) : esc(ag.cpamp_message || '待接入密钥')}</div>
          </div>
          <div class="kpi-card${poolCacheKpiTitle ? ' has-tip' : ''}"${poolCacheKpiTitle ? ` title="${esc(poolCacheKpiTitle)}"` : ''}>
            <div class="kpi-label">估算价值 / 缓存命中</div>
            <div class="kpi-val num">${ag.cpamp_status === 'ok' ? fmtCost(ag.summary.total_cost) : '待接入'}</div>
            <div class="kpi-sub num">${poolCacheSubHtml}</div>
          </div>
          <div class="kpi-card">
            <div class="kpi-label">各窗口平均余量</div>
            <div class="kpi-sub" style="margin-top:4px;">${avgPills || '<span class="section-sub">暂无采样</span>'}</div>
          </div>
        </div>
      `;
    }

    let bodyHtml = '';
    if (accounts.length === 0) {
      bodyHtml = `<div class="empty-box">当前未检测到启用中的 Antigravity 账号</div>`;
    } else if (isSingle) {
      bodyHtml = renderAGSingleBento(accounts[0], ag);
    } else if (state.viewMode === 'full') {
      bodyHtml = `<div class="ag-grid">${accounts.map(renderAGMultiCard).join('')}</div>`;
    }

    return `
      <section class="section" id="section-antigravity">
        <div class="section-head">
          <div class="section-title-wrap">
            <h2 class="section-title">Antigravity 号池</h2>
            <span class="section-sub">CLIProxyAPI</span>
            ${tierStr}
            ${cpampBadge}
          </div>
          <span class="section-sub num">活跃启用 ${fmtInt(ag.summary.active_accounts)} 个</span>
        </div>
        ${summaryHtml}
        ${bodyHtml}
      </section>
    `;
  }

  // ---------- Lamplighter Math Engine ----------
  function extractCycleResetTimes(series) {
    if (!series) return [];
    if (Array.isArray(series.cycle_resets)) {
      const unique = [];
      const sorted = series.cycle_resets
        .map(Number)
        .filter(t => Number.isFinite(t) && t > 0)
        .sort((a, b) => a - b);
      for (const t of sorted) {
        if (!unique.length || unique[unique.length - 1] !== t) unique.push(t);
      }
      return unique;
    }
    const resets = Array.isArray(series.resets) ? series.resets : [];
    const pts = Array.isArray(series.points) ? series.points : [];
    const out = [];
    for (let i = 1; i < resets.length; i++) {
      const prev = resets[i - 1];
      const cur = resets[i];
      if (!Array.isArray(prev) || !Array.isArray(cur)) continue;
      const prevEff = Number(prev[1]);
      const curT = Number(cur[0]);
      const curEff = Number(cur[1]);
      if (!Number.isFinite(curT) || curT <= 0) continue;
      if (prevEff > 0 && curEff > 0 && curEff > prevEff + RESET_DRIFT) {
        out.push(curT);
      } else if (prevEff > 0 && curEff === 0) {
        let prevRem = null;
        for (let k = pts.length - 1; k >= 0; k--) {
          if (pts[k][0] < curT) {
            prevRem = pts[k][1];
            break;
          }
        }
        if (prevRem != null && prevRem < 99.999) {
          out.push(curT);
        }
      }
    }
    out.sort((a, b) => a - b);
    return out;
  }

  function hasResetInRange(t0, t1, resetTimes) {
    if (!resetTimes || !resetTimes.length) return false;
    for (let i = 0; i < resetTimes.length; i++) {
      const rt = resetTimes[i];
      if (rt > t0 && rt <= t1) return true;
      if (rt > t1) break;
    }
    return false;
  }

  function latestResetAtOrBefore(resetTimes, t) {
    if (!resetTimes || !resetTimes.length) return null;
    let best = null;
    for (let i = 0; i < resetTimes.length; i++) {
      if (resetTimes[i] <= t) best = resetTimes[i];
      else break;
    }
    return best;
  }

  function findResetAnchorTime(prevT, curT, resetsMeta, winLen) {
    if (!(winLen > 0) || !Array.isArray(resetsMeta) || !resetsMeta.length) return null;
    let eff = 0;
    for (let i = 0; i < resetsMeta.length; i++) {
      const r = resetsMeta[i];
      if (!Array.isArray(r)) continue;
      if (r[0] <= curT + RESET_DRIFT) {
        eff = Number(r[1]) || 0;
      } else {
        break;
      }
    }
    if (!(eff > 0)) return null;
    const anchorT = eff - winLen;
    if (anchorT > prevT && anchorT < curT) return anchorT;
    return null;
  }

  function cycleFloor(points, resetTimes = []) {
    let low = null;
    let prevT = null;
    return points.map(p => {
      const crossed = prevT != null && hasResetInRange(prevT, p[0], resetTimes);
      low = (low == null || crossed || p[1] - low >= RESET_JUMP) ? p[1] : Math.min(low, p[1]);
      prevT = p[0];
      return [p[0], low];
    });
  }

  function valueAt(points, t) {
    let val = null;
    for (let i = 0; i < points.length; i++) {
      if (points[i][0] <= t) val = points[i];
      else break;
    }
    return val;
  }

  function splitRuns(points, limit, end) {
    const out = [];
    let cur = [];
    for (const p of points) {
      if (cur.length && p[0] - cur[cur.length - 1][0] > limit) {
        out.push(cur);
        cur = [];
      }
      cur.push(p);
    }
    if (cur.length) out.push(cur);
    return out.map((pts, i) => {
      const last = pts[pts.length - 1][0];
      const stop = i === out.length - 1 ? Math.min(end, last + limit) : last + Math.min(limit, 300);
      return { pts, start: pts[0][0], end: Math.max(stop, last) };
    });
  }

  function splitAtResets(pts, resetTimes = [], resetsMeta = [], winLen = 0) {
    if (!pts.length) return { parts: [], resets: [] };
    const parts = [[pts[0]]];
    const resets = [];
    for (let i = 1; i < pts.length; i++) {
      const prevT = pts[i - 1][0];
      const curT = pts[i][0];
      const byResetTime = hasResetInRange(prevT, curT, resetTimes);
      const byJump = pts[i][1] - pts[i - 1][1] >= RESET_JUMP;
      if (byResetTime || byJump) {
        let resetMarkT = curT;
        let anchorPt = null;
        if (pts[i][1] < 100 - LEVEL_EPSILON) {
          const anchorT = findResetAnchorTime(prevT, curT, resetsMeta, winLen);
          if (anchorT != null) {
            resetMarkT = anchorT;
            anchorPt = [anchorT, 100];
          }
        }
        if (!byResetTime && byJump && parts.length > 1 && parts[parts.length - 1].length === 1) {
          parts[parts.length - 1] = anchorPt ? [anchorPt] : [];
          resets[resets.length - 1] = resetMarkT;
        } else {
          parts.push(anchorPt ? [anchorPt] : []);
          resets.push(resetMarkT);
        }
      }
      parts[parts.length - 1].push(pts[i]);
    }
    for (let k = 0; k + 1 < parts.length; k++) {
      const tail = parts[k][parts[k].length - 1];
      const nextStartT = parts[k + 1][0][0];
      if (nextStartT > tail[0]) {
        parts[k].push([nextStartT, tail[1]]);
      }
    }
    return { parts, resets };
  }

  // PAVA isotonic regression (monotone non-increasing within a cycle)
  function declining(pts) {
    const blocks = [];
    for (const p of pts) {
      blocks.push({ sum: p[1], n: 1 });
      while (blocks.length > 1) {
        const top = blocks[blocks.length - 1];
        const prev = blocks[blocks.length - 2];
        if (top.sum / top.n <= prev.sum / prev.n) break;
        prev.sum += top.sum;
        prev.n += top.n;
        blocks.pop();
      }
    }
    const out = [];
    let idx = 0;
    for (const b of blocks) {
      const avg = b.sum / b.n;
      for (let j = 0; j < b.n; j++, idx++) {
        out.push([pts[idx][0], avg]);
      }
    }
    return out;
  }

  function curvePoints(pts) {
    return pts.filter((p, i) => {
      if (i === 0 || i === pts.length - 1 || p[1] !== pts[i - 1][1]) return true;
      return Math.abs(pts[i + 1][1] - p[1]) > QUANTUM;
    });
  }

  // Exact evaluator matching Fritsch-Carlson monotone cubic spline in (t, pct) space
  function buildMonotoneSpline(pts) {
    const n = pts.length;
    if (n === 0) return { evalPct: () => null };
    if (n === 1) return { evalPct: () => pts[0][1] };
    if (n === 2) {
      const dt = pts[1][0] - pts[0][0];
      return {
        evalPct: (t) => {
          if (!(dt > 0)) return pts[1][1];
          const u = Math.max(0, Math.min(1, (t - pts[0][0]) / dt));
          return pts[0][1] + u * (pts[1][1] - pts[0][1]);
        },
      };
    }

    const dx = new Array(n - 1);
    const m = new Array(n - 1);
    const tg = new Array(n);
    for (let i = 0; i < n - 1; i++) {
      dx[i] = pts[i + 1][0] - pts[i][0];
      m[i] = dx[i] ? (pts[i + 1][1] - pts[i][1]) / dx[i] : 0;
    }
    tg[0] = m[0];
    tg[n - 1] = m[n - 2];
    for (let i = 1; i < n - 1; i++) {
      tg[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2;
    }
    for (let i = 0; i < n - 1; i++) {
      if (!m[i]) { tg[i] = tg[i + 1] = 0; continue; }
      const a = tg[i] / m[i];
      const b = tg[i + 1] / m[i];
      const h = a * a + b * b;
      if (h > 9) {
        const k = 3 / Math.sqrt(h);
        tg[i] = k * a * m[i];
        tg[i + 1] = k * b * m[i];
      }
    }

    return {
      evalPct: (t) => {
        if (t <= pts[0][0]) return pts[0][1];
        if (t >= pts[n - 1][0]) return pts[n - 1][1];
        let lo = 0;
        let hi = n - 2;
        while (lo < hi) {
          const mid = (lo + hi + 1) >> 1;
          if (pts[mid][0] <= t) lo = mid;
          else hi = mid - 1;
        }
        const i = lo;
        const dt = dx[i];
        if (!(dt > 0)) return pts[i + 1][1];
        const u = (t - pts[i][0]) / dt;
        const u2 = u * u;
        const u3 = u2 * u;
        const h00 = 2 * u3 - 3 * u2 + 1;
        const h10 = u3 - 2 * u2 + u;
        const h01 = -2 * u3 + 3 * u2;
        const h11 = u3 - u2;
        const val = h00 * pts[i][1] + h10 * (tg[i] * dt) + h01 * pts[i + 1][1] + h11 * (tg[i + 1] * dt);
        return Math.max(0, Math.min(100, val));
      },
    };
  }

  // Fritsch-Carlson monotone cubic spline
  function monotonePath(xy) {
    const n = xy.length;
    if (n === 0) return '';
    if (n < 3) return xy.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(2)} ${p[1].toFixed(2)}`).join('');
    const dx = new Array(n - 1);
    const m = new Array(n - 1);
    const tg = new Array(n);
    for (let i = 0; i < n - 1; i++) {
      dx[i] = xy[i + 1][0] - xy[i][0];
      m[i] = dx[i] ? (xy[i + 1][1] - xy[i][1]) / dx[i] : 0;
    }
    tg[0] = m[0];
    tg[n - 1] = m[n - 2];
    for (let i = 1; i < n - 1; i++) {
      tg[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2;
    }
    for (let i = 0; i < n - 1; i++) {
      if (!m[i]) { tg[i] = tg[i + 1] = 0; continue; }
      const a = tg[i] / m[i];
      const b = tg[i + 1] / m[i];
      const h = a * a + b * b;
      if (h > 9) {
        const k = 3 / Math.sqrt(h);
        tg[i] = k * a * m[i];
        tg[i + 1] = k * b * m[i];
      }
    }
    let d = `M${xy[0][0].toFixed(2)} ${xy[0][1].toFixed(2)}`;
    for (let i = 0; i < n - 1; i++) {
      const third = dx[i] / 3;
      d += `C${(xy[i][0] + third).toFixed(2)} ${(xy[i][1] + tg[i] * third).toFixed(2)} ${(xy[i + 1][0] - third).toFixed(2)} ${(xy[i + 1][1] - tg[i + 1] * third).toFixed(2)} ${xy[i + 1][0].toFixed(2)} ${xy[i + 1][1].toFixed(2)}`;
    }
    return d;
  }

  function paceSegments(resets, length, currentReset, now) {
    let list = (resets || []).filter(r => Array.isArray(r) && Number.isFinite(r[0]) && Number.isFinite(r[1]));
    const last = list[list.length - 1];
    const lastKnown = !!last && last[1] > 0;
    if (currentReset != null && Number.isFinite(currentReset) && (!last || now >= last[0])) {
      if (currentReset > 0 ? (!lastKnown || Math.abs(currentReset - last[1]) > RESET_DRIFT) : lastKnown) {
        list = list.concat([[now, currentReset > 0 ? currentReset : 0]]);
      }
    }
    const out = [];
    if (!(length > 0)) return out;
    list.forEach((r, i) => {
      const x = r[1];
      if (!(x > 0)) return;
      const next = list[i + 1];
      const a = Math.max(r[0], x - length);
      const b = next ? Math.min(next[0], x) : x;
      if (b > a) out.push([a, b, x, !!next && !(next[1] > 0) && next[0] <= x]);
    });
    return out;
  }

  function paceExtensions(segments, points, length, resetTimes = []) {
    const out = [];
    const pts = points || [];
    segments.forEach((s, i) => {
      const prev = segments[i - 1];
      if (prev && prev[1] === s[0] && !prev[3] && Math.abs(prev[2] - s[2]) <= RESET_DRIFT) return;
      if (s[0] <= s[2] - length + RESET_DRIFT) return;
      let start = null;
      for (let k = 1; k < pts.length && pts[k][0] <= s[0]; k++) {
        if (
          pts[k][0] >= s[2] - length - RESET_DRIFT &&
          (pts[k][1] - pts[k - 1][1] >= RESET_JUMP || hasResetInRange(pts[k - 1][0], pts[k][0], resetTimes))
        ) {
          start = pts[k][0];
        }
      }
      if (start != null && start < s[0]) out.push([start, s[0], s[2]]);
    });
    return out;
  }

  function paceAt(segments, length, t) {
    let value = null;
    for (const s of segments) {
      if (t >= s[0] && (s[3] ? t < s[1] : t <= s[1])) {
        value = 100 * (s[2] - t) / length;
      }
    }
    return value;
  }

  function extendedPaceAt(extensions, length, t) {
    let value = null;
    for (const e of extensions) {
      if (t >= e[0] && t <= e[1]) {
        value = 100 * (e[2] - t) / length;
      }
    }
    return value;
  }

  function lineFit(pts, a, b) {
    const n = b - a + 1;
    const t0 = pts[a][0];
    let st = 0, sv = 0, stt = 0, stv = 0;
    for (let i = a; i <= b; i++) {
      const dt = pts[i][0] - t0;
      st += dt; sv += pts[i][1]; stt += dt * dt; stv += dt * pts[i][1];
    }
    const d = n * stt - st * st;
    const slope = d ? (n * stv - st * sv) / d : 0;
    const base = (sv - slope * st) / n;
    let error = 0;
    for (let i = a; i <= b; i++) {
      error = Math.max(error, Math.abs(base + slope * (pts[i][0] - t0) - pts[i][1]));
    }
    return { slope, error };
  }

  function recentSlope(pts, minSpanSec = 900) {
    if (pts.length < 2) return 0;
    const pieces = [];
    for (let i = 0; i + 1 < pts.length; i++) pieces.push([i, i + 1]);
    const cost = (k) => lineFit(pts, pieces[k][0], pieces[k + 1][1]).error;
    const costs = [];
    for (let i = 0; i + 1 < pieces.length; i++) costs.push(cost(i));
    while (costs.length) {
      let best = 0;
      for (let i = 1; i < costs.length; i++) if (costs[i] < costs[best]) best = i;
      if (costs[best] > QUANTUM) break;
      pieces.splice(best, 2, [pieces[best][0], pieces[best + 1][1]]);
      costs.splice(best, 1);
      if (best < costs.length) costs[best] = cost(best);
      if (best > 0) costs[best - 1] = cost(best - 1);
    }
    const last = pieces[pieces.length - 1];
    let a = last[0];
    const b = last[1];
    const totalSpan = pts[pts.length - 1][0] - pts[0][0];
    const reqSpan = Math.min(minSpanSec, Math.max(0, totalSpan));
    while (a > 0 && (pts[b][0] - pts[a][0]) < reqSpan) {
      a--;
    }
    return lineFit(pts, a, b).slope;
  }

  function interpAtTime(pts, t) {
    if (!pts.length) return null;
    if (t <= pts[0][0]) return pts[0][1];
    const last = pts[pts.length - 1];
    if (t >= last[0]) return last[1];
    for (let i = 0; i + 1 < pts.length; i++) {
      const p0 = pts[i], p1 = pts[i + 1];
      if (t >= p0[0] && t <= p1[0]) {
        const dt = p1[0] - p0[0];
        if (!(dt > 0)) return p1[1];
        return p0[1] + ((t - p0[0]) / dt) * (p1[1] - p0[1]);
      }
    }
    return last[1];
  }

  function slidingWindowRate(pts, curT, curR, lookbackSec, minSpanSec = 1800) {
    if (!pts.length) return null;
    const refT = Math.max(pts[0][0], curT - lookbackSec);
    const dt = curT - refT;
    if (dt < minSpanSec) return null;
    const refR = interpAtTime(pts, refT);
    if (refR == null) return null;
    return Math.min(0, (curR - refR) / dt);
  }

  function computeProjection(series, floor, g, lastRun, limit, windowKind, sampleStep, resetTimes = []) {
    const pts = series.points || [];
    if (!pts.length || !floor.length || g.to <= g.now || !lastRun || lastRun.end < g.now - limit) return null;
    const last = pts[pts.length - 1];
    if (!(last[1] > 0)) return null;

    const winLen = WINDOW_SECONDS[windowKind];
    const latestReset = latestResetAtOrBefore(resetTimes, g.now);

    let startIdx = 0;
    for (let i = 1; i < floor.length; i++) {
      if (floor[i][1] > floor[i - 1][1] || hasResetInRange(floor[i - 1][0], floor[i][0], resetTimes)) {
        startIdx = i;
      }
    }
    if (latestReset != null) {
      while (startIdx < floor.length - 1 && floor[startIdx][0] < latestReset) {
        startIdx++;
      }
    }

    // Parse optional real-time post-sample estimate & CPAMP idle confirmation (never mutating series.points!)
    const rawEst = Array.isArray(series.estimated_point) && series.estimated_point.length >= 2
      ? [Number(series.estimated_point[0]), Number(series.estimated_point[1])]
      : null;
    const estValid = !!(
      rawEst &&
      Number.isFinite(rawEst[0]) &&
      Number.isFinite(rawEst[1]) &&
      rawEst[0] > last[0] + 20 &&
      rawEst[0] <= g.now + 60
    );
    const estT = estValid ? Math.min(g.now, rawEst[0]) : null;
    const estRem = estValid ? Math.max(0, Math.min(last[1], rawEst[1])) : null;
    const hasLiveBurn = estValid && (last[1] - estRem) >= 0.02;
    const confirmedIdle = series.post_sample_requests === 0;
    const hasPostSampleActivity = (series.post_sample_requests > 0) || hasLiveBurn;
    const sinceLastSec = Math.max(0, g.now - last[0]);

    let rate = null;
    const t0 = last[0];
    const v0 = last[1];
    let via = null;
    let viaIsLiveBurn = false;

    if (windowKind === '7d') {
      const resetSec = series.reset_sec;
      const cycleStart = resetSec - winLen;
      if (resetSec > g.now && cycleStart < g.now && last[0] >= cycleStart - RESET_DRIFT) {
        const cycPts = floor.slice(startIdx).filter(p => p[0] >= cycleStart - RESET_DRIFT);
        if (!cycPts.length || cycPts[0][0] > cycleStart + 60) {
          cycPts.unshift([cycleStart, 100]);
        }
        let curT = last[0];
        let curR = last[1];
        if (hasLiveBurn) {
          cycPts.push([estT, estRem]);
          curT = estT;
          curR = estRem;
          via = [estT, estRem];
          viaIsLiveBurn = true;
        } else if (confirmedIdle && sinceLastSec >= 600) {
          cycPts.push([g.now, last[1]]);
          curT = g.now;
          curR = last[1];
          via = [g.now, last[1]];
        }

        const elapsedCycle = Math.max(60, curT - cycleStart);
        // Exact time-matched whole-cycle average rate (fixes denominator time mismatch)
        const overallRate = -(100 - curR) / elapsedCycle;

        const rate6h = slidingWindowRate(cycPts, curT, curR, 6 * 3600, 1800);
        const rate12h = slidingWindowRate(cycPts, curT, curR, 12 * 3600, 1800);
        const rate2h = slidingWindowRate(cycPts, curT, curR, 2 * 3600, 900);

        if (elapsedCycle < 6 * 3600 || (rate6h == null && rate12h == null)) {
          rate = overallRate;
        } else {
          const r6 = rate6h != null ? rate6h : rate12h;
          const r12 = rate12h != null ? rate12h : rate6h;
          const burn6 = -r6;
          const burn12 = -r12;
          const burnAll = -overallRate;

          let recentRate;
          let wRecent;
          if (burn6 > Math.max(burn12, burnAll) * 1.25) {
            // Accelerating burst: emphasize recent 6h burn velocity
            recentRate = 0.72 * r6 + 0.28 * r12;
            wRecent = 0.78;
          } else if (burn6 < Math.max(burn12, burnAll) * 0.5) {
            // Slowing down or long pause: decay smoothly toward recent flat pace
            const flat6 = burn6 < 0.05 / (6 * 3600);
            const flat12 = burn12 < 0.05 / (12 * 3600);
            if (!hasPostSampleActivity && flat12) {
              recentRate = r6;
              wRecent = 0.94;
            } else if (!hasPostSampleActivity && flat6) {
              recentRate = 0.75 * r6 + 0.25 * r12;
              wRecent = 0.84;
            } else {
              recentRate = 0.65 * r6 + 0.35 * r12;
              wRecent = 0.72;
            }
          } else {
            // Steady pace
            recentRate = 0.55 * r6 + 0.45 * r12;
            wRecent = 0.65;
          }
          rate = wRecent * recentRate + (1 - wRecent) * overallRate;

          // Reflect acute real-time bursts on 7d within 15s
          if (hasLiveBurn && rate2h != null && -rate2h > Math.max(burn6, burnAll) * 1.4 && (last[1] - estRem) >= 0.15) {
            rate = 0.35 * rate2h + 0.65 * rate;
          }
          // Confirmed idle attenuation after 2h quiet + 10m zero requests
          if (!hasPostSampleActivity && confirmedIdle && sinceLastSec >= 600 && rate2h != null && -rate2h < 0.05 / (2 * 3600)) {
            rate *= 0.5;
          }
        }
      }
    } else {
      // 5h window projection
      const cycPts = floor.slice(startIdx);
      const resetSec = series.reset_sec;
      const cycleStart = resetSec > g.now ? resetSec - winLen : 0;
      if (cycleStart > 0 && cycPts.length > 0 && cycPts[0][0] > cycleStart + 60 && cycPts[0][1] < 99.999) {
        if (latestReset == null || cycleStart >= latestReset - RESET_DRIFT) {
          cycPts.unshift([cycleStart, 100]);
        }
      }

      const tail = cycPts[cycPts.length - 1];
      let idleHeld = false;
      if (hasLiveBurn) {
        cycPts.push([estT, estRem]);
        via = [estT, estRem];
        viaIsLiveBurn = true;
      } else if (confirmedIdle && sinceLastSec >= 600) {
        cycPts.push([g.now, tail[1]]);
        via = [g.now, tail[1]];
        idleHeld = true;
      } else if (!hasPostSampleActivity && sinceLastSec > Math.max(HOLD_AFTER, sampleStep * 1.5)) {
        cycPts.push([g.now, tail[1]]);
        via = [g.now, tail[1]];
      }

      // Silence detector: >= 2 consecutive intervals (>= 15m span) with < 0.08% drop and no active requests -> flat 0
      let isQuietFlat = false;
      if (!hasPostSampleActivity && cycPts.length >= 3) {
        const endPt = cycPts[cycPts.length - 1];
        let flatIdx = cycPts.length - 1;
        while (flatIdx > 0 && Math.abs(cycPts[flatIdx - 1][1] - endPt[1]) < 0.08) {
          flatIdx--;
        }
        const flatIntervals = (cycPts.length - 1) - flatIdx;
        const flatSpan = endPt[0] - cycPts[flatIdx][0];
        if (flatIntervals >= 2 && flatSpan >= 900) {
          isQuietFlat = true;
        }
      }

      if (isQuietFlat) {
        rate = 0;
      } else {
        rate = recentSlope(cycPts, 900);
        // Smooth decay during confirmed 10m..15m idle period
        if (idleHeld && rate < 0) {
          if (sinceLastSec >= 900) {
            rate = 0;
          } else {
            const decay = Math.max(0, (900 - sinceLastSec) / 300) * 0.5;
            rate *= decay;
          }
        }
      }
    }

    if (rate == null || !Number.isFinite(rate)) return null;

    const slope = Math.min(0, rate);
    const anchorT = via ? via[0] : t0;
    const anchorV = via ? via[1] : v0;

    let out = Infinity;
    if (anchorV <= 0) {
      out = anchorT;
    } else if (slope < 0) {
      out = anchorT + anchorV / -slope;
    }
    const end = Math.min(out, g.to);
    const left = out <= g.to ? 0 : Math.max(0, anchorV + slope * (g.to - anchorT));

    return {
      t0,
      v0,
      via,
      viaIsLiveBurn,
      anchorT,
      anchorV,
      slope,
      end,
      out: out <= g.to ? out : null,
      left,
      at: (t) => {
        if (t < t0 || t > g.to) return null;
        if (via && t <= via[0]) {
          const dt = via[0] - t0;
          if (!(dt > 0)) return via[1];
          return Math.max(0, v0 + ((t - t0) / dt) * (via[1] - v0));
        }
        return Math.max(0, anchorV + slope * (t - anchorT));
      },
    };
  }

  function buildTicks(fromSec, toSec, plotW = 600, isMobile = false) {
    const span = Math.max(60, toSec - fromSec);
    const minPxPerTick = isMobile ? 48 : 66;
    const maxTicks = Math.max(3, Math.floor(plotW / minPxPerTick));
    const candidates = [
      600, 1200, 1800, 3600, 2 * 3600, 3 * 3600, 4 * 3600, 6 * 3600,
      8 * 3600, 12 * 3600, 86400, 2 * 86400, 3 * 86400, 5 * 86400, 7 * 86400, 10 * 86400,
    ];
    let step = candidates[candidates.length - 1];
    for (const c of candidates) {
      if (Math.floor(span / c) + 1 <= maxTicks) {
        step = c;
        break;
      }
    }
    const offsetSec = 8 * 3600;
    const list = [];
    for (let t = Math.ceil((fromSec + offsetSec) / step) * step - offsetSec; t <= toSec; t += step) {
      list.push(t);
    }
    return {
      list,
      label: (t) => span <= 2 * 86400 ? fmtHM(t * 1000) : fmtDay(t * 1000),
    };
  }

  function addHatchDefs(root, uid) {
    const defs = svgEl('defs');
    const pat = svgEl('pattern', { id: `${uid}_hatch`, width: 6, height: 6, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' });
    pat.appendChild(svgEl('rect', { width: 2, height: 6, fill: cssVar('--chart-hatch') }));
    defs.appendChild(pat);

    const emptyPat = svgEl('pattern', { id: `${uid}_empty`, width: 6, height: 6, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' });
    emptyPat.appendChild(svgEl('rect', { width: 2.5, height: 6, fill: cssVar('--q-empty-stripe') }));
    defs.appendChild(emptyPat);

    root.appendChild(defs);
    return defs;
  }

  function drawLevelRect(root, attrs, level, uid) {
    root.appendChild(svgEl('rect', { ...attrs, fill: levelColor(level) }));
    if (level === 'empty') {
      const stripeAttrs = { ...attrs };
      delete stripeAttrs['fill-opacity'];
      root.appendChild(svgEl('rect', { ...stripeAttrs, fill: `url(#${uid}_empty)` }));
    }
  }

  function drawHatchRect(root, g, a, b, y0, h, uid) {
    const ca = Math.max(a, g.from);
    const cb = Math.min(b, g.to);
    const w = g.x(cb) - g.x(ca);
    if (w > 1) {
      root.appendChild(svgEl('rect', { x: g.x(ca), y: y0, width: w, height: h, fill: `url(#${uid}_hatch)` }));
    }
  }

  function placeTipBox(tip, box, g, t, topPx) {
    const boxW = box.clientWidth || g.width;
    const scale = boxW / g.width;
    const tipW = tip.offsetWidth || 185;
    let left = g.x(t) * scale + 14;
    if (left + tipW + 8 > boxW) left = g.x(t) * scale - tipW - 14;
    tip.style.left = Math.max(4, left) + 'px';
    tip.style.top = Math.max(4, topPx) + 'px';
  }

  // ---------- Render Lamplighter Panel (Upgraded Visuals + Freed Width) ----------
  function renderAccountHistoryPanel(accId) {
    const panel = document.querySelector(`[data-lamp-panel="${CSS.escape(accId)}"]`);
    if (!panel) return;
    const isPermanent = panel.getAttribute('data-permanent') === '1';
    const pref = getChartPref(accId);
    const curRange = currentRangeOf(pref);
    const cacheKey = historyCacheKey(accId, pref.window, curRange);
    const entry = state.historyCache.get(cacheKey);
    const rangeDefs = RANGES[pref.window] || RANGES['5h'];

    const accObj = (state.snapshot?.antigravity?.accounts || []).find(a => a.id === accId);
    const isUltra = String(accObj?.tier || '').toLowerCase() === 'ultra';

    const winBtns = `
      <div class="seg seg-sm" role="group" aria-label="额度窗口">
        <button type="button" class="${pref.window === '5h' ? 'active' : ''}" data-lamp-win="${esc(accId)}" data-val="5h">5 小时</button>
        <button type="button" class="${pref.window === '7d' ? 'active' : ''}" data-lamp-win="${esc(accId)}" data-val="7d">周额度</button>
      </div>
    `;
    const rangeBtns = `
      <div class="seg seg-sm" role="group" aria-label="时间跨度">
        ${rangeDefs.map(r => `<button type="button" class="${curRange === r.key ? 'active' : ''}" data-lamp-range="${esc(accId)}" data-val="${esc(r.key)}">${esc(r.label)}</button>`).join('')}
      </div>
    `;

    panel.innerHTML = `
      <div class="lamp-toolbar">
        <div class="lamp-toolbar-primary">
          <span class="lamp-title">周期燃烧分析</span>
          ${winBtns}
        </div>
        <div class="lamp-toolbar-secondary">
          ${rangeBtns}
          ${isPermanent ? '' : `<button type="button" class="lamp-collapse-btn" data-trend-toggle="${esc(accId)}">收起 ▲</button>`}
        </div>
      </div>
      <div class="lamp-body"></div>
    `;

    const collapseBtn = panel.querySelector('[data-trend-toggle]');
    if (collapseBtn) {
      collapseBtn.addEventListener('click', () => {
        state.collapsedTrends.add(accId);
        saveCollapsedSet(state.collapsedTrends);
        render();
      });
    }

    panel.querySelectorAll('[data-lamp-win]').forEach(btn => {
      btn.addEventListener('click', () => {
        const nextWin = btn.getAttribute('data-val');
        if (pref.window === nextWin) return;
        pref.window = nextWin;
        renderAccountHistoryPanel(accId);
        fetchAccountHistory(accId, pref.window, currentRangeOf(pref), false);
      });
    });

    panel.querySelectorAll('[data-lamp-range]').forEach(btn => {
      btn.addEventListener('click', () => {
        const nextRange = btn.getAttribute('data-val');
        if (pref.window === '7d') pref.range7d = nextRange;
        else pref.range5h = nextRange;
        renderAccountHistoryPanel(accId);
        fetchAccountHistory(accId, pref.window, nextRange, false);
      });
    });

    const body = panel.querySelector('.lamp-body');
    if (!entry || (entry.loading && !entry.data)) {
      body.innerHTML = `<div class="lamp-loading">正在加载${pref.window === '7d' ? '周额度' : ' 5 小时'}历史采样…</div>`;
      return;
    }

    const rawSeries = entry.data?.series || [];
    const filteredSeries = isUltra
      ? rawSeries.filter(s => s.group !== 'argon' && !String(s.key || '').includes('argon'))
      : rawSeries;

    if (!entry.data || filteredSeries.length === 0) {
      body.innerHTML = `<div class="lamp-loading">${esc(entry.error || '当前时间范围暂无历史采样数据')}</div>`;
      return;
    }

    const hist = { ...entry.data, series: filteredSeries };
    let selected = null;
    if (pref.userPickedGroup) {
      selected = filteredSeries.find(s => s.group === pref.selectedGroup);
    }
    if (!selected) {
      selected = filteredSeries.find(s => s.remaining < 100 - LEVEL_EPSILON)
        || filteredSeries.find(s => s.group === 'gemini')
        || filteredSeries[0];
      pref.selectedGroup = selected.group;
    }

    drawDualLamplighterChart(body, accId, hist, selected, pref);
  }

  function drawDualLamplighterChart(container, accId, hist, selectedSeries, pref) {
    const uidBase = accId.replace(/[^a-zA-Z0-9_-]/g, '') + '_' + hist.window;
    const sampleStep = Math.max(hist.bucket_seconds || 0, hist.sample_step_sec || 900);
    const limit = sampleStep * 3.5;
    const nowSec = hist.to_sec;
    const rawSpan = Math.max(3600, hist.to_sec - hist.from_sec);
    const rangeMeta = (RANGES[hist.window] || []).find(r => r.key === hist.range);
    const allowAhead = rangeMeta ? rangeMeta.ahead : (hist.window === '5h' ? hist.range !== '3h' : !['24h', '3d'].includes(hist.range));

    let axisEnd = nowSec;
    if (allowAhead && selectedSeries.reset_sec > nowSec && (selectedSeries.reset_sec - nowSec) < rawSpan) {
      axisEnd = selectedSeries.reset_sec;
    }
    const axisFrom = axisEnd - rawSpan;

    const rows = hist.series.map(s => {
      const cycleResetTimes = extractCycleResetTimes(s);
      return {
        ...s,
        cycleResetTimes,
        floor: cycleFloor(s.points || [], cycleResetTimes),
      };
    });
    const selectedRow = rows.find(r => r.group === selectedSeries.group) || rows[0];
    const winLen = WINDOW_SECONDS[hist.window];
    const stretches = splitRuns(selectedRow.points || [], limit, nowSec);
    const lastRun = stretches[stretches.length - 1] || null;
    const ahead = computeProjection(
      selectedRow,
      selectedRow.floor,
      { from: axisFrom, to: axisEnd, now: nowSec },
      lastRun,
      limit,
      hist.window,
      sampleStep,
      selectedRow.cycleResetTimes
    );

    // Freed left gutter! Only Y-axis tick width needed on the left.
    const W = Math.max(260, Math.round(container.clientWidth || 640));
    const isMobile = W < 500;
    const needRightPct = !!(ahead && ahead.out == null);
    const labelW = isMobile ? 34 : 40;
    const rightW = needRightPct ? (isMobile ? 34 : 40) : (isMobile ? 10 : 14);
    const plotW = Math.max(140, W - labelW - rightW);

    const g = {
      from: axisFrom,
      to: axisEnd,
      now: nowSec,
      width: W,
      labelW,
      rightW,
      plotW,
      x: (t) => labelW + ((t - axisFrom) / (axisEnd - axisFrom)) * plotW,
      t: (px) => axisFrom + ((px - labelW) / plotW) * (axisEnd - axisFrom),
    };
    const ticks = buildTicks(g.from, g.to, g.plotW, isMobile);
    const fam = familyPalette(selectedRow.group);

    let hoverSource = '';
    let activeInlineScrub = false;
    const hoverListeners = [];
    const selPts = selectedRow.points || [];

    function snapTime(t) {
      if (t > g.now || !selPts.length) return Math.max(g.from, Math.min(g.to, t));
      let best = selPts[0][0], bestDist = Math.abs(best - t);
      for (const p of selPts) {
        const d = Math.abs(p[0] - t);
        if (d < bestDist) { bestDist = d; best = p[0]; }
      }
      return bestDist <= limit ? best : t;
    }

    function emitHover(t, rowGroup, inlineScrub = false) {
      activeInlineScrub = inlineScrub;
      for (const fn of hoverListeners) fn(t, rowGroup, inlineScrub);
    }
    scrubClearers.set('lamp_' + accId, () => {
      if (activeInlineScrub) emitHover(null, null, false);
    });

    container.innerHTML = `
      <div class="lamp-chart-box" data-role="strip"></div>
      <div class="lamp-detail-wrap" data-role="detail-wrap">
        <div class="lamp-kpi-bar" data-role="detail-head"></div>
        <div class="lamp-chart-box" data-role="detail-box"></div>
      </div>
      <div class="lamp-legend" data-role="legend"></div>
    `;

    const colBorder = cssVar('--border');
    const colBorderStrong = cssVar('--border-strong');
    const colText = cssVar('--text');
    const colText2 = cssVar('--text-2');
    const colText3 = cssVar('--text-3');
    const colSurface = cssVar('--surface');
    const colSurface2 = cssVar('--surface-2');
    const colLane = cssVar('--chart-lane');
    const colSelect = cssVar('--chart-select');

    // ---------- 1. Upper Model-Family Interactive Status Strip ----------
    const stripBox = container.querySelector('[data-role="strip"]');
    const AXIS_H = 18;
    const LANE = 13;
    const ROW_H = 44;
    const ROW_GAP = 7;
    const stripH = AXIS_H + rows.length * (ROW_H + ROW_GAP);
    const stripUid = uidBase + '_s';
    const stripSvg = svgEl('svg', { viewBox: `0 0 ${W} ${stripH}` });
    addHatchDefs(stripSvg, stripUid);

    for (const t of ticks.list) {
      const tx = g.x(t);
      if (tx >= labelW - 6 && tx <= labelW + plotW + 10) {
        stripSvg.appendChild(svgText({
          x: tx, y: 11,
          'text-anchor': 'middle',
          'font-size': isMobile ? 9.5 : 10,
          fill: colText3,
        }, ticks.label(t)));
      }
      if (tx > labelW + 3 && tx < labelW + plotW - 3) {
        stripSvg.appendChild(svgEl('line', {
          x1: tx, x2: tx, y1: AXIS_H - 2, y2: stripH - 2,
          stroke: colBorder, 'stroke-dasharray': '2 4',
        }));
      }
    }

    const rowY = {};
    rows.forEach((r, idx) => {
      const y0 = AXIS_H + idx * (ROW_H + ROW_GAP);
      const bandY = y0 + 25;
      rowY[r.group] = y0;
      const isSel = r.group === selectedRow.group;
      const lvl = quotaLevel(r.remaining);
      const rFam = familyPalette(r.group);

      // Interactive rounded outer frame per model family
      stripSvg.appendChild(svgEl('rect', {
        x: 0.5, y: y0 + 0.5, width: W - 1, height: ROW_H - 1,
        rx: 9,
        fill: isSel ? colSelect : colSurface2,
        'fill-opacity': isSel ? 1 : 0.55,
        stroke: isSel ? rFam.a : colBorder,
        'stroke-opacity': isSel ? 0.55 : 1,
        'stroke-width': isSel ? 1.2 : 1,
      }));

      // Selected left vertical highlight accent bar
      if (isSel) {
        stripSvg.appendChild(svgEl('rect', {
          x: 2, y: y0 + 5, width: 3.5, height: ROW_H - 10,
          rx: 2, fill: rFam.a,
        }));
      }

      // Top line inside strip row: Indicator arrow + Family name + click hint (left), Live % & reset (right)
      const leftText = svgEl('text', {
        x: isSel ? 12 : 10,
        y: y0 + 16,
        'font-size': isMobile ? 11.5 : 12,
      });
      const tArrow = svgEl('tspan', { fill: isSel ? rFam.a : colText3, 'font-weight': 700 });
      tArrow.textContent = isSel ? '▸ ' : '◦ ';
      const tName = svgEl('tspan', { fill: isSel ? colText : colText2, 'font-weight': isSel ? 700 : 600 });
      tName.textContent = r.label;
      leftText.appendChild(tArrow);
      leftText.appendChild(tName);
      if (!isMobile && rows.length > 1) {
        const tHint = svgEl('tspan', { fill: isSel ? rFam.a : colText3, 'font-size': 10, 'font-weight': 500 });
        tHint.textContent = isSel ? '  · 当前曲线' : '  · 点击切换';
        leftText.appendChild(tHint);
      }
      stripSvg.appendChild(leftText);

      const rightText = svgEl('text', {
        x: W - 10,
        y: y0 + 16,
        'text-anchor': 'end',
        'font-size': 11.5,
      });
      const tDot = svgEl('tspan', { fill: levelColor(lvl) });
      tDot.textContent = '● ';
      const tPct = svgEl('tspan', { 'font-weight': 700, fill: colText });
      tPct.textContent = fmtRoundPct(r.remaining);
      rightText.appendChild(tDot);
      rightText.appendChild(tPct);
      if (r.reset_sec > nowSec) {
        const tRst = svgEl('tspan', {
          fill: colText3,
          'font-size': 10.5,
          class: 'cd-target',
          'data-ts': r.reset_sec * 1000,
          'data-compact': '1',
          'data-prefix': '  ↻ ',
        });
        tRst.textContent = '  ↻ ' + fmtCompactCountdown(r.reset_sec * 1000);
        rightText.appendChild(tRst);
      }
      stripSvg.appendChild(rightText);

      // Aligned color status lane strictly matching [labelW, labelW + plotW]
      const laneClipId = `${stripUid}_lc_${idx}`;
      const laneClip = svgEl('clipPath', { id: laneClipId });
      laneClip.appendChild(svgEl('rect', { x: labelW, y: bandY, width: plotW, height: LANE, rx: 4 }));
      stripSvg.querySelector('defs').appendChild(laneClip);

      stripSvg.appendChild(svgEl('rect', { x: labelW, y: bandY, width: plotW, height: LANE, rx: 4, fill: colLane }));

      const laneGroup = svgEl('g', { 'clip-path': `url(#${laneClipId})` });
      let cursor = g.from;
      const segRuns = splitRuns(r.floor, limit, g.now);
      for (const run of segRuns) {
        if (run.start > cursor + limit) {
          drawHatchRect(laneGroup, g, cursor, run.start, bandY, LANE, stripUid);
        }
        cursor = run.end;
        const merged = [];
        run.pts.forEach((p, i) => {
          const segEnd = i + 1 < run.pts.length ? run.pts[i + 1][0] : run.end;
          const segLvl = quotaLevel(p[1]);
          const prevSeg = merged[merged.length - 1];
          if (prevSeg && prevSeg.level === segLvl) prevSeg.end = segEnd;
          else merged.push({ start: Math.max(g.from, p[0]), end: segEnd, level: segLvl });
        });
        for (const seg of merged) {
          if (seg.end <= g.from) continue;
          const x0 = Math.round(g.x(seg.start));
          const x1 = Math.max(x0 + 1, Math.round(g.x(Math.min(g.to, seg.end))));
          drawLevelRect(laneGroup, {
            x: x0, y: bandY, width: x1 - x0, height: LANE,
            'fill-opacity': seg.level === 'empty' ? 0.95 : 0.88,
            'shape-rendering': 'crispEdges',
          }, seg.level, stripUid);
        }
      }
      const dEnd = cursor < g.now - limit ? cursor : g.now;
      drawHatchRect(laneGroup, g, dEnd, g.to, bandY, LANE, stripUid);
      stripSvg.appendChild(laneGroup);

      // Full-row click & hover target
      const hit = svgEl('rect', { x: 0, y: y0, width: W, height: ROW_H, rx: 9, fill: 'transparent', style: 'cursor:pointer' });
      hit.addEventListener('click', () => {
        if (pref.selectedGroup !== r.group) {
          pref.selectedGroup = r.group;
          pref.userPickedGroup = true;
          renderAccountHistoryPanel(accId);
        }
      });
      hit.addEventListener('pointermove', (e) => {
        if (e.pointerType === 'touch' || isMobile) return;
        const rect = stripSvg.getBoundingClientRect();
        const rawT = g.t(((e.clientX - rect.left) / rect.width) * g.width);
        if (rawT < g.from || rawT > g.to) { emitHover(null, null, false); return; }
        hoverSource = 'strip';
        emitHover(snapTime(rawT), r.group, false);
      });
      stripSvg.appendChild(hit);
    });

    const stripCross = svgEl('line', { y1: AXIS_H - 2, y2: stripH - 2, stroke: fam.a, 'stroke-width': 1.2, 'stroke-opacity': 0.85, visibility: 'hidden', 'pointer-events': 'none' });
    stripSvg.appendChild(stripCross);
    stripSvg.addEventListener('pointerleave', (e) => {
      if (e.pointerType !== 'touch' && !activeInlineScrub) emitHover(null, null, false);
    });
    stripBox.appendChild(stripSvg);

    const stripTip = document.createElement('div');
    stripTip.className = 'lamp-tip';
    stripTip.hidden = true;
    stripBox.appendChild(stripTip);

    hoverListeners.push((t, activeGroup, inlineScrub) => {
      if (t == null) {
        stripCross.setAttribute('visibility', 'hidden');
        stripTip.hidden = true;
        return;
      }
      const cx = g.x(t);
      stripCross.setAttribute('x1', cx);
      stripCross.setAttribute('x2', cx);
      stripCross.setAttribute('visibility', 'visible');
      if (inlineScrub || isMobile || hoverSource !== 'strip' || t > g.now) {
        stripTip.hidden = true;
        return;
      }
      const rowsHtml = rows.map(r => {
        const p = valueAt(r.points || [], t);
        const valid = p && (t - p[0] <= limit);
        const val = valid ? p[1] : null;
        const swColor = val != null ? levelColor(quotaLevel(val)) : colBorderStrong;
        return `<div class="lamp-tip-row ${r.group === activeGroup ? 'strong' : ''}">
          <span class="lamp-tip-left"><span class="lamp-tip-key" style="background:${swColor}"></span>${esc(r.label)}</span>
          <span class="num">${val != null ? fmtPct(val, 1) : '无数据'}</span>
        </div>`;
      }).join('');
      stripTip.innerHTML = `<div class="lamp-tip-time">${fmtDateTime(t * 1000)}</div>${rowsHtml}`;
      stripTip.hidden = false;
      const scale = (stripBox.clientWidth || W) / W;
      placeTipBox(stripTip, stripBox, g, t, ((rowY[activeGroup] ?? AXIS_H) + 38) * scale);
    });

    // ---------- 2. Lower Cycle Burn Chart (Golden Aspect Ratio + Semantic Gradient) ----------
    const detailHead = container.querySelector('[data-role="detail-head"]');
    const detailBox = container.querySelector('[data-role="detail-box"]');
    const detailUid = uidBase + '_d';

    const pieces = stretches.map(run => {
      const pts = run.pts.slice();
      const tail = pts[pts.length - 1];
      const endT = (ahead && run === lastRun) ? ahead.t0 : run.end;
      if (endT > tail[0]) pts.push([endT, tail[1]]);
      return {
        pts,
        split: splitAtResets(pts, selectedRow.cycleResetTimes, selectedRow.resets, winLen),
      };
    });
    const allResets = [];
    for (const pc of pieces) allResets.push(...pc.split.resets);
    allResets.sort((a, b) => a - b);

    const pSegs = paceSegments(selectedRow.resets, winLen, selectedRow.reset_sec, g.now);
    const pExts = paceExtensions(pSegs, selectedRow.points, winLen, selectedRow.cycleResetTimes);
    const obsSec = (selectedRow.observed_sec > 0 && selectedRow.observed_sec <= g.now)
      ? selectedRow.observed_sec
      : (selPts.length ? selPts[selPts.length - 1][0] : g.now);
    const paceNow = paceAt(pSegs, winLen, g.now);
    const paceAtObs = paceAt(pSegs, winLen, obsSec) ?? extendedPaceAt(pExts, winLen, obsSec);
    const refPaceForDiff = paceAtObs != null ? paceAtObs : paceNow;
    const diffNow = refPaceForDiff == null ? null : Math.round(selectedRow.remaining - refPaceForDiff);
    const diffCls = diffNow == null ? '' : diffNow > 0 ? 'ahead' : diffNow < 0 ? 'behind' : '';

    const resetDisplay = selectedRow.reset_sec > nowSec
      ? `<span class="cd-target num" data-ts="${selectedRow.reset_sec * 1000}" data-compact="1">${fmtCompactCountdown(selectedRow.reset_sec * 1000)}</span>`
      : (selectedRow.remaining >= 99.999 ? '未开窗' : '—');

    const curTitle = `官方采样时刻 ${fmtDateTime(obsSec * 1000)} 余量 ${fmtPct(selectedRow.remaining, 2)}`;
    const paceTitle = paceNow != null
      ? `当前时刻匀速目标 ${fmtPct(paceNow, 1)}${paceAtObs != null && Math.abs(g.now - obsSec) >= 30 ? `（采样时刻 ${fmtHM(obsSec * 1000)} 目标 ${fmtPct(paceAtObs, 1)}）` : ''}`
      : '未开窗或暂无匀速基线';
    const diffTitle = refPaceForDiff != null
      ? `按同一采样时刻 ${fmtHM(obsSec * 1000)} 对齐：${fmtPct(selectedRow.remaining, 1)} − ${fmtPct(refPaceForDiff, 1)} = ${fmtDiffPct(diffNow)}`
      : '暂无差值';

    function renderLiveDetailHead() {
      detailHead.classList.remove('scrubbing');
      detailHead.innerHTML = `
        <div class="lamp-kpi-identity">
          <span class="lamp-kpi-dot" style="background:${fam.a}"></span>
          <span class="lamp-kpi-name">${esc(selectedRow.label)} · ${hist.window === '7d' ? '周额度' : '5 小时'}</span>
          ${isMobile ? `<span class="lamp-kpi-hint">滑动看读数</span>` : ''}
        </div>
        <div class="lamp-kpi-stats num">
          <span class="lamp-kpi-item" title="${esc(curTitle)}"><span>当前</span><strong style="color:${pctTextColor(selectedRow.remaining)}">${fmtRoundPct(selectedRow.remaining)}</strong></span>
          <span class="lamp-kpi-item" title="${esc(paceTitle)}"><span>匀速</span><strong>${fmtRoundPct(paceNow)}</strong></span>
          <span class="lamp-kpi-item" title="${esc(diffTitle)}"><span>差值</span><strong class="${diffCls}">${fmtDiffPct(diffNow)}</strong></span>
          <span class="lamp-kpi-item"><span>距重置</span><strong>${resetDisplay}</strong></span>
        </div>
      `;
    }

    function renderScrubDetailHead(t) {
      const paceVal = paceAt(pSegs, winLen, t);
      const extVal = extendedPaceAt(pExts, winLen, t);
      const refPace = paceVal != null ? paceVal : extVal;
      const isFuture = t > g.now;
      const nearSec = (g.to - g.from) / plotW * 10;
      const nearReset = !isFuture ? allResets.find(r => r >= g.from && Math.abs(r - t) <= nearSec) : null;

      const valLabel = isFuture ? '预计' : '剩余';
      let valNum = null;
      if (isFuture) {
        valNum = ahead ? ahead.at(t) : null;
      } else {
        const p = valueAt(selectedRow.points || [], t);
        if (p && (t - p[0] <= limit)) valNum = p[1];
      }
      const diffVal = (valNum != null && refPace != null) ? Math.round(valNum - refPace) : null;
      const dCls = diffVal == null ? '' : diffVal > 0 ? 'ahead' : diffVal < 0 ? 'behind' : '';
      const projMeta = isFuture && ahead
        ? (ahead.out != null
            ? ` · 预计 ${hist.window === '7d' ? fmtDateTime(ahead.out * 1000) : fmtHM(ahead.out * 1000)} 耗尽`
            : ` · 期末余 ${fmtRoundPct(ahead.left)}`)
        : '';
      const timeBadge = `${fmtDateTime(t * 1000)}${isFuture ? ' · 预测' + projMeta : ''}${nearReset ? ' · ↻重置' : ''}`;

      detailHead.classList.add('scrubbing');
      detailHead.innerHTML = `
        <div class="lamp-kpi-identity" style="width:100%;justify-content:space-between;">
          <span class="lamp-kpi-name mono" style="font-size:11.5px;color:var(--accent);">${esc(timeBadge)}</span>
          <button type="button" class="lamp-scrub-reset" data-scrub-clear="1">恢复实时 ×</button>
        </div>
        <div class="lamp-kpi-stats num">
          <span class="lamp-kpi-item"><span>${valLabel}</span><strong>${valNum != null ? fmtPct(valNum, 1) : '无数据'}</strong></span>
          <span class="lamp-kpi-item"><span>${paceVal == null && extVal != null ? '回延匀速' : '匀速'}</span><strong>${refPace != null ? fmtPct(refPace, 1) : '—'}</strong></span>
          <span class="lamp-kpi-item"><span>差值</span><strong class="${dCls}">${fmtDiffPct(diffVal)}</strong></span>
          <span class="lamp-kpi-item"><span>距重置</span><strong>${resetDisplay}</strong></span>
        </div>
      `;
      const clearBtn = detailHead.querySelector('[data-scrub-clear]');
      if (clearBtn) {
        clearBtn.addEventListener('click', (ev) => {
          ev.stopPropagation();
          emitHover(null, null, false);
        });
      }
    }

    renderLiveDetailHead();

    // Golden aspect ratio: plotH = 200px on desktop (195-215px target), 178px on mobile
    const padT = 22;
    const padB = 22;
    const plotH = isMobile ? 178 : 204;
    const H = plotH + padT + padB;
    const yOf = (v) => padT + (1 - Math.max(0, Math.min(100, v)) / 100) * plotH;

    const detailSvg = svgEl('svg', { viewBox: `0 0 ${W} ${H}` });
    const defs = addHatchDefs(detailSvg, detailUid);

    // Model-family semantic stroke & area gradients
    const strokeGrad = svgEl('linearGradient', { id: `${detailUid}_stroke`, x1: 0, x2: 1, y1: 0, y2: 0 });
    strokeGrad.appendChild(svgEl('stop', { offset: '0%', 'stop-color': fam.a }));
    strokeGrad.appendChild(svgEl('stop', { offset: '100%', 'stop-color': fam.b }));
    defs.appendChild(strokeGrad);

    const areaGrad = svgEl('linearGradient', { id: `${detailUid}_fade`, x1: 0, x2: 0, y1: 0, y2: 1 });
    areaGrad.appendChild(svgEl('stop', { offset: '0%', 'stop-color': fam.area, 'stop-opacity': 1 }));
    areaGrad.appendChild(svgEl('stop', { offset: '100%', 'stop-color': fam.area, 'stop-opacity': 0 }));
    defs.appendChild(areaGrad);

    const clip = svgEl('clipPath', { id: `${detailUid}_clip` });
    clip.appendChild(svgEl('rect', { x: labelW, y: 0, width: plotW, height: H }));
    defs.appendChild(clip);

    const yFontSize = isMobile ? 9.5 : 10;
    const yLabelX = labelW - 6;

    // Horizontal Y grid: 0%, 60%, 100% + 25% coral warning line (unified 5-level scale)
    [0, 60, 100].forEach(v => {
      const yv = yOf(v);
      detailSvg.appendChild(svgEl('line', {
        x1: labelW, x2: labelW + plotW, y1: yv, y2: yv,
        stroke: v === 0 ? colBorderStrong : colBorder,
        'stroke-dasharray': v === 0 ? 'none' : '2 4',
      }));
      detailSvg.appendChild(svgText({ x: yLabelX, y: yv + 3.5, 'text-anchor': 'end', 'font-size': yFontSize, fill: colText3 }, `${v}%`));
    });

    const colRedWarn = cssVar('--q-low');
    const y25 = yOf(25);
    detailSvg.appendChild(svgEl('line', {
      x1: labelW, x2: labelW + plotW, y1: y25, y2: y25,
      stroke: colRedWarn, 'stroke-opacity': 0.5, 'stroke-dasharray': '4 4',
    }));
    detailSvg.appendChild(svgText({ x: yLabelX, y: y25 + 3.5, 'text-anchor': 'end', 'font-size': yFontSize, fill: colRedWarn }, '25%'));

    let outLabel = null;
    if (ahead && ahead.out != null) {
      const outText = hist.window === '7d' ? fmtDateTime(ahead.out * 1000) : fmtHM(ahead.out * 1000);
      const outW = outText.length * (isMobile ? 5.8 : CHAR_W);
      outLabel = {
        text: outText,
        w: outW,
        x: Math.max(labelW + outW / 2, Math.min(W - outW / 2 - 2, g.x(ahead.out))),
      };
    }

    for (const t of ticks.list) {
      const tx = g.x(t);
      if (tx > labelW + 4 && tx < labelW + plotW - 4) {
        detailSvg.appendChild(svgEl('line', { x1: tx, x2: tx, y1: padT, y2: padT + plotH, stroke: colBorder, 'stroke-dasharray': '2 4' }));
      }
      const lbl = ticks.label(t);
      if (tx < labelW - 6 || tx > labelW + plotW + 10) continue;
      if (outLabel && Math.abs(tx - outLabel.x) < (outLabel.w + lbl.length * (isMobile ? 5.6 : CHAR_W)) / 2 + 5) continue;
      detailSvg.appendChild(svgText({ x: tx, y: H - 5, 'text-anchor': 'middle', 'font-size': yFontSize, fill: colText3 }, lbl));
    }

    // Gap & future hatching
    let cursor = g.from;
    for (const run of stretches) {
      if (run.start > cursor + limit) {
        drawHatchRect(detailSvg, g, cursor, run.start, padT, plotH, detailUid);
      }
      cursor = run.end;
    }
    const hatchStart = ahead ? ahead.t0 : (cursor < g.now - limit ? cursor : g.now);
    drawHatchRect(detailSvg, g, hatchStart, g.to, padT, plotH, detailUid);

    const firstIn = stretches.find(r => r.end > g.from);
    if (!firstIn || firstIn.start > g.from + limit) {
      const midX = (g.x(g.from) + g.x(firstIn ? firstIn.start : Math.min(g.now, g.to))) / 2;
      detailSvg.appendChild(svgText({ x: midX, y: padT + plotH / 2 + 4, 'text-anchor': 'middle', 'font-size': 12, fill: colText3 }, '无数据'));
    }

    [labelW, labelW + plotW].forEach(bx => {
      detailSvg.appendChild(svgEl('line', { x1: bx, x2: bx, y1: padT, y2: padT + plotH, stroke: colBorderStrong }));
    });

    const plot = svgEl('g', { 'clip-path': `url(#${detailUid}_clip)` });
    detailSvg.appendChild(plot);

    // Even-pace reference lines
    const breakSet = [g.from, g.to];
    for (const s of pSegs) { if (s[0] > g.from && s[0] < g.to) breakSet.push(s[0]); if (s[1] > g.from && s[1] < g.to) breakSet.push(s[1]); }
    for (const e of pExts) { if (e[0] > g.from && e[0] < g.to) breakSet.push(e[0]); if (e[1] > g.from && e[1] < g.to) breakSet.push(e[1]); }
    breakSet.sort((a, b) => a - b);

    function buildPacePath(evalFn) {
      let d = '';
      let prevVal = null;
      for (let k = 0; k + 1 < breakSet.length; k++) {
        const a = breakSet[k], b = breakSet[k + 1];
        if (b - a <= 0) continue;
        const inset = Math.min(1, (b - a) / 4);
        const va = evalFn(a + inset);
        const vb = evalFn(b - inset);
        if (va == null || vb == null) { prevVal = null; continue; }
        if (prevVal == null || Math.abs(prevVal - va) > 0.5) {
          d += `M${g.x(a).toFixed(2)} ${yOf(va).toFixed(2)}`;
        }
        d += `L${g.x(b).toFixed(2)} ${yOf(vb).toFixed(2)}`;
        prevVal = vb;
      }
      return d;
    }

    const paceD = buildPacePath(t => paceAt(pSegs, winLen, t));
    const extPaceD = buildPacePath(t => extendedPaceAt(pExts, winLen, t));
    if (paceD) {
      plot.appendChild(svgEl('path', {
        d: paceD, fill: 'none', stroke: colText3,
        'stroke-opacity': 0.78, 'stroke-width': 1.4, 'stroke-dasharray': '5 4',
      }));
    }
    if (extPaceD) {
      plot.appendChild(svgEl('path', {
        d: extPaceD, fill: 'none', stroke: colText3,
        'stroke-opacity': 0.65, 'stroke-width': 1.4, 'stroke-dasharray': '8 3 1.5 3',
      }));
    }

    // Cycle burn curves with model-family gradient stroke + soft area fill
    const renderedCurves = [];
    for (const piece of pieces) {
      const nParts = piece.split.parts.length;
      for (let pi = 0; pi < nParts; pi++) {
        const part = piece.split.parts[pi];
        const smoothed = curvePoints(declining(part));
        if (!smoothed.length) continue;
        const spline = buildMonotoneSpline(smoothed);
        renderedCurves.push({
          tStart: smoothed[0][0],
          tEnd: smoothed[smoothed.length - 1][0],
          rightOpen: pi + 1 < nParts,
          spline,
        });
        const xy = smoothed.map(p => [g.x(p[0]), yOf(p[1])]);
        if (xy.length === 1) xy.push([xy[0][0] + 1, xy[0][1]]);
        const d = monotonePath(xy);
        if (!d) continue;
        const areaD = `${d}L${xy[xy.length - 1][0].toFixed(2)} ${yOf(0).toFixed(2)}L${xy[0][0].toFixed(2)} ${yOf(0).toFixed(2)}Z`;
        plot.appendChild(svgEl('path', { d: areaD, fill: `url(#${detailUid}_fade)` }));
        plot.appendChild(svgEl('path', {
          d, fill: 'none', stroke: `url(#${detailUid}_stroke)`,
          'stroke-width': 2.35, 'stroke-linejoin': 'round', 'stroke-linecap': 'round',
          'data-role': 'burn-curve',
        }));
      }
    }

    function evalCurvePct(t) {
      for (let i = 0; i < renderedCurves.length; i++) {
        const c = renderedCurves[i];
        if (t >= c.tStart && (c.rightOpen ? t < c.tEnd : t <= c.tEnd)) {
          return c.spline.evalPct(t);
        }
      }
      for (let i = renderedCurves.length - 1; i >= 0; i--) {
        const c = renderedCurves[i];
        if (t >= c.tStart && t <= c.tEnd) {
          return c.spline.evalPct(t);
        }
      }
      return null;
    }

    // Reset vertical markers + top labels (all dashed lines kept; labels collision-filtered)
    let lastResetLblX = -999;
    const resetMinGap = isMobile ? 44 : 48;
    for (const rt of allResets) {
      if (rt < g.from || rt > g.to) continue;
      const rx = g.x(rt);
      detailSvg.appendChild(svgEl('line', {
        x1: rx, x2: rx, y1: padT - 4, y2: padT + plotH,
        stroke: colText3, 'stroke-opacity': 0.65, 'stroke-dasharray': '3 3',
        'data-role': 'reset-line',
        'data-ts': rt,
      }));
      if (rx - lastResetLblX >= resetMinGap && rx > labelW + 12 && rx < labelW + plotW - 12) {
        const rLabel = hist.window === '7d' ? '↻' + fmtDay(rt * 1000) : '↻' + fmtHM(rt * 1000);
        detailSvg.appendChild(svgText({
          x: rx, y: padT - 6,
          'text-anchor': 'middle',
          'font-size': isMobile ? 9.5 : 10,
          fill: colText3,
          'data-role': 'reset-label',
        }, rLabel));
        lastResetLblX = rx;
      }
    }

    // Real-time-aware projection dotted trajectory (starts from last real sample, optionally via live estimate point)
    const rightLabelX = labelW + plotW + 6;
    if (ahead) {
      const px = g.x(ahead.end);
      const py = yOf(ahead.out != null ? 0 : ahead.left);
      const projStroke = ahead.out != null ? colRedWarn : fam.a;
      const startCurveV = evalCurvePct(ahead.t0);
      let projD = `M${g.x(ahead.t0).toFixed(2)} ${yOf(startCurveV != null ? startCurveV : ahead.v0).toFixed(2)}`;
      if (ahead.via && ahead.via[0] > ahead.t0 && ahead.via[0] < ahead.end) {
        projD += `L${g.x(ahead.via[0]).toFixed(2)} ${yOf(ahead.via[1]).toFixed(2)}`;
      }
      projD += `L${px.toFixed(2)} ${py.toFixed(2)}`;

      detailSvg.appendChild(svgEl('path', {
        d: projD,
        fill: 'none',
        stroke: projStroke,
        'stroke-opacity': 0.88, 'stroke-width': 2,
        'stroke-dasharray': '1.5 4.5', 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
        'data-role': 'proj-line',
      }));
      if (ahead.via && ahead.viaIsLiveBurn && ahead.via[0] > ahead.t0 && ahead.via[0] < ahead.end) {
        detailSvg.appendChild(svgEl('circle', {
          cx: g.x(ahead.via[0]), cy: yOf(ahead.via[1]), r: 3.1,
          fill: colSurface, stroke: projStroke, 'stroke-width': 1.8,
          'data-role': 'proj-live-est',
        }));
      }
      detailSvg.appendChild(svgEl('circle', {
        cx: px, cy: py, r: 3.5,
        fill: projStroke,
        stroke: colSurface, 'stroke-width': 1.5,
      }));
      if (outLabel) {
        detailSvg.appendChild(svgText({
          x: outLabel.x, y: H - 5, 'text-anchor': 'middle',
          'font-size': yFontSize, 'font-weight': 700, fill: colRedWarn,
        }, outLabel.text));
      } else {
        detailSvg.appendChild(svgText({
          x: rightLabelX, y: Math.max(padT + 10, Math.min(padT + plotH, py + 3.5)),
          'font-size': isMobile ? 10.5 : 11.5, 'font-weight': 700, fill: fam.a,
        }, fmtRoundPct(ahead.left)));
      }
    }

    // Latest live real .jsonl sample dot
    const latestPt = (selectedRow.points || [])[selectedRow.points.length - 1];
    if (latestPt && lastRun && lastRun.end >= g.now - limit) {
      const lt = ahead ? ahead.t0 : lastRun.end;
      const lv = evalCurvePct(lt);
      detailSvg.appendChild(svgEl('circle', {
        cx: g.x(lt), cy: yOf(lv != null ? lv : latestPt[1]), r: 4.2,
        fill: fam.a, stroke: colSurface, 'stroke-width': 2,
      }));
    }

    // Crosshair & Hover/Touch Scrubbing
    const detailCross = svgEl('line', { y1: padT, y2: padT + plotH, stroke: fam.a, 'stroke-opacity': 0.85, 'stroke-width': 1.2, visibility: 'hidden', 'pointer-events': 'none' });
    const detailDot = svgEl('circle', { r: 4.5, fill: fam.a, stroke: colSurface, 'stroke-width': 2, visibility: 'hidden', 'pointer-events': 'none', 'data-role': 'detail-dot' });
    detailSvg.appendChild(detailCross);
    detailSvg.appendChild(detailDot);

    const hitRect = svgEl('rect', { x: labelW, y: padT, width: plotW, height: plotH, fill: 'transparent', style: 'cursor:crosshair' });
    function handleDetailPointer(clientX, inlineScrub) {
      const rect = detailSvg.getBoundingClientRect();
      const rawT = g.t(((clientX - rect.left) / rect.width) * g.width);
      const clampedT = Math.max(g.from, Math.min(g.to, rawT));
      hoverSource = 'detail';
      emitHover(snapTime(clampedT), selectedRow.group, inlineScrub);
    }
    hitRect.addEventListener('pointerdown', (e) => {
      if (e.pointerType === 'touch' || isMobile) handleDetailPointer(e.clientX, true);
    });
    hitRect.addEventListener('pointermove', (e) => {
      if (e.pointerType === 'touch') return;
      handleDetailPointer(e.clientX, isMobile);
    });
    let touchStartX = 0, touchStartY = 0;
    hitRect.addEventListener('touchstart', (e) => {
      if (e.touches[0]) {
        touchStartX = e.touches[0].clientX;
        touchStartY = e.touches[0].clientY;
        handleDetailPointer(touchStartX, true);
      }
    }, { passive: true });
    hitRect.addEventListener('touchmove', (e) => {
      if (!e.touches[0]) return;
      if (Math.abs(e.touches[0].clientX - touchStartX) >= Math.abs(e.touches[0].clientY - touchStartY)) {
        handleDetailPointer(e.touches[0].clientX, true);
      }
    }, { passive: true });

    detailSvg.appendChild(hitRect);
    detailSvg.addEventListener('pointerleave', (e) => {
      if (e.pointerType !== 'touch' && !activeInlineScrub) emitHover(null, null, false);
    });

    detailBox.appendChild(detailSvg);
    const detailTip = document.createElement('div');
    detailTip.className = 'lamp-tip';
    detailTip.hidden = true;
    detailBox.appendChild(detailTip);

    hoverListeners.push((t, _rowGroup, inlineScrub) => {
      if (t == null) {
        detailCross.setAttribute('visibility', 'hidden');
        detailDot.setAttribute('visibility', 'hidden');
        detailTip.hidden = true;
        if (detailHead.classList.contains('scrubbing')) renderLiveDetailHead();
        return;
      }
      const cx = g.x(t);
      detailCross.setAttribute('x1', cx);
      detailCross.setAttribute('x2', cx);
      detailCross.setAttribute('visibility', 'visible');

      const paceVal = paceAt(pSegs, winLen, t);
      const extVal = extendedPaceAt(pExts, winLen, t);

      if (t > g.now) {
        const projVal = ahead ? ahead.at(t) : null;
        if (projVal != null) {
          detailDot.setAttribute('cx', cx);
          detailDot.setAttribute('cy', yOf(projVal));
          detailDot.setAttribute('visibility', 'visible');
        } else {
          detailDot.setAttribute('visibility', 'hidden');
        }
      } else {
        const p = valueAt(selectedRow.points || [], t);
        const valid = p && (t - p[0] <= limit);
        if (valid) {
          const dotT = p[0];
          const curveVal = evalCurvePct(dotT);
          detailDot.setAttribute('cx', g.x(dotT));
          detailDot.setAttribute('cy', yOf(curveVal != null ? curveVal : p[1]));
          detailDot.setAttribute('visibility', 'visible');
        } else {
          detailDot.setAttribute('visibility', 'hidden');
        }
      }

      if (inlineScrub || isMobile) {
        detailTip.hidden = true;
        renderScrubDetailHead(t);
        return;
      }

      if (hoverSource !== 'detail') { detailTip.hidden = true; return; }

      if (t > g.now) {
        const projVal = ahead ? ahead.at(t) : null;
        const rowsHtml = [
          `<div class="lamp-tip-row strong">
            <span class="lamp-tip-left"><span class="lamp-tip-key line-proj"></span>预计剩余</span>
            <span class="num">${projVal != null ? fmtPct(projVal, 1) : '—'}</span>
          </div>`,
        ];
        if (ahead && ahead.viaIsLiveBurn && ahead.via) {
          rowsHtml.push(`<div class="lamp-tip-row">
            <span class="lamp-tip-left"><span class="lamp-tip-key" style="background:var(--accent)"></span>当前实时估算</span>
            <span class="num">${fmtPct(ahead.via[1], 1)}</span>
          </div>`);
        }
        if (paceVal != null) {
          rowsHtml.push(`<div class="lamp-tip-row">
            <span class="lamp-tip-left"><span class="lamp-tip-key line-pace"></span>匀速参考</span>
            <span class="num">${fmtPct(paceVal, 1)}</span>
          </div>`);
        }
        let projNote = '';
        if (ahead) {
          if (ahead.out != null) {
            const outStr = hist.window === '7d' ? fmtDateTime(ahead.out * 1000) : fmtHM(ahead.out * 1000);
            projNote = `<div class="lamp-tip-note" style="color:var(--red-text);font-weight:600;">预计 ${esc(outStr)} 耗尽（约 ${esc(fmtCompactCountdown(ahead.out * 1000))} 后）</div>`;
          } else {
            projNote = `<div class="lamp-tip-note">按当前速率，周期结束预计剩余 <strong>${fmtPct(ahead.left, 1)}</strong></div>`;
          }
        }
        detailTip.innerHTML = `<div class="lamp-tip-time">${fmtDateTime(t * 1000)} · 预测</div>${rowsHtml.join('')}${projNote}`;
        detailTip.hidden = false;
        placeTipBox(detailTip, detailBox, g, t, 8);
        return;
      }

      const p = valueAt(selectedRow.points || [], t);
      const valid = p && (t - p[0] <= limit);
      const rowsHtml = [];
      const valColor = valid ? levelColor(quotaLevel(p[1])) : colBorderStrong;
      rowsHtml.push(`<div class="lamp-tip-row strong">
        <span class="lamp-tip-left"><span class="lamp-tip-key" style="background:${valColor}"></span>${esc(selectedRow.label)}</span>
        <span class="num">${valid ? fmtPct(p[1], 2) : '无数据'}</span>
      </div>`);
      if (paceVal != null) {
        rowsHtml.push(`<div class="lamp-tip-row">
          <span class="lamp-tip-left"><span class="lamp-tip-key line-pace"></span>匀速参考</span>
          <span class="num">${fmtPct(paceVal, 1)}</span>
        </div>`);
        if (valid) {
          const d = Math.round(p[1] - paceVal);
          const dCol = d > 0 ? 'var(--q-high)' : d < 0 ? 'var(--q-low)' : 'var(--text-2)';
          rowsHtml.push(`<div class="lamp-tip-row">
            <span class="lamp-tip-left" style="padding-left:18px;color:var(--text-3);">相对匀速</span>
            <span class="num" style="font-weight:700;color:${dCol};">${fmtDiffPct(d)}</span>
          </div>`);
        }
      } else if (extVal != null) {
        rowsHtml.push(`<div class="lamp-tip-row">
          <span class="lamp-tip-left"><span class="lamp-tip-key line-pace"></span>回延匀速</span>
          <span class="num">${fmtPct(extVal, 1)}</span>
        </div>`);
      }
      const nearSec = (g.to - g.from) / plotW * 8;
      const nearReset = allResets.find(r => r >= g.from && Math.abs(r - t) <= nearSec);
      const noteHtml = nearReset
        ? `<div class="lamp-tip-note">↻ ${hist.window === '7d' ? fmtDateTime(nearReset * 1000) : fmtHM(nearReset * 1000)} 周期重置</div>`
        : '';

      detailTip.innerHTML = `<div class="lamp-tip-time">${fmtDateTime(t * 1000)}</div>${rowsHtml.join('')}${noteHtml}`;
      detailTip.hidden = false;
      placeTipBox(detailTip, detailBox, g, t, 8);
    });

    // ---------- 3. Streamlined Unified 5-Level Legend ----------
    const legend = container.querySelector('[data-role="legend"]');
    legend.innerHTML = `
      <span class="lamp-legend-item"><i class="sw full"></i>100%</span>
      <span class="lamp-legend-item"><i class="sw high"></i>&gt;60%</span>
      <span class="lamp-legend-item"><i class="sw med"></i>25–60%</span>
      <span class="lamp-legend-item"><i class="sw low"></i>1–25%</span>
      <span class="lamp-legend-item"><i class="sw empty"></i>0%</span>
      <span class="lamp-legend-item"><i class="sw hatch"></i>无数据/未来</span>
      <span class="lamp-legend-item">↻ 重置</span>
      <span class="lamp-legend-item"><i class="sw line-pace"></i>匀速</span>
      <span class="lamp-legend-item"><i class="sw line-proj"></i>预测</span>
    `;
  }

  // ---------- AIStudio2API 24-Hour Slot Builder & Sparkline ----------
  function build24HourSlots(ais) {
    const raw = ais.hourly_buckets || [];
    const nowMs = state.snapshot?.generated_at_ms || Date.now();
    const startMs = ais.cycle_start_ms;

    if (!(startMs > 0)) {
      return raw.map(b => ({
        ...b,
        isFuture: false,
        isCurrentHour: false,
      }));
    }

    const byIndex = new Map();
    for (const b of raw) {
      const idx = Math.round((b.timestamp_ms - startMs) / 3600_000);
      if (idx >= 0 && idx < 24) byIndex.set(idx, b);
    }

    const slots = [];
    for (let i = 0; i < 24; i++) {
      const ts = startMs + i * 3600_000;
      const b = byIndex.get(i);
      const isFuture = !b && ts > nowMs;
      const isCurrentHour = !isFuture && nowMs >= ts && nowMs < ts + 3600_000;
      slots.push({
        timestamp_ms: ts,
        label: b?.label || fmtHM(ts),
        requests: b?.requests || 0,
        succeeded: b?.succeeded || 0,
        failed: b?.failed || 0,
        other_failed: b?.other_failed || 0,
        canceled: b?.canceled || 0,
        rate_limited: b?.rate_limited || 0,
        input_tokens: b?.input_tokens || 0,
        reasoning_tokens: b?.reasoning_tokens || 0,
        reply_tokens: b?.reply_tokens || 0,
        total_tokens: b?.total_tokens || 0,
        first_event_p50_ms: b?.first_event_p50_ms || 0,
        first_event_avg_ms: b?.first_event_avg_ms || 0,
        isFuture,
        isCurrentHour,
      });
    }
    return slots;
  }

  function renderMiniSparklineHTML(slots) {
    const elapsed = slots.filter(s => !s.isFuture);
    const W = 220;
    const H = 24;
    const vals = elapsed.map(s => s.requests || 0);
    const maxV = Math.max(1, ...vals);
    if (elapsed.length === 0 || vals.every(v => v === 0)) {
      return `<div class="kpi-sparkline" aria-hidden="true"><svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none"><line x1="0" y1="${H - 2}" x2="${W}" y2="${H - 2}" stroke="var(--border)" stroke-dasharray="3 3"/></svg></div>`;
    }
    const stepX = W / 23;
    const pts = elapsed.map((s, idx) => {
      const x = idx * stepX;
      const y = (H - 3) - ((s.requests || 0) / maxV) * (H - 6);
      return [x, y];
    });
    if (pts.length === 1) pts.push([pts[0][0] + 6, pts[0][1]]);
    const pathD = monotonePath(pts);
    const areaD = `${pathD}L${pts[pts.length - 1][0].toFixed(1)} ${H}L${pts[0][0].toFixed(1)} ${H}Z`;

    return `
      <div class="kpi-sparkline" aria-hidden="true" title="本轮 24h 每小时请求节律预览">
        <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">
          <defs>
            <linearGradient id="kpi-spark-fill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stop-color="var(--q-high)" stop-opacity="0.32"/>
              <stop offset="100%" stop-color="var(--q-high)" stop-opacity="0.0"/>
            </linearGradient>
          </defs>
          <path d="${areaD}" fill="url(#kpi-spark-fill)"/>
          <path d="${pathD}" fill="none" stroke="var(--q-high)" stroke-width="1.6" stroke-linecap="round"/>
        </svg>
      </div>
    `;
  }

  // ---------- Render AIStudio2API Playground Section ----------
  function isImageModel(m) {
    return m.category === 'image'
      || /(?:^|-)image$|^gemini-nano-banana|^imagen/i.test(m.id || '')
      || /Nano\s*Banana/i.test(m.label || '');
  }

  function isGemini4Model(m) {
    if (isImageModel(m)) return false;
    return /^gemini-4/i.test(m.id || '') || /Gemini\s*4/i.test(m.label || '');
  }

  function computeModelDimensions(m) {
    const hasTokenLimit = (m.limit_tokens || 0) > 0;
    const reqPct = m.limit_requests > 0
      ? Math.max(0, Math.min(100, (m.effective_remaining_requests / m.limit_requests) * 100))
      : 0;
    const tokPct = hasTokenLimit
      ? Math.max(0, Math.min(100, (m.effective_remaining_tokens / m.limit_tokens) * 100))
      : null;

    let bottleneck = 'balanced'; // 'tokens' | 'requests' | 'balanced' | 'requests_only' | 'none'
    if (m.eligible_accounts === 0) {
      bottleneck = 'none';
    } else if (!hasTokenLimit) {
      bottleneck = 'requests_only';
    } else if (m.used_requests > 0 || m.used_tokens > 0 || m.effective_percent < 100 - LEVEL_EPSILON) {
      if (Math.abs(reqPct - tokPct) >= 0.1) {
        bottleneck = tokPct < reqPct ? 'tokens' : 'requests';
      }
    }
    return { hasTokenLimit, reqPct, tokPct, bottleneck };
  }

  function bottleneckTagHTML(bottleneck) {
    if (bottleneck === 'tokens') return `<span class="bn-tag bn-tok">Token 短板</span>`;
    if (bottleneck === 'requests') return `<span class="bn-tag bn-req">次数短板</span>`;
    if (bottleneck === 'requests_only') return `<span class="bn-tag bn-req-only">按次计额</span>`;
    if (bottleneck === 'balanced') return `<span class="bn-tag bn-bal">均衡</span>`;
    return `<span class="bn-tag bn-bal">无合格号</span>`;
  }

  function renderAIStudio(ais) {
    const tierStr = ['Ultra', 'Pro', 'Free']
      .filter(t => (ais.tier_counts || {})[t] > 0)
      .map(t => `<span class="badge ${tierBadgeClass(t)}">${esc(t)} ×${ais.tier_counts[t]}</span>`)
      .join('');

    const resetBadge = `
      <span class="badge badge-blue num" title="每日北京时间 16:00 重置（下次：${esc(fmtDateTime(ais.next_reset_ms))}）">
        距离 16:00 重置 <strong class="cd-hms" data-ts="${ais.next_reset_ms}">${fmtHMS(ais.next_reset_ms)}</strong>
      </span>
    `;

    if (!ais.online) {
      return `
        <section class="section" id="section-aistudio">
          <div class="section-head">
            <div class="section-title-wrap">
              <h2 class="section-title">AIStudio2API 号池</h2>
              <span class="badge badge-red">离线</span>
            </div>
            ${resetBadge}
          </div>
          <div class="empty-box">${esc(ais.status_message || 'AIStudio2API 暂不可达')}</div>
        </section>
      `;
    }

    const c = ais.account_counts || {};
    const pg = ais.playground || {};
    const slots = build24HourSlots(ais);

    // Card 1: Readiness badge & subline
    const abnormalCnt = (c.auth_required || 0) + (c.unavailable || 0);
    let readyBadgeHtml = '';
    if (!ais.service_ready) {
      readyBadgeHtml = `<span class="badge badge-amber">${esc(ais.service_state || '未就绪')}</span>`;
    } else if (abnormalCnt > 0) {
      readyBadgeHtml = `<span class="badge badge-red num">异常 ${fmtInt(abnormalCnt)}</span>`;
    } else if ((c.cooldown || 0) > 0) {
      readyBadgeHtml = `<span class="badge badge-amber num">冷却 ${fmtInt(c.cooldown)}</span>`;
    } else if ((c.busy || 0) > 0) {
      readyBadgeHtml = `<span class="badge badge-blue num">繁忙 ${fmtInt(c.busy)}</span>`;
    } else {
      readyBadgeHtml = `<span class="badge badge-green">全员就绪</span>`;
    }

    const readinessSubBits = [`<span style="color:var(--green-text);font-weight:600;">就绪 ${fmtInt(c.ready)}</span>`];
    if ((c.busy || 0) > 0) readinessSubBits.push(`<span style="color:var(--blue-text);font-weight:600;">繁忙 ${fmtInt(c.busy)}</span>`);
    if ((c.cooldown || 0) > 0) readinessSubBits.push(`<span style="color:var(--amber-text);font-weight:600;">冷却 ${fmtInt(c.cooldown)}</span>`);
    if (abnormalCnt > 0) readinessSubBits.push(`<span style="color:var(--red-text);font-weight:600;">异常 ${fmtInt(abnormalCnt)}</span>`);
    if ((ais.active_requests || 0) > 0) readinessSubBits.push(`<span>并发 ${fmtInt(ais.active_requests)}</span>`);
    if ((c.busy || 0) === 0 && (c.cooldown || 0) === 0 && abnormalCnt === 0) {
      readinessSubBits.push(`<span>0 冷却 · 全员待命</span>`);
    }

    // Card 2: Playground traffic + Input/Reasoning/Reply proportion bar + 24px Sparkline
    const inputTok = pg.input_tokens || 0;
    const replyTok = pg.reply_tokens || 0;
    const totalTok = pg.total_tokens || 0;
    const reasonTok = Math.max(0, totalTok - inputTok - replyTok);
    const tokSum = Math.max(1, inputTok + reasonTok + replyTok);
    const inPct = (inputTok / tokSum) * 100;
    const rsPct = (reasonTok / tokSum) * 100;
    const rpPct = (replyTok / tokSum) * 100;

    const failOtherCnt = (pg.failed || 0) + (pg.canceled || 0);
    const ratioBarHtml = totalTok > 0 ? `
      <div class="tok-ratio-wrap" title="输入 ${fmtCompact(inputTok)} · 思考 ${fmtCompact(reasonTok)} · 回复 ${fmtCompact(replyTok)}">
        <div class="tok-ratio-bar">
          ${inputTok > 0 ? `<div class="tok-ratio-seg seg-input" style="width:${inPct.toFixed(2)}%"></div>` : ''}
          ${reasonTok > 0 ? `<div class="tok-ratio-seg seg-reason" style="width:${rsPct.toFixed(2)}%"></div>` : ''}
          ${replyTok > 0 ? `<div class="tok-ratio-seg seg-reply" style="width:${rpPct.toFixed(2)}%"></div>` : ''}
        </div>
        <div class="tok-ratio-meta num">
          <span><i class="tok-dot" style="background:var(--tok-input)"></i>入 ${fmtCompact(inputTok)}</span>
          ${reasonTok > 0 ? `<span><i class="tok-dot" style="background:var(--tok-reason)"></i>思 ${fmtCompact(reasonTok)}</span>` : ''}
          <span><i class="tok-dot" style="background:var(--tok-reply)"></i>出 ${fmtCompact(replyTok)}</span>
        </div>
      </div>
    ` : '';

    // Card 3: TTFT (Big P50, subline Avg & P95 — no duplicate P50!)
    const ttftMain = pg.first_event_p50_ms > 0 ? fmtMs(pg.first_event_p50_ms) : fmtMs(pg.first_event_avg_ms);

    const kpiHtml = `
      <div class="kpi-grid kpi-grid-3">
        <div class="kpi-card">
          <div class="kpi-label">
            <span>号池就绪度</span>
            ${readyBadgeHtml}
          </div>
          <div class="kpi-val num">
            <span>${fmtInt(c.enabled)}</span>
            <span class="kpi-unit">启用账号</span>
          </div>
          <div class="kpi-sub">${tierStr}</div>
          <div class="kpi-sub num" style="margin-top:auto;">${readinessSubBits.join('<span>·</span>')}</div>
        </div>

        <div class="kpi-card">
          <div class="kpi-label">
            <span>本轮 Playground 流量</span>
            ${pg.success_rate != null ? `<span class="badge ${healthBadgeClass(pg.success_rate)} num">成功率 ${fmtPct(pg.success_rate, 1)}</span>` : ''}
          </div>
          <div class="kpi-val num">
            <span>${fmtInt(pg.requests)}</span>
            <span class="kpi-unit">次 · ${fmtCompact(totalTok)} Token</span>
          </div>
          <div class="kpi-sub num">
            <span style="color:var(--green-text)">成功 ${fmtInt(pg.succeeded)}</span>
            <span style="color:${pg.rate_limited > 0 ? 'var(--amber-text);font-weight:700' : 'var(--text-3)'}">429: ${fmtInt(pg.rate_limited)}</span>
            <span style="color:${pg.failed > 0 ? 'var(--red-text);font-weight:600' : 'var(--text-3)'}">异常/取消 ${fmtInt(failOtherCnt)}</span>
          </div>
          ${ratioBarHtml}
          ${renderMiniSparklineHTML(slots)}
        </div>

        <div class="kpi-card">
          <div class="kpi-label">
            <span>首字延迟 (TTFT)</span>
            <span class="badge badge-slate">P50 中位数</span>
          </div>
          <div class="kpi-val num">${ttftMain}</div>
          <div class="kpi-sub num">
            <span>均值 <strong style="color:var(--text)">${fmtMs(pg.first_event_avg_ms)}</strong></span>
            <span>·</span>
            <span>P95 <strong style="color:var(--text)">${fmtMs(pg.first_event_p95_ms)}</strong></span>
          </div>
          <div class="kpi-sub num" style="margin-top:auto;color:var(--text-3);">
            <span>周期起点 ${fmtDateTime(ais.cycle_start_ms)}</span>
            ${pg.active_cooldowns > 0 ? `<span style="color:var(--amber-text);font-weight:600;">· 活跃冷却 ${pg.active_cooldowns}</span>` : ''}
          </div>
        </div>
      </div>
    `;

    // 24h Rhythm Chart Header Stats (Peak Hour & Hourly Average)
    const metric = state.aisHourlyMetric;
    const elapsedSlots = slots.filter(s => !s.isFuture);
    const valOf = (s) => metric === 'tokens' ? (s.total_tokens || 0) : (s.requests || 0);
    let peakSlot = null;
    let sumElapsed = 0;
    for (const s of elapsedSlots) {
      const v = valOf(s);
      sumElapsed += v;
      if (!peakSlot || v > valOf(peakSlot)) peakSlot = s;
    }
    const peakVal = peakSlot ? valOf(peakSlot) : 0;
    const nowMs = state.snapshot?.generated_at_ms || Date.now();
    const elapsedHours = (ais.cycle_start_ms > 0)
      ? Math.max(1, Math.min(24, (nowMs - ais.cycle_start_ms) / 3600000))
      : Math.max(1, elapsedSlots.length);
    const avgVal = elapsedSlots.length > 0 ? sumElapsed / elapsedHours : 0;
    const peakChip = peakVal > 0
      ? `<span class="micro-chip num">峰值 <strong>${esc(peakSlot.label)}</strong> · ${metric === 'tokens' ? fmtCompact(peakVal) : fmtInt(peakVal) + '次'}</span>`
      : `<span class="micro-chip">暂无峰值</span>`;
    const avgChip = `<span class="micro-chip num">时均 <strong>${metric === 'tokens' ? fmtCompact(avgVal) : (avgVal >= 10 ? Math.round(avgVal) : avgVal.toFixed(1))}</strong>${metric === 'tokens' ? '/h' : ' 次/h'}</span>`;

    const hasAnyTtft = elapsedSlots.some(s => (s.first_event_p50_ms || 0) > 0);
    const ttftToggleHtml = metric === 'requests' && hasAnyTtft ? `
      <button type="button" class="ttft-toggle-btn ${state.aisShowTtft ? 'active' : ''}" id="btn-ais-ttft" title="叠加显示各小时首字延迟 P50 趋势折线">
        <i class="sw line-ttft"></i>TTFT P50
      </button>
    ` : '';

    const trendLegendHtml = metric === 'requests'
      ? `<span class="lamp-legend-item"><i class="sw high"></i>成功</span>
         <span class="lamp-legend-item"><i class="sw med"></i>429 限流</span>
         <span class="lamp-legend-item"><i class="sw low"></i>其他失败</span>
         ${state.aisShowTtft && hasAnyTtft ? `<span class="lamp-legend-item"><i class="sw line-ttft"></i>TTFT P50</span>` : ''}`
      : `<span class="lamp-legend-item"><i class="sw tok-in"></i>输入 Input</span>
         <span class="lamp-legend-item"><i class="sw tok-rs"></i>思考 Reasoning</span>
         <span class="lamp-legend-item"><i class="sw tok-rp"></i>回复 Reply</span>`;

    const hourlyCardHtml = `
      <div class="ais-trend-card">
        <div class="ais-trend-head">
          <div class="ais-trend-left">
            <span class="card-title">本轮 24h 分时节律（16:00 → 次日 15:00）</span>
            <div class="ais-trend-legend">${trendLegendHtml}</div>
          </div>
          <div class="ais-trend-right">
            <div class="ais-stat-chips">${peakChip}${avgChip}</div>
            ${ttftToggleHtml}
            <div class="seg seg-sm" role="group" aria-label="分时指标">
              <button type="button" class="${metric === 'requests' ? 'active' : ''}" data-ais-metric="requests">请求数</button>
              <button type="button" class="${metric === 'tokens' ? 'active' : ''}" data-ais-metric="tokens">Token</button>
            </div>
          </div>
        </div>
        <div class="ais-scrub-bar" id="ais-scrub-bar" hidden></div>
        <div class="ais-trend-box" id="ais-hourly-chart"></div>
      </div>
    `;

    // ---------- Model Quota Matrix (Desktop Table + Mobile Zero-Horizontal-Scroll Cards) ----------
    const allModels = ais.models || [];
    const usedCnt = allModels.filter(m => m.used_requests > 0 || m.used_tokens > 0 || m.effective_percent < 100 - LEVEL_EPSILON).length;
    const g4Cnt = allModels.filter(isGemini4Model).length;
    const imgCnt = allModels.filter(isImageModel).length;
    const g3Cnt = allModels.filter(m => !isGemini4Model(m) && !isImageModel(m)).length;

    if (state.aisModelGroup === 'IMAGE' && imgCnt === 0) {
      state.aisModelGroup = 'ALL';
    }

    let visibleModels = allModels.filter(m => {
      if (state.aisModelGroup === 'USED') return m.used_requests > 0 || m.used_tokens > 0 || m.effective_percent < 100 - LEVEL_EPSILON;
      if (state.aisModelGroup === 'G4') return isGemini4Model(m);
      if (state.aisModelGroup === 'G3') return !isGemini4Model(m) && !isImageModel(m);
      if (state.aisModelGroup === 'IMAGE') return isImageModel(m);
      return true;
    });

    if (state.aisModelSort === 'remaining_asc') {
      visibleModels = visibleModels.slice().sort((a, b) => {
        if (a.effective_percent !== b.effective_percent) return a.effective_percent - b.effective_percent;
        return (b.used_requests || 0) - (a.used_requests || 0);
      });
    }

    const focusPillHtml = state.selectedModel ? `
      <button type="button" class="focus-banner-pill" id="matrix-clear-focus" title="点击清除模型聚焦">
        <span>当前聚焦：${esc(modelLabelMap.get(state.selectedModel) || state.selectedModel)}</span>
        <span aria-hidden="true">×</span>
      </button>
    ` : '';

    const desktopRowsHtml = visibleModels.length === 0
      ? `<tr><td colspan="5" style="text-align:center;color:var(--text-3);padding:24px;">当前分组暂无匹配模型</td></tr>`
      : visibleModels.map(m => {
          const tiers = (m.eligible_tiers || []).map(t => `<span class="badge ${tierBadgeClass(t)}" style="font-size:10px;padding:1px 6px;">${esc(t)}</span>`).join('');
          const availBadgeCls = m.eligible_accounts === 0 ? 'badge-slate'
            : m.available_accounts === 0 ? 'badge-red'
            : m.available_accounts < m.eligible_accounts ? 'badge-amber'
            : 'badge-green';

          const issueBits = [];
          if (m.cooling_accounts > 0) issueBits.push(`冷却 ${m.cooling_accounts}`);
          if (m.exhausted_accounts > 0) issueBits.push(`耗尽 ${m.exhausted_accounts}`);
          if (m.unavailable_accounts > 0) issueBits.push(`异常 ${m.unavailable_accounts}`);

          const recoveryHtml = m.earliest_recovery_ms
            ? `<span style="color:var(--amber-text)">最早 <span class="cd-target num" data-ts="${m.earliest_recovery_ms}" data-suffix="后恢复">${fmtCountdown(m.earliest_recovery_ms, '后恢复')}</span></span>`
            : '';

          const dims = computeModelDimensions(m);
          const selCls = state.selectedModel === m.id ? 'selected' : '';
          const usedCls = m.used_requests > 0 ? 'has-usage' : '';
          const rowAccent = pctTextColor(m.effective_percent);
          const isEmpty = m.eligible_accounts > 0 && quotaLevel(m.effective_percent) === 'empty';
          const reqBarCls = m.eligible_accounts === 0 ? 'bar-slate' : barColorClass(dims.reqPct);

          const tokenCellHtml = dims.hasTokenLimit
            ? `<div><strong>${fmtCompact(m.effective_remaining_tokens)}</strong> <span style="color:var(--text-3)">/ ${fmtCompact(m.limit_tokens)}</span></div>
               <div class="mini-sub">已用 ${fmtCompact(m.used_tokens)}</div>`
            : `<div class="token-unlimited-label">按次计额</div>
               <div class="mini-sub">${(m.used_tokens || 0) > 0 ? `已耗 ${fmtCompact(m.used_tokens)}` : '无 Token 限额'}</div>`;

          const dimBarsHtml = dims.hasTokenLimit
            ? `<div class="dual-bars">
                 <div class="dual-bar-item ${dims.bottleneck === 'requests' ? 'is-bottleneck' : ''}" title="次数剩余 ${fmtPct(dims.reqPct, 1)}">
                   <div class="dual-bar-top num"><span>次数</span><span>${fmtPct(dims.reqPct, 1)}</span></div>
                   <div class="bar-track bar-track-xs"><div class="bar-fill ${reqBarCls}" style="width:${dims.reqPct}%"></div></div>
                 </div>
                 <div class="dual-bar-item ${dims.bottleneck === 'tokens' ? 'is-bottleneck' : ''}" title="Token 剩余 ${fmtPct(dims.tokPct, 1)}">
                   <div class="dual-bar-top num"><span>Token</span><span>${fmtPct(dims.tokPct, 1)}</span></div>
                   <div class="bar-track bar-track-xs"><div class="bar-fill ${m.eligible_accounts === 0 ? 'bar-slate' : barColorClass(dims.tokPct)}" style="width:${dims.tokPct}%"></div></div>
                 </div>
               </div>`
            : `<div class="dual-bars dual-bars-single">
                 <div class="dual-bar-item" title="次数剩余 ${fmtPct(dims.reqPct, 1)}">
                   <div class="dual-bar-top num"><span>次数余量</span><span>${fmtPct(dims.reqPct, 1)}</span></div>
                   <div class="bar-track bar-track-xs"><div class="bar-fill ${reqBarCls}" style="width:${dims.reqPct}%"></div></div>
                 </div>
               </div>`;

          return `
            <tr class="clickable ${selCls} ${usedCls}" style="--row-accent:${rowAccent};" data-select-model="${esc(m.id)}">
              <td>
                <div class="model-friendly">${esc(m.label || m.id)}</div>
                <div class="model-sub-row">
                  <span class="model-id-code">${esc(m.id)}</span>
                  ${tiers}
                </div>
              </td>
              <td>
                <span class="badge ${availBadgeCls} num">${m.available_accounts} / ${m.eligible_accounts}</span>
                ${issueBits.length > 0 ? `<div class="mini-sub">${esc(issueBits.join(' · '))}</div>` : ''}
              </td>
              <td class="num">
                <div><strong>${fmtInt(m.effective_remaining_requests)}</strong> <span style="color:var(--text-3)">/ ${fmtInt(m.limit_requests)}</span></div>
                <div class="mini-sub">已用 ${fmtInt(m.used_requests)}</div>
              </td>
              <td class="num">
                ${tokenCellHtml}
              </td>
              <td class="prog-cell">
                <div class="prog-row">
                  <div class="bar-track ${isEmpty ? 'is-empty' : ''}">
                    <div class="bar-fill ${m.eligible_accounts === 0 ? 'bar-slate' : barColorClass(m.effective_percent)}" style="width:${Math.max(0, Math.min(100, m.effective_percent))}%"></div>
                  </div>
                  <div class="prog-pct num" style="color:${m.eligible_accounts === 0 ? 'var(--text-3)' : rowAccent}">${fmtPct(m.effective_percent, 1)}</div>
                </div>
                ${dimBarsHtml}
                <div class="bottleneck-meta">
                  ${bottleneckTagHTML(dims.bottleneck)}
                  <span>${recoveryHtml}</span>
                </div>
              </td>
            </tr>
          `;
        }).join('');

    const mobileCardsHtml = visibleModels.length === 0
      ? `<div class="empty-box" style="padding:20px;">当前分组暂无匹配模型</div>`
      : visibleModels.map(m => {
          const tiers = (m.eligible_tiers || []).map(t => `<span class="badge ${tierBadgeClass(t)}" style="font-size:10px;padding:1px 6px;">${esc(t)}</span>`).join('');
          const availBadgeCls = m.eligible_accounts === 0 ? 'badge-slate'
            : m.available_accounts === 0 ? 'badge-red'
            : m.available_accounts < m.eligible_accounts ? 'badge-amber'
            : 'badge-green';
          const dims = computeModelDimensions(m);
          const selCls = state.selectedModel === m.id ? 'selected' : '';
          const usedCls = m.used_requests > 0 ? 'has-usage' : '';
          const rowAccent = pctTextColor(m.effective_percent);
          const isEmpty = m.eligible_accounts > 0 && quotaLevel(m.effective_percent) === 'empty';
          const reqBarCls = m.eligible_accounts === 0 ? 'bar-slate' : barColorClass(dims.reqPct);

          const mobileRightDimHtml = dims.hasTokenLimit
            ? `<div class="m-dim-box">
                 <div class="m-dim-head num"><span>Token 余量</span><span>${fmtPct(dims.tokPct, 1)}</span></div>
                 <div class="m-dim-val num">${fmtCompact(m.effective_remaining_tokens)} <span style="font-weight:500;color:var(--text-3)">/ ${fmtCompact(m.limit_tokens)}</span></div>
                 <div class="bar-track bar-track-xs"><div class="bar-fill ${m.eligible_accounts === 0 ? 'bar-slate' : barColorClass(dims.tokPct)}" style="width:${dims.tokPct}%"></div></div>
               </div>`
            : `<div class="m-dim-box">
                 <div class="m-dim-head"><span>已耗 Token</span><span>不限额</span></div>
                 <div class="m-dim-val num m-dim-val-muted">${(m.used_tokens || 0) > 0 ? fmtCompact(m.used_tokens) : '0'}</div>
                 <div class="m-dim-note">按次计额，无 Token 上限</div>
               </div>`;

          return `
            <div class="m-card ${selCls} ${usedCls}" style="--row-accent:${rowAccent};" data-select-model="${esc(m.id)}">
              <div class="m-card-top">
                <div style="min-width:0;">
                  <div class="model-friendly">${esc(m.label || m.id)}</div>
                  <div class="model-sub-row">
                    <span class="model-id-code">${esc(m.id)}</span>
                    ${tiers}
                  </div>
                </div>
                <div class="m-card-pct num" style="color:${m.eligible_accounts === 0 ? 'var(--text-3)' : rowAccent}">${fmtPct(m.effective_percent, 1)}</div>
              </div>
              <div class="bar-track ${isEmpty ? 'is-empty' : ''}">
                <div class="bar-fill ${m.eligible_accounts === 0 ? 'bar-slate' : barColorClass(m.effective_percent)}" style="width:${Math.max(0, Math.min(100, m.effective_percent))}%"></div>
              </div>
              <div class="m-card-mid">
                <span class="badge ${availBadgeCls} num">可用 ${m.available_accounts} / ${m.eligible_accounts}</span>
                ${bottleneckTagHTML(dims.bottleneck)}
              </div>
              <div class="m-card-dims">
                <div class="m-dim-box">
                  <div class="m-dim-head num"><span>次数余量</span><span>${fmtPct(dims.reqPct, 1)}</span></div>
                  <div class="m-dim-val num">${fmtInt(m.effective_remaining_requests)} <span style="font-weight:500;color:var(--text-3)">/ ${fmtInt(m.limit_requests)}</span></div>
                  <div class="bar-track bar-track-xs"><div class="bar-fill ${reqBarCls}" style="width:${dims.reqPct}%"></div></div>
                </div>
                ${mobileRightDimHtml}
              </div>
            </div>
          `;
        }).join('');

    const groupDefs = [
      { key: 'ALL', label: '全部', cnt: allModels.length },
      { key: 'USED', label: '有消耗', cnt: usedCnt },
      { key: 'G4', label: 'Gemini 4', cnt: g4Cnt },
      { key: 'G3', label: '3.x 系列', cnt: g3Cnt },
    ];
    if (imgCnt > 0) {
      groupDefs.push({ key: 'IMAGE', label: 'Nano Banana', cnt: imgCnt });
    }

    const tableCard = `
      <div class="card">
        <div class="card-header">
          <div>
            <div class="card-title">Playground 全部 ${allModels.length} 个模型配额矩阵${imgCnt > 0 ? '（文本双维度 · 画图按次）' : '（双维度短板熔断）'}</div>
            <div class="card-note">${esc(ais.reset_rule_note)} · 点击模型行或卡片聚焦下方单号明细</div>
          </div>
          ${focusPillHtml}
        </div>
        <div class="matrix-toolbar">
          <div class="matrix-filters" role="group" aria-label="模型分组过滤">
            ${groupDefs.map(g => `
              <button type="button" class="filter-pill ${state.aisModelGroup === g.key ? 'active' : ''}" data-ais-group="${esc(g.key)}">
                <span>${esc(g.label)}</span>
                <span class="filter-pill-cnt num">(${g.cnt})</span>
              </button>
            `).join('')}
          </div>
          <div class="matrix-right">
            <div class="seg seg-sm" role="group" aria-label="模型排序">
              <button type="button" class="${state.aisModelSort === 'default' ? 'active' : ''}" data-ais-msort="default">默认顺序</button>
              <button type="button" class="${state.aisModelSort === 'remaining_asc' ? 'active' : ''}" data-ais-msort="remaining_asc">余量升序</button>
            </div>
          </div>
        </div>
        <div class="model-table-desktop">
          <table>
            <thead>
              <tr>
                <th>模型 / 适用档位</th>
                <th>可用 / 合格号</th>
                <th>剩余次数 / 总额</th>
                <th>剩余 Token / 总额</th>
                <th>综合有效剩余 &amp; 计额维度</th>
              </tr>
            </thead>
            <tbody>${desktopRowsHtml}</tbody>
          </table>
        </div>
        <div class="model-cards-mobile">${mobileCardsHtml}</div>
      </div>
    `;

    const detailHtml = state.viewMode === 'full' ? renderAISAccountDetails(ais) : '';

    return `
      <section class="section" id="section-aistudio">
        <div class="section-head">
          <div class="section-title-wrap">
            <h2 class="section-title">AIStudio2API 号池</h2>
            <span class="section-sub">Playground 通道</span>
            ${tierStr}
          </div>
          ${resetBadge}
        </div>
        ${kpiHtml}
        ${hourlyCardHtml}
        ${tableCard}
        ${detailHtml}
      </section>
    `;
  }

  // ---------- Render AIStudio 24h Rhythm Chart SVG (Bars + 3-color Stacked Tokens + TTFT Line) ----------
  function renderAISHourlyChartSVG() {
    const box = document.getElementById('ais-hourly-chart');
    const scrubBar = document.getElementById('ais-scrub-bar');
    const ais = state.snapshot?.aistudio;
    if (!box || !ais || !ais.online) return;

    const slots = build24HourSlots(ais);
    if (!slots.length) return;

    const metric = state.aisHourlyMetric;
    const W = Math.max(260, Math.round(box.clientWidth || 760));
    const isMobile = W < 560;
    const showTtft = metric === 'requests' && state.aisShowTtft && slots.some(s => !s.isFuture && s.first_event_p50_ms > 0);

    const H = isMobile ? 128 : 132;
    const padL = isMobile ? 34 : 42;
    const padR = showTtft ? (isMobile ? 38 : 46) : (isMobile ? 8 : 12);
    const padT = 10;
    const padB = isMobile ? 22 : 24;
    const plotW = Math.max(120, W - padL - padR);
    const plotH = H - padT - padB;

    const values = slots.map(s => metric === 'tokens' ? (s.total_tokens || 0) : (s.requests || 0));
    const hasUsage = values.some(v => v > 0);
    const maxVal = Math.max(1, ...values);
    const maxTtft = showTtft ? Math.max(1000, ...slots.map(s => s.first_event_p50_ms || 0)) : 1;

    const svg = svgEl('svg', { viewBox: `0 0 ${W} ${H}` });
    const defs = svgEl('defs');
    const reqGrad = svgEl('linearGradient', { id: 'ais_req_grad', x1: 0, x2: 0, y1: 0, y2: 1 });
    reqGrad.appendChild(svgEl('stop', { offset: '0%', 'stop-color': cssVar('--q-high') }));
    reqGrad.appendChild(svgEl('stop', { offset: '100%', 'stop-color': cssVar('--q-full'), 'stop-opacity': 0.85 }));
    defs.appendChild(reqGrad);
    svg.appendChild(defs);

    const colBorder = cssVar('--border');
    const colText3 = cssVar('--text-3');
    const colMed = cssVar('--q-medium');
    const colLow = cssVar('--q-low');
    const colCancel = cssVar('--border-strong');
    const colSelect = cssVar('--chart-select');
    const colLane = cssVar('--chart-lane');
    const colFuture = cssVar('--chart-future');
    const colFutureStroke = cssVar('--chart-future-stroke');
    const colTokIn = cssVar('--tok-input');
    const colTokRs = cssVar('--tok-reason');
    const colTokRp = cssVar('--tok-reply');
    const colTtft = cssVar('--ttft-line');
    const colSurface = cssVar('--surface');

    // Horizontal grid & Left Y-axis ticks (+ Right TTFT axis ticks when enabled)
    [0, 0.5, 1].forEach(ratio => {
      const y = padT + (1 - ratio) * plotH;
      svg.appendChild(svgEl('line', {
        x1: padL, x2: W - padR, y1: y, y2: y,
        stroke: colBorder, 'stroke-dasharray': ratio === 0 ? 'none' : '2 4',
      }));
      if (ratio > 0) {
        const lbl = hasUsage
          ? (metric === 'tokens' ? fmtCompact(maxVal * ratio) : fmtCompact(Math.round(maxVal * ratio)))
          : (ratio === 1 ? '1' : '');
        if (lbl) {
          svg.appendChild(svgText({
            x: padL - 5, y: y + 3.5,
            'text-anchor': 'end', 'font-size': isMobile ? 9.5 : 10, fill: colText3,
          }, lbl));
        }
        if (showTtft) {
          svg.appendChild(svgText({
            x: W - padR + 5, y: y + 3.5,
            'text-anchor': 'start', 'font-size': isMobile ? 9 : 9.5, fill: colTtft,
          }, fmtMs(maxTtft * ratio)));
        }
      }
    });

    const n = slots.length;
    const slotW = plotW / Math.max(1, n);
    const barW = Math.max(5, Math.min(26, slotW * 0.66));
    const maxLabels = Math.max(4, Math.floor(plotW / (isMobile ? 42 : 52)));
    const labelEvery = Math.max(1, Math.ceil(n / maxLabels));

    const hoverBand = svgEl('rect', { y: padT, height: plotH, width: slotW, fill: colSelect, rx: 4, visibility: 'hidden', 'pointer-events': 'none' });
    svg.appendChild(hoverBand);

    let lastLabelX = -999;
    const ttftRuns = [];
    let curTtftRun = [];

    slots.forEach((b, i) => {
      const cx = padL + (i + 0.5) * slotW;
      const bx = cx - barW / 2;

      if (b.isFuture) {
        // Subtle dashed placeholder slot for upcoming hours in the 24h cycle
        svg.appendChild(svgEl('rect', {
          x: bx + 0.5, y: padT + 4, width: Math.max(3, barW - 1), height: Math.max(4, plotH - 4),
          rx: 3, fill: colFuture, stroke: colFutureStroke, 'stroke-width': 1, 'stroke-dasharray': '2 3',
        }));
      } else {
        // Subtle elapsed rail background
        svg.appendChild(svgEl('rect', {
          x: bx, y: padT, width: barW, height: plotH,
          rx: 3, fill: colLane,
        }));

        if (metric === 'tokens') {
          const total = b.total_tokens || 0;
          if (total > 0) {
            const totalH = Math.max(4, (total / maxVal) * plotH);
            const inTok = b.input_tokens || 0;
            const rsTok = b.reasoning_tokens || 0;
            const rpTok = b.reply_tokens || 0;
            const sumParts = Math.max(1, inTok + rsTok + rpTok);
            const segs = [
              { val: inTok, color: colTokIn },
              { val: rsTok, color: colTokRs },
              { val: rpTok, color: colTokRp },
            ].filter(s => s.val > 0);
            let accH = 0;
            segs.forEach((s, si) => {
              const segH = si === segs.length - 1 ? Math.max(1.5, totalH - accH) : Math.max(1.5, (s.val / sumParts) * totalH);
              svg.appendChild(svgEl('rect', {
                x: bx, y: padT + plotH - accH - segH, width: barW, height: segH,
                rx: si === segs.length - 1 ? 2.5 : 0.8,
                fill: s.color, 'fill-opacity': 0.9,
              }));
              accH += segH;
            });
          }
        } else {
          const total = b.requests || 0;
          if (total > 0) {
            const totalH = Math.max(4, (total / maxVal) * plotH);
            const segs = [
              { val: b.succeeded || 0, fill: 'url(#ais_req_grad)' },
              { val: b.rate_limited || 0, fill: colMed },
              { val: b.other_failed || 0, fill: colLow },
              { val: b.canceled || 0, fill: colCancel },
            ].filter(s => s.val > 0);
            let accH = 0;
            segs.forEach((s, si) => {
              const segH = si === segs.length - 1 ? Math.max(1.5, totalH - accH) : Math.max(1.5, (s.val / total) * totalH);
              svg.appendChild(svgEl('rect', {
                x: bx, y: padT + plotH - accH - segH, width: barW, height: segH,
                rx: si === segs.length - 1 ? 2.5 : 0.8,
                fill: s.fill, 'fill-opacity': 0.92,
              }));
              accH += segH;
            });
          }
        }

        // Collect TTFT P50 polyline points
        if (showTtft) {
          if (b.first_event_p50_ms > 0) {
            const ty = padT + (1 - Math.min(1, b.first_event_p50_ms / maxTtft)) * plotH;
            curTtftRun.push([cx, ty]);
          } else if (curTtftRun.length) {
            ttftRuns.push(curTtftRun);
            curTtftRun = [];
          }
        }
      }

      if ((i % labelEvery === 0 || i === n - 1) && (cx - lastLabelX >= (isMobile ? 34 : 42))) {
        svg.appendChild(svgText({
          x: cx, y: H - 5,
          'text-anchor': 'middle',
          'font-size': isMobile ? 9.5 : 10,
          'font-weight': b.isCurrentHour ? 700 : 400,
          fill: b.isCurrentHour ? cssVar('--accent') : colText3,
        }, b.label || ''));
        lastLabelX = cx;
      }
    });

    if (curTtftRun.length) ttftRuns.push(curTtftRun);

    // Draw TTFT P50 smooth overlay curve + dots
    if (showTtft) {
      for (const run of ttftRuns) {
        if (run.length >= 2) {
          svg.appendChild(svgEl('path', {
            d: monotonePath(run),
            fill: 'none',
            stroke: colTtft,
            'stroke-width': 1.85,
            'stroke-linecap': 'round',
            'stroke-linejoin': 'round',
            'pointer-events': 'none',
          }));
        }
        for (const pt of run) {
          svg.appendChild(svgEl('circle', {
            cx: pt[0], cy: pt[1], r: 2.8,
            fill: colTtft, stroke: colSurface, 'stroke-width': 1.3,
            'pointer-events': 'none',
          }));
        }
      }
    }

    if (!hasUsage) {
      svg.appendChild(svgText({
        x: padL + plotW / 2, y: padT + plotH / 2 + 4,
        'text-anchor': 'middle', 'font-size': 12, fill: colText3,
      }, '本轮 16:00 重置以来暂无 Playground 调用记录'));
    }

    box.innerHTML = '';
    box.appendChild(svg);
    const tip = document.createElement('div');
    tip.className = 'lamp-tip';
    tip.hidden = true;
    box.appendChild(tip);

    let aisInlineActive = false;
    function clearAISHover() {
      aisInlineActive = false;
      hoverBand.setAttribute('visibility', 'hidden');
      tip.hidden = true;
      if (scrubBar) scrubBar.hidden = true;
    }
    scrubClearers.set('ais_hourly', () => {
      if (aisInlineActive) clearAISHover();
    });

    const hit = svgEl('rect', { x: padL, y: padT, width: plotW, height: plotH, fill: 'transparent', style: 'cursor:pointer' });
    function showIdx(clientX, inlineScrub) {
      const rect = svg.getBoundingClientRect();
      const relX = ((clientX - rect.left) / rect.width) * W;
      const idx = Math.max(0, Math.min(n - 1, Math.floor((relX - padL) / slotW)));
      const b = slots[idx];
      hoverBand.setAttribute('x', padL + idx * slotW);
      hoverBand.setAttribute('visibility', 'visible');

      const sr = b.requests > 0 ? ((b.succeeded / b.requests) * 100).toFixed(1).replace(/\.0$/, '') + '%' : '—';
      const endLbl = b.timestamp_ms ? fmtHM(b.timestamp_ms + 3600 * 1000) : '';

      if ((inlineScrub || isMobile) && scrubBar) {
        aisInlineActive = true;
        tip.hidden = true;
        scrubBar.hidden = false;
        if (b.isFuture) {
          scrubBar.innerHTML = `
            <div class="ais-scrub-metrics num">
              <strong class="mono" style="color:var(--accent);">${esc(b.label)}${endLbl ? '–' + esc(endLbl) : ''}</strong>
              <span style="color:var(--text-3);">未来时段（尚未到来）</span>
            </div>
            <button type="button" class="lamp-scrub-reset" data-ais-clear="1">×</button>
          `;
        } else if (metric === 'tokens') {
          scrubBar.innerHTML = `
            <div class="ais-scrub-metrics num">
              <strong class="mono" style="color:var(--accent);">${esc(b.label)}${endLbl ? '–' + esc(endLbl) : ''}</strong>
              <span>总 <strong>${fmtCompact(b.total_tokens)}</strong></span>
              <span style="color:var(--tok-input);">入 ${fmtCompact(b.input_tokens)}</span>
              <span style="color:var(--tok-reason);">思 ${fmtCompact(b.reasoning_tokens)}</span>
              <span style="color:var(--tok-reply);">出 ${fmtCompact(b.reply_tokens)}</span>
            </div>
            <button type="button" class="lamp-scrub-reset" data-ais-clear="1">×</button>
          `;
        } else {
          scrubBar.innerHTML = `
            <div class="ais-scrub-metrics num">
              <strong class="mono" style="color:var(--accent);">${esc(b.label)}${endLbl ? '–' + esc(endLbl) : ''}</strong>
              <span><strong>${fmtInt(b.requests)}</strong> 次 (${sr})</span>
              <span style="color:${b.rate_limited > 0 ? 'var(--amber-text)' : 'var(--text-3)'};">429: ${fmtInt(b.rate_limited)}</span>
              <span>Token <strong>${fmtCompact(b.total_tokens)}</strong></span>
              ${b.first_event_p50_ms > 0 ? `<span style="color:var(--ttft-line);">TTFT ${fmtMs(b.first_event_p50_ms)}</span>` : ''}
            </div>
            <button type="button" class="lamp-scrub-reset" data-ais-clear="1">×</button>
          `;
        }
        const cBtn = scrubBar.querySelector('[data-ais-clear]');
        if (cBtn) cBtn.addEventListener('click', (ev) => { ev.stopPropagation(); clearAISHover(); });
        return;
      }

      if (scrubBar) scrubBar.hidden = true;
      if (b.isFuture) {
        tip.innerHTML = `
          <div class="lamp-tip-time">${esc(b.label)}${endLbl ? ' – ' + esc(endLbl) : ''}</div>
          <div class="lamp-tip-row"><span style="color:var(--text-3);">未来时段（尚未到来）</span></div>
        `;
      } else {
        tip.innerHTML = `
          <div class="lamp-tip-time">${esc(b.label)}${endLbl ? ' – ' + esc(endLbl) : ''}${b.isCurrentHour ? ' · 当前小时' : ''}</div>
          <div class="lamp-tip-row strong"><span>请求数</span><span class="num">${fmtInt(b.requests)}（成功率 ${sr}）</span></div>
          <div class="lamp-tip-row"><span style="color:var(--green-text);">成功 ${fmtInt(b.succeeded)}</span><span style="color:${b.rate_limited > 0 ? 'var(--amber-text)' : 'var(--text-3)'};">429: ${fmtInt(b.rate_limited)} · 失败: ${fmtInt(b.other_failed)}</span></div>
          <div class="lamp-tip-row strong" style="margin-top:4px;padding-top:4px;border-top:1px solid var(--hairline);"><span>总 Token</span><span class="num">${fmtCompact(b.total_tokens)}</span></div>
          <div class="lamp-tip-row"><span class="lamp-tip-left"><i class="sw tok-in"></i>输入 Input</span><span class="num">${fmtCompact(b.input_tokens)}</span></div>
          <div class="lamp-tip-row"><span class="lamp-tip-left"><i class="sw tok-rs"></i>思考 Reasoning</span><span class="num">${fmtCompact(b.reasoning_tokens)}</span></div>
          <div class="lamp-tip-row"><span class="lamp-tip-left"><i class="sw tok-rp"></i>回复 Reply</span><span class="num">${fmtCompact(b.reply_tokens)}</span></div>
          ${b.first_event_p50_ms > 0 ? `<div class="lamp-tip-row" style="margin-top:4px;padding-top:4px;border-top:1px solid var(--hairline);"><span style="color:var(--ttft-line);">首字 P50</span><span class="num" style="font-weight:700;color:var(--ttft-line);">${fmtMs(b.first_event_p50_ms)}</span></div>` : ''}
        `;
      }
      tip.hidden = false;
      const boxW = box.clientWidth || W;
      const scale = boxW / W;
      const anchorX = (padL + (idx + 0.5) * slotW) * scale;
      const tipW = tip.offsetWidth || 205;
      let left = anchorX + 12;
      if (left + tipW + 6 > boxW) left = Math.max(4, anchorX - tipW - 12);
      tip.style.left = left + 'px';
      tip.style.top = '4px';
    }

    hit.addEventListener('pointerdown', e => {
      if (e.pointerType === 'touch' || isMobile) showIdx(e.clientX, true);
    });
    hit.addEventListener('pointermove', e => {
      if (e.pointerType === 'touch') return;
      showIdx(e.clientX, isMobile);
    });
    let aisTouchX = 0, aisTouchY = 0;
    hit.addEventListener('touchstart', e => {
      if (e.touches[0]) {
        aisTouchX = e.touches[0].clientX;
        aisTouchY = e.touches[0].clientY;
        showIdx(aisTouchX, true);
      }
    }, { passive: true });
    hit.addEventListener('touchmove', e => {
      if (!e.touches[0]) return;
      if (Math.abs(e.touches[0].clientX - aisTouchX) >= Math.abs(e.touches[0].clientY - aisTouchY)) {
        showIdx(e.touches[0].clientX, true);
      }
    }, { passive: true });

    svg.appendChild(hit);
    svg.addEventListener('pointerleave', e => {
      if (e.pointerType !== 'touch' && !aisInlineActive) clearAISHover();
    });
  }

  // ---------- Render AIStudio Individual Sanitized Account Details ----------
  function isAccountIssue(acc) {
    return acc.cooling_models > 0 || acc.state === 'cooldown' || acc.state === 'auth_required' || acc.state === 'unavailable' || acc.state === 'restricted';
  }

  function isAccountModelFullZero(m) {
    const hasTokenLimit = (m.limit_tokens || 0) > 0;
    const effPct = m.effective_percent || 0;
    return (
      m.status === 'available' &&
      effPct >= 100 - LEVEL_EPSILON &&
      !((m.used_requests || 0) > 0) &&
      (!hasTokenLimit || !((m.used_tokens || 0) > 0))
    );
  }

  function renderAISAccountDetails(ais) {
    if (!Array.isArray(ais.accounts)) {
      return `
        <div>
          <div class="filter-bar">
            <div class="filter-left">
              <span class="card-title">单号脱敏明细</span>
            </div>
          </div>
          <div class="empty-box">正在加载单号脱敏明细…</div>
        </div>
      `;
    }
    const models = ais.models || [];
    const accounts = ais.accounts;
    const tierCounts = ais.tier_counts || {};
    const issuesCount = accounts.filter(isAccountIssue).length;

    // Hide 0-count tier filters dynamically (e.g. Free(0))
    const tierFilters = [
      { key: 'ALL', label: `全部 (${accounts.length})` },
    ];
    for (const t of ['Ultra', 'Pro', 'Free']) {
      if ((tierCounts[t] || 0) > 0) {
        tierFilters.push({ key: t, label: `${t} (${tierCounts[t]})` });
      }
    }
    if (issuesCount > 0 || state.selectedTier === 'ISSUES') {
      tierFilters.push({ key: 'ISSUES', label: `异常/冷却 (${issuesCount})` });
    }
    if (!tierFilters.some(f => f.key === state.selectedTier)) {
      state.selectedTier = 'ALL';
    }

    const modelOpts = [
      `<option value="">全部模型（自动折叠 100% 满血）</option>`,
      ...models.map(m => `<option value="${esc(m.id)}" ${state.selectedModel === m.id ? 'selected' : ''}>聚焦：${esc(m.label || m.id)}</option>`)
    ].join('');

    let filteredAccounts = accounts.filter(acc => {
      if (state.selectedTier === 'ISSUES') return isAccountIssue(acc);
      if (state.selectedTier !== 'ALL' && acc.tier !== state.selectedTier) return false;
      return true;
    });

    // Sort accounts
    if (state.aisAccountSort === 'usage') {
      filteredAccounts = filteredAccounts.slice().sort((a, b) => {
        if ((b.used_requests || 0) !== (a.used_requests || 0)) return (b.used_requests || 0) - (a.used_requests || 0);
        return (b.used_tokens || 0) - (a.used_tokens || 0);
      });
    } else if (state.aisAccountSort === 'issues') {
      filteredAccounts = filteredAccounts.slice().sort((a, b) => {
        const ia = isAccountIssue(a) ? 1 : 0;
        const ib = isAccountIssue(b) ? 1 : 0;
        if (ib !== ia) return ib - ia;
        if ((b.cooling_models || 0) !== (a.cooling_models || 0)) return (b.cooling_models || 0) - (a.cooling_models || 0);
        return (a.available_models - a.eligible_models) - (b.available_models - b.eligible_models);
      });
    }

    const expandAllBtnHtml = !state.selectedModel ? `
      <button type="button" class="icon-btn" id="btn-ais-expand-all" title="一键切换展开或折叠各账号下的 100% 满血模型">
        ${state.aisExpandAllFull ? '折叠满血模型 ▲' : '展开全部满血 ▼'}
      </button>
    ` : '';

    const cardsHtml = filteredAccounts.length === 0
      ? `<div class="empty-box">当前筛选条件下无启用账号</div>`
      : `<div class="ais-acc-grid">${filteredAccounts.map(renderAISAccountCard).join('')}</div>`;

    return `
      <div>
        <div class="filter-bar">
          <div class="filter-left">
            <span class="card-title">单号脱敏明细（${filteredAccounts.length}）</span>
            <select class="select-input" id="ais-model-select" aria-label="按模型聚焦">${modelOpts}</select>
            ${state.selectedModel ? `<button type="button" class="icon-btn" id="ais-clear-model">清除聚焦 ×</button>` : ''}
          </div>
          <div class="filter-right">
            <div class="seg seg-sm" role="group" aria-label="账号排序">
              <button type="button" class="${state.aisAccountSort === 'default' ? 'active' : ''}" data-ais-asort="default">默认</button>
              <button type="button" class="${state.aisAccountSort === 'usage' ? 'active' : ''}" data-ais-asort="usage">消耗优先</button>
              <button type="button" class="${state.aisAccountSort === 'issues' ? 'active' : ''}" data-ais-asort="issues">异常优先</button>
            </div>
            <div class="seg seg-sm" role="group" aria-label="档位筛选">
              ${tierFilters.map(f => `
                <button type="button" class="${state.selectedTier === f.key ? 'active' : ''}" data-ais-tier="${esc(f.key)}">${esc(f.label)}</button>
              `).join('')}
            </div>
            ${expandAllBtnHtml}
          </div>
        </div>
        ${cardsHtml}
      </div>
    `;
  }

  function renderAISAccountCard(acc) {
    const eligibleModels = (acc.models || []).filter(m => m.status !== 'ineligible');
    const stateText = acc.state === 'busy' && acc.active_requests > 0
      ? `繁忙 (${acc.active_requests})`
      : acc.state_label;

    let bodyHtml = '';
    if (state.selectedModel) {
      const m = (acc.models || []).find(x => x.model_id === state.selectedModel);
      const friendly = modelLabelMap.get(state.selectedModel) || state.selectedModel;
      if (!m || m.status === 'ineligible') {
        bodyHtml = `<div class="ais-m-focus" style="opacity:0.62;">
          <div style="display:flex;justify-content:space-between;align-items:center;gap:6px;">
            <span style="font-size:12.5px;font-weight:700;">${esc(friendly)}</span>
            <span class="badge badge-slate">当前档位不支持</span>
          </div>
        </div>`;
      } else {
        const effPct = m.effective_percent || 0;
        const limReq = m.limit_requests || 0;
        const usedReq = m.used_requests || 0;
        const effRemReq = m.effective_remaining_requests || 0;
        const limTok = m.limit_tokens || 0;
        const usedTok = m.used_tokens || 0;
        const effRemTok = m.effective_remaining_tokens || 0;
        const isEmpty = quotaLevel(effPct) === 'empty';
        const hasTokenLimit = limTok > 0;
        const focusTokenSpan = hasTokenLimit
          ? `<span>Token ${fmtCompact(effRemTok)} / ${fmtCompact(limTok)}</span>`
          : `<span style="color:var(--text-3);">${usedTok > 0 ? `按次计额 · 已耗 ${fmtCompact(usedTok)} tok` : '按次计额'}</span>`;
        const cdLine = m.cooldown_until_ms && m.status === 'cooldown'
          ? `<div style="font-size:11.5px;color:var(--amber-text)">冷却恢复：<span class="cd-target num" data-ts="${m.cooldown_until_ms}" data-suffix="后恢复">${fmtCountdown(m.cooldown_until_ms, '后恢复')}</span></div>`
          : '';
        bodyHtml = `
          <div class="ais-m-focus">
            <div style="display:flex;justify-content:space-between;align-items:center;gap:6px;">
              <span style="font-size:12.5px;font-weight:700;">${esc(friendly)}</span>
              <span class="badge ${statusBadgeClass(m.status)}">${esc(m.status_label)}</span>
            </div>
            <div class="prog-row">
              <div class="bar-track ${isEmpty ? 'is-empty' : ''}">
                <div class="bar-fill ${barColorClass(effPct)}" style="width:${Math.max(0, Math.min(100, effPct))}%"></div>
              </div>
              <span class="prog-pct num" style="color:${pctTextColor(effPct)}">${fmtPct(effPct, 1)}</span>
            </div>
            <div style="display:flex;justify-content:space-between;font-size:11.5px;color:var(--text-2);flex-wrap:wrap;gap:4px;" class="num">
              <span>次数 ${fmtInt(effRemReq)} / ${fmtInt(limReq)}（已用 ${fmtInt(usedReq)}）</span>
              ${focusTokenSpan}
            </div>
            ${cdLine}
          </div>
        `;
      }
    } else {
      const activeModels = [];
      const fullModels = [];
      for (const m of eligibleModels) {
        if (isAccountModelFullZero(m)) fullModels.push(m);
        else activeModels.push(m);
      }

      const isExp = state.aisExpandAllFull || state.expandedAisAccounts.has(acc.id);
      const visible = isExp ? [...activeModels, ...fullModels] : activeModels;

      const rows = visible.map(m => {
        const friendly = modelLabelMap.get(m.model_id) || m.model_id;
        const effPct = m.effective_percent || 0;
        const limReq = m.limit_requests || 0;
        const effRemReq = m.effective_remaining_requests || 0;
        const limTok = m.limit_tokens || 0;
        const usedTok = m.used_tokens || 0;
        const effRemTok = m.effective_remaining_tokens || 0;
        const isEmpty = quotaLevel(effPct) === 'empty';
        let rightCol = '';
        if (m.status === 'available') {
          rightCol = `
            <div class="ais-m-bar-cell">
              <div class="bar-track bar-track-sm ${isEmpty ? 'is-empty' : ''}">
                <div class="bar-fill ${barColorClass(effPct)}" style="width:${Math.max(0, Math.min(100, effPct))}%"></div>
              </div>
              <span class="ais-m-pct num" style="color:${pctTextColor(effPct)}">${fmtRoundPct(effPct)}</span>
            </div>
          `;
        } else if (m.status === 'cooldown' && m.cooldown_until_ms) {
          rightCol = `<span class="badge badge-amber cd-target num" style="font-size:10px;padding:1px 6px;" data-ts="${m.cooldown_until_ms}" data-compact="1">${fmtCompactCountdown(m.cooldown_until_ms)}</span>`;
        } else {
          rightCol = `<span class="badge ${statusBadgeClass(m.status)}" style="font-size:10px;padding:1px 6px;">${esc(m.status_label)}</span>`;
        }

        const hasTokenLimit = limTok > 0;
        const rowTitle = hasTokenLimit
          ? `${friendly} (${m.model_id}): 剩余次数 ${effRemReq}/${limReq}, 剩余 Token ${fmtCompact(effRemTok)}/${fmtCompact(limTok)}`
          : `${friendly} (${m.model_id}): 剩余次数 ${effRemReq}/${limReq}（按次计额${usedTok > 0 ? `，已耗 ${fmtCompact(usedTok)} tok` : ''}）`;
        const rowNums = hasTokenLimit
          ? `${fmtInt(effRemReq)}次 · ${fmtCompact(effRemTok)}`
          : (usedTok > 0
              ? `${fmtInt(effRemReq)}次 · 已耗 ${fmtCompact(usedTok)} tok`
              : `${fmtInt(effRemReq)}/${fmtInt(limReq)}次`);

        return `
          <div class="ais-m-row" title="${esc(rowTitle)}">
            <div class="ais-m-name">${esc(friendly)}</div>
            <div class="ais-m-nums num">${rowNums}</div>
            <div style="text-align:right;">${rightCol}</div>
          </div>
        `;
      }).join('');

      const allFullNote = (!isExp && activeModels.length === 0 && fullModels.length > 0)
        ? `<div class="ais-all-full-note num"><span>全部 ${fullModels.length} 个适用模型</span><span>100% 满血就绪</span></div>`
        : '';

      const moreBtn = fullModels.length > 0 && !state.aisExpandAllFull
        ? `<button type="button" class="ais-expand-btn" data-ais-expand="${esc(acc.id)}">
            ${isExp ? `收起 ${fullModels.length} 个 100% 满血模型 ▲` : `展开其余 ${fullModels.length} 个 100% 满血模型 ▼`}
          </button>`
        : '';

      bodyHtml = `${allFullNote}${rows ? `<div class="ais-model-list">${rows}</div>` : ''}${moreBtn}`;
    }

    return `
      <article class="ais-acc-card">
        <div class="ais-acc-head">
          <div class="ais-acc-title">
            <span>${esc(acc.code_name)}</span>
            <span class="badge ${tierBadgeClass(acc.tier)}">${esc(acc.tier)}</span>
          </div>
          <span class="badge ${statusBadgeClass(acc.state)}">${esc(stateText)}</span>
        </div>
        <div class="ais-acc-meta num">
          <span>可用 <strong>${acc.available_models}/${acc.eligible_models}</strong></span>
          <span>本轮已用 <strong>${fmtInt(acc.used_requests)}</strong> 次 · ${fmtCompact(acc.used_tokens)}</span>
          ${acc.cooling_models > 0 ? `<span style="color:var(--amber-text);font-weight:600;">冷却 ${acc.cooling_models}</span>` : ''}
        </div>
        ${bodyHtml}
      </article>
    `;
  }

  // ---------- Main Render & Chart Hydration ----------
  function hydrateAllCharts() {
    const ag = state.snapshot?.antigravity;
    if (ag && ag.accounts) {
      const isSingle = ag.accounts.length === 1;
      for (const acc of ag.accounts) {
        if (!acc.has_history) continue;
        if (!isSingle && state.collapsedTrends.has(acc.id)) continue;
        renderAccountHistoryPanel(acc.id);
        const pref = getChartPref(acc.id);
        const key = historyCacheKey(acc.id, pref.window, currentRangeOf(pref));
        if (!state.historyCache.get(key)?.data) {
          fetchAccountHistory(acc.id, pref.window, currentRangeOf(pref), false);
        }
      }
    }
    renderAISHourlyChartSVG();
  }

  function redrawVisibleCharts() {
    const ag = state.snapshot?.antigravity;
    if (ag && ag.accounts) {
      const isSingle = ag.accounts.length === 1;
      for (const acc of ag.accounts) {
        if (!acc.has_history) continue;
        if (!isSingle && state.collapsedTrends.has(acc.id)) continue;
        renderAccountHistoryPanel(acc.id);
      }
    }
    renderAISHourlyChartSVG();
  }

  function render() {
    const snap = state.snapshot;
    if (!snap) return;
    renderTopBadges();
    const root = document.getElementById('app-root');
    if (!root) return;
    root.innerHTML = renderAntigravity(snap.antigravity) + renderAIStudio(snap.aistudio);
    bindDynamicEvents();
    hydrateAllCharts();
  }

  function bindDynamicEvents() {
    // CPAMP 20-bucket touch tooltip
    document.querySelectorAll('[data-cpamp-block]').forEach(el => {
      el.addEventListener('pointerdown', (e) => {
        if (e.pointerType === 'touch') {
          e.preventDefault();
          const wasActive = el.classList.contains('active');
          document.querySelectorAll('.cpamp-block-wrap.active').forEach(x => x.classList.remove('active'));
          if (!wasActive) el.classList.add('active');
        }
      });
    });

    // Multi-account expand collapsed burn chart
    document.querySelectorAll('.trend-toggle[data-trend-toggle]').forEach(btn => {
      btn.addEventListener('click', () => {
        const id = btn.getAttribute('data-trend-toggle');
        state.collapsedTrends.delete(id);
        saveCollapsedSet(state.collapsedTrends);
        render();
      });
    });

    // AIStudio 24h metric switcher
    document.querySelectorAll('[data-ais-metric]').forEach(btn => {
      btn.addEventListener('click', () => {
        const next = btn.getAttribute('data-ais-metric');
        if (state.aisHourlyMetric === next) return;
        state.aisHourlyMetric = next;
        safeStorageSet(STORAGE_AIS_METRIC, next);
        render();
      });
    });

    // AIStudio TTFT overlay toggle
    const ttftBtn = document.getElementById('btn-ais-ttft');
    if (ttftBtn) {
      ttftBtn.addEventListener('click', () => {
        state.aisShowTtft = !state.aisShowTtft;
        safeStorageSet(STORAGE_AIS_TTFT, state.aisShowTtft ? '1' : '0');
        render();
      });
    }

    // Matrix group filter pills
    document.querySelectorAll('[data-ais-group]').forEach(btn => {
      btn.addEventListener('click', () => {
        const next = btn.getAttribute('data-ais-group');
        if (state.aisModelGroup === next) return;
        state.aisModelGroup = next;
        render();
      });
    });

    // Matrix sort toggle
    document.querySelectorAll('[data-ais-msort]').forEach(btn => {
      btn.addEventListener('click', () => {
        const next = btn.getAttribute('data-ais-msort');
        if (state.aisModelSort === next) return;
        state.aisModelSort = next;
        render();
      });
    });

    // Clear focus banner inside matrix header
    const matrixClear = document.getElementById('matrix-clear-focus');
    if (matrixClear) {
      matrixClear.addEventListener('click', () => {
        state.selectedModel = '';
        render();
      });
    }

    // Click model row / mobile card to focus
    document.querySelectorAll('[data-select-model]').forEach(row => {
      row.addEventListener('click', () => {
        const mid = row.getAttribute('data-select-model');
        state.selectedModel = state.selectedModel === mid ? '' : mid;
        if (state.viewMode === 'summary' && state.selectedModel) {
          state.viewMode = 'full';
          safeStorageSet(STORAGE_VIEW, 'full');
          syncViewButtons();
        }
        render();
        if (needsFullAccountsFetch()) {
          fetchSnapshot(false, false, { forceView: 'full', bypassIfNoneMatch: true });
        }
      });
    });

    // Account detail controls
    const modelSel = document.getElementById('ais-model-select');
    if (modelSel) {
      modelSel.addEventListener('change', () => {
        state.selectedModel = modelSel.value;
        render();
      });
    }
    const clearBtn = document.getElementById('ais-clear-model');
    if (clearBtn) {
      clearBtn.addEventListener('click', () => {
        state.selectedModel = '';
        render();
      });
    }

    document.querySelectorAll('[data-ais-asort]').forEach(btn => {
      btn.addEventListener('click', () => {
        const next = btn.getAttribute('data-ais-asort');
        if (state.aisAccountSort === next) return;
        state.aisAccountSort = next;
        render();
      });
    });

    document.querySelectorAll('[data-ais-tier]').forEach(btn => {
      btn.addEventListener('click', () => {
        state.selectedTier = btn.getAttribute('data-ais-tier');
        render();
      });
    });

    const expandAllBtn = document.getElementById('btn-ais-expand-all');
    if (expandAllBtn) {
      expandAllBtn.addEventListener('click', () => {
        state.aisExpandAllFull = !state.aisExpandAllFull;
        render();
      });
    }

    document.querySelectorAll('[data-ais-expand]').forEach(btn => {
      btn.addEventListener('click', () => {
        const id = btn.getAttribute('data-ais-expand');
        if (state.expandedAisAccounts.has(id)) state.expandedAisAccounts.delete(id);
        else state.expandedAisAccounts.add(id);
        render();
      });
    });
  }

  // ---------- Static UI Bindings ----------
  function syncViewButtons() {
    const fullBtn = document.getElementById('view-full');
    const sumBtn = document.getElementById('view-summary');
    if (fullBtn) fullBtn.classList.toggle('active', state.viewMode === 'full');
    if (sumBtn) sumBtn.classList.toggle('active', state.viewMode === 'summary');
  }

  function bindStaticEvents() {
    const themeBtn = document.getElementById('btn-theme');
    if (themeBtn) themeBtn.addEventListener('click', () => cycleThemeWithTransition(themeBtn));

    const gateThemeBtn = document.getElementById('btn-theme-gate');
    if (gateThemeBtn) gateThemeBtn.addEventListener('click', () => cycleThemeWithTransition(gateThemeBtn));

    const fullBtn = document.getElementById('view-full');
    if (fullBtn) {
      fullBtn.addEventListener('click', () => {
        if (state.viewMode === 'full') return;
        state.viewMode = 'full';
        safeStorageSet(STORAGE_VIEW, 'full');
        syncViewButtons();
        render();
        if (needsFullAccountsFetch()) {
          fetchSnapshot(false, false, { forceView: 'full', bypassIfNoneMatch: true });
        }
      });
    }

    const sumBtn = document.getElementById('view-summary');
    if (sumBtn) {
      sumBtn.addEventListener('click', () => {
        if (state.viewMode === 'summary') return;
        state.viewMode = 'summary';
        safeStorageSet(STORAGE_VIEW, 'summary');
        syncViewButtons();
        render();
      });
    }

    const refreshBtn = document.getElementById('btn-refresh');
    if (refreshBtn) {
      refreshBtn.addEventListener('click', () => {
        fetchSnapshot(true, true);
      });
    }

    // Passcode eye visibility toggle
    const eyeBtn = document.getElementById('btn-toggle-eye');
    const passInput = document.getElementById('passcode-input');
    if (eyeBtn && passInput) {
      eyeBtn.addEventListener('click', () => {
        const showing = passInput.type === 'text';
        passInput.type = showing ? 'password' : 'text';
        const showIcon = eyeBtn.querySelector('.eye-show');
        const hideIcon = eyeBtn.querySelector('.eye-hide');
        if (showIcon) showIcon.hidden = !showing;
        if (hideIcon) hideIcon.hidden = showing;
        eyeBtn.setAttribute('aria-label', showing ? '显示口令' : '隐藏口令');
        passInput.focus();
      });
      passInput.addEventListener('input', () => {
        const wrap = document.getElementById('gate-input-wrap');
        const err = document.getElementById('passcode-err');
        if (wrap) wrap.classList.remove('has-error');
        if (err && err.textContent) err.textContent = '';
      });
    }

    // Passcode submit
    const form = document.getElementById('passcode-form');
    if (form) {
      form.addEventListener('submit', async (e) => {
        e.preventDefault();
        const input = document.getElementById('passcode-input');
        const val = (input?.value || '').trim();
        if (!val) {
          triggerGateShake('请输入访问口令');
          if (input) input.focus();
          return;
        }
        state.passcode = val;
        setGateLoading(true);
        const res = await fetchSnapshot(true);
        setGateLoading(false);
        if (res.ok) {
          safeStorageSet(STORAGE_PASS, val);
          if (input) input.value = '';
          showShell();
        } else if (res.status === 401) {
          state.passcode = '';
          triggerGateShake('口令不正确，请重新输入');
          if (input) { input.focus(); input.select(); }
        } else {
          triggerGateShake('连接状态服务失败，请稍后重试');
        }
      });
    }

    // Global click-outside handler for CPAMP tooltips & mobile chart scrubbing
    document.addEventListener('pointerdown', (e) => {
      if (!e.target.closest('.cpamp-block-wrap')) {
        document.querySelectorAll('.cpamp-block-wrap.active').forEach(el => el.classList.remove('active'));
      }
      if (!e.target.closest('.lamp-panel') && !e.target.closest('.ais-trend-card')) {
        scrubClearers.forEach(fn => { try { fn(); } catch {} });
      }
    });

    // Debounced resize redraw for crisp non-distorted SVGs
    let resizeTimer = null;
    window.addEventListener('resize', () => {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        if (state.snapshot && !state.gateOpen) redrawVisibleCharts();
      }, 140);
    });

    // Zero-traffic background pause + instant foreground catch-up
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState !== 'visible') return;
      if (state.gateOpen) return;
      updateLiveCountdowns();
      const intervalMs = pollIntervalSeconds() * 1000;
      const elapsedMs = state.lastSyncWallMs > 0 ? (Date.now() - state.lastSyncWallMs) : Infinity;
      if (!state.snapshot || elapsedMs >= intervalMs) {
        state.countdown = pollIntervalSeconds();
        updateSyncMeta();
        fetchSnapshot(false, false);
      } else {
        state.countdown = Math.max(1, Math.ceil((intervalMs - elapsedMs) / 1000));
        updateSyncMeta();
      }
    });
  }

  function updateLiveCountdowns() {
    document.querySelectorAll('.cd-target').forEach(el => {
      const ts = Number(el.getAttribute('data-ts'));
      if (!(ts > 0)) return;
      const prefix = el.getAttribute('data-prefix') ?? '';
      if (el.getAttribute('data-compact') === '1') {
        el.textContent = prefix + fmtCompactCountdown(ts);
      } else {
        const suffix = el.getAttribute('data-suffix') ?? '';
        el.textContent = prefix + fmtCountdown(ts, suffix);
      }
    });
    document.querySelectorAll('.age-target').forEach(el => {
      const ts = Number(el.getAttribute('data-obs-ts'));
      if (ts > 0) el.textContent = fmtSampleAge(ts);
    });
    document.querySelectorAll('.cd-hms').forEach(el => {
      const ts = Number(el.getAttribute('data-ts'));
      if (ts > 0) el.textContent = fmtHMS(ts);
    });
  }

  // ---------- 1s Live Countdown Tick & Meta-Synced Silent Polling ----------
  setInterval(() => {
    if (document.visibilityState === 'hidden') return;
    if (state.snapshot && !state.gateOpen) {
      state.countdown--;
      if (state.countdown <= 0) {
        state.countdown = pollIntervalSeconds();
        fetchSnapshot(false, false);
      }
      updateSyncMeta();
    }
    if (!state.gateOpen) {
      updateLiveCountdowns();
    }
  }, 1000);

  // ---------- Startup Bootstrap (Meta-first auth flow) ----------
  async function bootstrap() {
    applyThemePref(state.themePref, false);
    syncViewButtons();
    bindStaticEvents();

    try {
      const metaResp = await fetch('/api/meta', { cache: 'no-store' });
      if (metaResp.ok) {
        state.meta = await metaResp.json();
        state.countdown = pollIntervalSeconds();
      }
    } catch {}

    if (state.meta.passcode_required && !state.passcode) {
      showGate('');
      return;
    }

    showShell();
    renderSkeleton();
    await fetchSnapshot(false, false);
  }

  bootstrap();
})();
