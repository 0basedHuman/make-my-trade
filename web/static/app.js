// app.js — MakeMyTrade pipeline UI

// ── State ─────────────────────────────────────────────────────────────────────
const rowMap   = {};          // ticker → { row, cells, tickerCell }
const prevStage = {};         // ticker → stage-array string (for diffing)
let currentData = null;
let selectedTicker = null;

// ── Init ──────────────────────────────────────────────────────────────────────
document.addEventListener('DOMContentLoaded', () => {
  updateClock();
  setInterval(updateClock, 30000);
  load();
});

document.addEventListener('keydown', e => {
  if (e.target.tagName === 'INPUT') return;
  if (e.code === 'Space')      { e.preventDefault(); runAnalysis(); }
  else if (e.key  === 'c')     { runConfirmation(); }
  else if (e.key  === 'r')     { load(); }
});

// ── Clock / market status ─────────────────────────────────────────────────────
function updateClock() {
  const pt  = new Date(new Date().toLocaleString('en-US', { timeZone: 'America/Los_Angeles' }));
  const day = pt.getDay();
  const min = pt.getHours() * 60 + pt.getMinutes();

  document.getElementById('datePill').textContent =
    pt.toLocaleDateString('en-US', { weekday:'short', month:'short', day:'numeric' }) + ' PT';

  const dot = document.getElementById('mktDot');
  const lbl = document.getElementById('mktLabel');
  if (day === 0 || day === 6) {
    dot.className = 'mkt-dot closed'; lbl.textContent = 'Weekend';
  } else if (min >= 390 && min < 960) {
    dot.className = 'mkt-dot open';   lbl.textContent = 'Market Open';
  } else if (min >= 360 && min < 390) {
    dot.className = 'mkt-dot pre';    lbl.textContent = 'Pre-Market';
  } else {
    dot.className = 'mkt-dot closed'; lbl.textContent = 'Closed';
  }
}

// ── API calls ─────────────────────────────────────────────────────────────────
async function load() {
  setStatus('Loading…');
  try {
    const pt  = new Date(new Date().toLocaleString('en-US', { timeZone: 'America/Los_Angeles' }));
    const ymd = pt.toLocaleDateString('en-US', { year:'numeric', month:'2-digit', day:'2-digit' })
                  .split('/').map((v,i)=>i===2?v:v.padStart(2,'0')).reverse().join('-')
                  .replace(/(\d{4})-(\d{2})-(\d{2})/,'$1-$3-$2'); // fix mm/dd/yyyy → yyyy-mm-dd
    // Simpler date formatter
    const y = pt.getFullYear(), m = String(pt.getMonth()+1).padStart(2,'0'), d = String(pt.getDate()).padStart(2,'0');
    const res = await fetch(`/api/daily-analysis?date=${y}-${m}-${d}`);
    if (!res.ok) { setStatus('No data'); return; }
    const data = await res.json();
    currentData = data;
    applyData(data);
    const total = allCandidates(data).length;
    setStatus(total > 0 ? `${total} tickers · ${data.symbols_scanned||0} scanned` : 'No analysis yet');
    log(`Loaded daily analysis — ${total} tickers`, 'info');
  } catch(e) { setStatus('Error'); log(`Load failed: ${e.message}`, 'error'); }
}

async function runAnalysis() {
  const btn = document.getElementById('btnRun');
  if (btn.disabled) return;
  btn.disabled = true;
  btn.innerHTML = '<div class="spinner"></div> Running…';
  setStrip('running');
  setStatus('Running analysis…');
  log('POST /api/run-analysis', 'info');
  try {
    const res  = await fetch('/api/run-analysis', { method: 'POST' });
    const data = await res.json();
    if (!res.ok) { log(`Analysis error: ${data.error || res.statusText}`, 'error'); return; }
    currentData = data;
    applyData(data);
    const confirmed = (data.confirmed||[]).length;
    setStrip(confirmed > 0 ? 'confirmed' : 'idle');
    log(`Analysis done — confirmed:${confirmed} entry_ready:${(data.entry_ready||[]).length} structural:${(data.structural_candidates||[]).length}`, 'success');
    setStatus(`Analysis complete · ${confirmed} confirmed`);
  } catch(e) { log(`Analysis failed: ${e.message}`, 'error'); setStrip('idle'); }
  finally { btn.disabled = false; btn.innerHTML = '▶ Run Analysis'; }
}

async function runConfirmation() {
  const btn = document.getElementById('btnConfirm');
  if (btn.disabled) return;
  btn.disabled = true;
  btn.innerHTML = '<div class="spinner"></div> Confirming…';
  setStrip('running');
  setStatus('Running confirmation…');
  log('POST /api/run-confirmation', 'info');
  try {
    const res  = await fetch('/api/run-confirmation', { method: 'POST' });
    const data = await res.json();
    if (!res.ok) { log(`Confirmation error: ${data.error || res.statusText}`, 'error'); return; }
    currentData = data;
    applyData(data);
    const confirmed = (data.confirmed||[]).length;
    setStrip(confirmed > 0 ? 'confirmed' : 'idle');
    log(`Confirmation done — confirmed:${confirmed} watch_only:${(data.watch_only||[]).length}`, 'success');
    if (confirmed > 0) {
      (data.confirmed||[]).forEach(c => log(`  ● Paper position created: ${c.ticker} ${c.direction||''}`, 'success'));
    }
    setStatus(`Confirmation complete · ${confirmed} confirmed`);
  } catch(e) { log(`Confirmation failed: ${e.message}`, 'error'); setStrip('idle'); }
  finally { btn.disabled = false; btn.innerHTML = '◉ Run Confirmation'; }
}

// ── Apply response → update grid ──────────────────────────────────────────────
function applyData(data) {
  const positions = data.open_positions || [];
  const posSet    = new Set(positions.map(p => p.ticker));

  // Flatten all tickers in display order
  const groups = [
    { list: data.confirmed             || [], statusKey: 'confirmed' },
    { list: data.entry_ready           || [], statusKey: 'entry_ready' },
    { list: data.structural_candidates || [], statusKey: 'structural_candidate' },
    { list: data.blocked_by_event      || [], statusKey: 'blocked_by_event' },
    { list: data.watch_only            || [], statusKey: 'watch_only' },
    { list: data.rejected              || [], statusKey: 'rejected' },
  ];

  const seen = new Set();

  groups.forEach(({ list, statusKey }) => {
    list.forEach(c => {
      if (seen.has(c.ticker)) return;
      seen.add(c.ticker);
      const status  = c.decision_status || statusKey;
      const hasPos  = posSet.has(c.ticker);
      const stages  = computeStages(status, hasPos);
      updateRow(c.ticker, stages, status === 'rejected');
    });
  });

  // Remove rows for tickers no longer in data
  Object.keys(rowMap).forEach(t => {
    if (!seen.has(t)) {
      rowMap[t].row.remove();
      delete rowMap[t];
      delete prevStage[t];
    }
  });

  // Empty state
  const rowsEl = document.getElementById('pipeline-rows');
  if (seen.size === 0) {
    if (!rowsEl.querySelector('.pipe-empty-state')) {
      rowsEl.innerHTML = '<div class="pipe-empty-state">No tickers yet.<br>Press <b>Space</b> to run analysis.</div>';
    }
  } else {
    const es = rowsEl.querySelector('.pipe-empty-state');
    if (es) es.remove();
  }

  // Refresh detail panel if a ticker is selected
  if (selectedTicker) {
    const c = findCandidate(data, selectedTicker);
    if (c) showDetail(c, data);
  }
}

// ── Pipeline row management ───────────────────────────────────────────────────
function computeStages(status, hasPosition) {
  // Returns array of 5 class names: [STRUCT, READY, CONFIRM, EXECUTE, MANAGE]
  const E = 'c-empty', R = 'c-reached';
  const base = {
    rejected:             [E,       E,         E,         E,         E],
    structural_candidate: ['c-struct', E,       E,         E,         E],
    blocked_by_event:     ['c-blocked', E,      E,         E,         E],
    watch_only:           ['c-watch',  E,       E,         E,         E],
    entry_ready:          [R,        'c-ready', E,         E,         E],
    confirmed:            [R,        R,         'c-confirm', E,       E],
  }[status] || [E, E, E, E, E];

  const s = [...base];
  if (hasPosition) {
    s[3] = 'c-execute';
    if (s[2] === E) s[2] = R;
  }
  return s;
}

function ensureRow(ticker) {
  if (rowMap[ticker]) return rowMap[ticker];

  const row = document.createElement('div');
  row.className = 'pipe-row';
  row.dataset.ticker = ticker;

  const tc = document.createElement('div');
  tc.className = 'ticker-cell';
  tc.textContent = ticker;
  row.appendChild(tc);

  const cells = [];
  for (let i = 0; i < 5; i++) {
    const cell = document.createElement('div');
    cell.className = 'pipe-cell c-empty';
    row.appendChild(cell);
    cells.push(cell);
  }

  row.addEventListener('click', () => {
    if (currentData) {
      const c = findCandidate(currentData, ticker);
      if (c) {
        selectedTicker = ticker;
        document.querySelectorAll('.pipe-row.selected').forEach(r => r.classList.remove('selected'));
        row.classList.add('selected');
        showDetail(c, currentData);
      }
    }
  });

  document.getElementById('pipeline-rows').appendChild(row);
  rowMap[ticker] = { row, cells, tickerCell: tc };
  return rowMap[ticker];
}

function updateRow(ticker, stages, isDim) {
  const key = stages.join(',');
  const { cells, tickerCell } = ensureRow(ticker);

  // Only update DOM if stage changed
  if (prevStage[ticker] !== key) {
    stages.forEach((cls, i) => {
      const next = 'pipe-cell ' + cls;
      if (cells[i].className !== next) cells[i].className = next;
    });
    prevStage[ticker] = key;
  }

  tickerCell.className = isDim ? 'ticker-cell dim' : 'ticker-cell';
}

// ── Detail panel ──────────────────────────────────────────────────────────────
function showDetail(c, data) {
  const el     = document.getElementById('detail-content');
  const status = c.decision_status || 'structural_candidate';
  const score  = c.score || 0;
  const scoreColor = score >= 75 ? 'var(--green)' : score >= 55 ? 'var(--yellow)' : 'var(--muted)';
  const dir    = c.direction || 'none';

  // Gate chips
  const gates = [
    ['Trend',    c.gates?.trend?.passed],
    ['Momentum', c.gates?.momentum?.passed],
    ['Volume',   c.gates?.volume?.passed],
    ['VIX',      c.gates?.vix?.passed],
    ['BTC',      c.gates?.btc?.passed],
    ['RSI',      c.gates?.rsi?.passed],
  ].map(([l,p]) => `<span class="gate-chip ${p?'pass':'fail'}">${l}</span>`).join('');

  // Score bar (only entry_ready / confirmed)
  const showScore = c.score_visible && score > 0;
  const scoreHTML = showScore ? `
    <div class="score-wrap">
      <div class="score-track"><div class="score-fill" style="width:${score}%;background:${scoreColor};"></div></div>
      <div class="score-num" style="color:${scoreColor}">${score}</div>
    </div>` : '';

  // Notice (what's missing / blocked)
  let noticeHTML = '';
  if (c.what_is_missing) {
    const cls = status === 'blocked_by_event' ? 'block' : 'warn';
    noticeHTML = `<div class="d-notice ${cls}">${c.what_is_missing}</div>`;
  }

  // Levels grid
  const levelsHTML = (c.entry_low || c.entry_high || c.stop_loss) ? `
    <div class="d-section-title">Levels</div>
    <div class="d-grid">
      <div class="d-kv"><div class="d-kv-label">Entry Low</div><div class="d-kv-val yellow">$${fmt(c.entry_low)}</div></div>
      <div class="d-kv"><div class="d-kv-label">Entry High</div><div class="d-kv-val yellow">$${fmt(c.entry_high)}</div></div>
      <div class="d-kv"><div class="d-kv-label">Stop Loss</div><div class="d-kv-val red">$${fmt(c.stop_loss)}</div></div>
      <div class="d-kv"><div class="d-kv-label">Target 1</div><div class="d-kv-val green">$${fmt(c.base_target||c.target1)}</div></div>
      <div class="d-kv"><div class="d-kv-label">Target 2</div><div class="d-kv-val purple">$${fmt(c.stretch_target||c.target2)}</div></div>
      <div class="d-kv"><div class="d-kv-label">R/R Ratio</div><div class="d-kv-val blue">${(c.rr_ratio||0).toFixed(1)}×</div></div>
    </div>` : '';

  // Contract block — confirmed only
  const isConfirmed = status === 'confirmed';
  const contractHTML = (isConfirmed && c.contract_type && c.contract_type !== 'none') ? `
    <div class="d-section-title">Option Contract</div>
    <div class="d-box">
      <div class="d-box-row"><span class="d-box-label">Type</span><span class="d-box-val" style="color:${c.contract_type==='call'?'var(--green)':'var(--red)'}">${c.contract_type.toUpperCase()}</span></div>
      <div class="d-box-row"><span class="d-box-label">DTE</span><span class="d-box-val" style="color:var(--blue)">${c.contract_dte != null ? c.contract_dte + ' DTE' : '—'}</span></div>
      <div class="d-box-row"><span class="d-box-label">Delta range</span><span class="d-box-val">${c.contract_delta_range || '—'}</span></div>
      <div class="d-box-row"><span class="d-box-label">Contracts available</span><span class="d-box-val">${c.option_contracts_available || 0}</span></div>
      ${c.entry_trigger_price ? `<div class="d-box-row"><span class="d-box-label">Entry trigger</span><span class="d-box-val" style="color:var(--yellow)">$${fmt(c.entry_trigger_price)}</span></div>` : ''}
    </div>` : '';

  // Open position for this ticker (if any)
  const pos = (data.open_positions||[]).find(p => p.ticker === c.ticker);
  const posHTML = pos ? (() => {
    const entry   = pos.option_premium || pos.entry_price || 0;
    const current = pos.last_option_price || 0;
    const peak    = pos.peak_option_price || 0;
    const pnlPct  = entry > 0 && current > 0 ? ((current - entry) / entry * 100) : null;
    const stopLevel  = entry > 0 ? entry * 0.70 : 0;  // -30%
    const tpLevel    = entry > 0 ? entry * 1.50 : 0;  // +50%
    const trailFloor = peak  > 0 ? peak  * 0.80 : 0;  // -20% from peak

    const pnlColor = pnlPct === null ? 'var(--muted)' : pnlPct >= 0 ? 'var(--green)' : 'var(--red)';
    const pnlStr   = pnlPct !== null ? `${pnlPct >= 0 ? '+' : ''}${pnlPct.toFixed(1)}%` : '—';

    // Next mechanical exit trigger
    let nextTrigger = '—';
    if (entry > 0 && current > 0) {
      if (current <= stopLevel) nextTrigger = `stop hit ($${fmt(stopLevel)})`;
      else if (current >= tpLevel) nextTrigger = `take-profit hit ($${fmt(tpLevel)})`;
      else if (pos.trailing_active && trailFloor > 0) nextTrigger = `trail floor $${fmt(trailFloor)}`;
      else if (pos.trailing_active) nextTrigger = `trailing active`;
      else nextTrigger = `stop $${fmt(stopLevel)} · TP $${fmt(tpLevel)}`;
    }

    return `
    <div class="d-section-title">Open Position</div>
    <div class="d-box">
      <div class="d-box-row"><span class="d-box-label">Entry premium</span><span class="d-box-val">$${fmt(entry)}</span></div>
      <div class="d-box-row"><span class="d-box-label">Current premium</span><span class="d-box-val" style="color:${pnlColor}">${current > 0 ? `$${fmt(current)}` : '—'}</span></div>
      <div class="d-box-row"><span class="d-box-label">P/L</span><span class="d-box-val" style="color:${pnlColor}">${pnlStr}</span></div>
      ${peak > 0 ? `<div class="d-box-row"><span class="d-box-label">Peak premium</span><span class="d-box-val">$${fmt(peak)}</span></div>` : ''}
      <div class="d-box-row"><span class="d-box-label">Stop loss level</span><span class="d-box-val" style="color:var(--red)">${entry > 0 ? `$${fmt(stopLevel)}` : '—'}</span></div>
      <div class="d-box-row"><span class="d-box-label">Take profit level</span><span class="d-box-val" style="color:var(--green)">${entry > 0 ? `$${fmt(tpLevel)}` : '—'}</span></div>
      <div class="d-box-row"><span class="d-box-label">Trailing stop</span><span class="d-box-val">${pos.trailing_active ? `<span style="color:var(--yellow)">ACTIVE — floor $${trailFloor > 0 ? fmt(trailFloor) : '?'}</span>` : 'not yet'}</span></div>
      <div class="d-box-row"><span class="d-box-label">Next trigger</span><span class="d-box-val" style="font-size:11px">${nextTrigger}</span></div>
      <div class="d-box-row"><span class="d-box-label">Hold overnight</span><span class="d-box-val">${pos.hold_overnight_approved ? '<span style="color:var(--green)">approved</span>' : '<span style="color:var(--muted)">not approved</span>'}</span></div>
    </div>`;
  })() : '';

  el.innerHTML = `
    <div>
      <div class="d-ticker">${c.ticker}</div>
      <div class="d-row">
        <span class="d-badge ${status}">${status.replace(/_/g,' ')}</span>
        ${dir !== 'none' ? `<span class="d-badge ${dir}">${dir.toUpperCase()}</span>` : ''}
        ${c.setup_family ? `<span style="font-size:11px;color:var(--muted);">${c.setup_family.replace(/_/g,' ')}</span>` : ''}
      </div>

      <div style="display:flex;align-items:baseline;gap:10px;margin:8px 0 4px;">
        <span style="font-size:20px;font-weight:700;color:var(--muted)">$${fmt(c.close_price)}</span>
        ${c.rsi14 ? `<span style="font-size:11px;color:var(--muted)">RSI ${c.rsi14.toFixed(1)}</span>` : ''}
        ${c.volume_ratio ? `<span style="font-size:11px;color:var(--muted)">Vol ${c.volume_ratio.toFixed(1)}×</span>` : ''}
      </div>

      ${scoreHTML}
      ${noticeHTML}
      <hr class="d-divider">

      <div class="d-section-title">Gates</div>
      <div class="gates-row" style="margin-bottom:14px">${gates}</div>

      ${levelsHTML}
      ${contractHTML}
      ${posHTML}

      ${c.thesis_summary && isConfirmed ? `<hr class="d-divider"><div class="d-section-title">Thesis</div><div class="d-thesis">${c.thesis_summary}</div>` : ''}
    </div>`;
}

// ── Helpers ───────────────────────────────────────────────────────────────────
function allCandidates(data) {
  return [
    ...(data.confirmed||[]),
    ...(data.entry_ready||[]),
    ...(data.structural_candidates||[]),
    ...(data.blocked_by_event||[]),
    ...(data.watch_only||[]),
    ...(data.rejected||[]),
  ];
}

function findCandidate(data, ticker) {
  return allCandidates(data).find(c => c.ticker === ticker) || null;
}

function fmt(n) {
  if (n == null || n === 0) return '—';
  return Number(n).toFixed(2);
}

function setStatus(msg) {
  document.getElementById('statusMsg').textContent = msg;
}

function setStrip(state) {
  const el = document.getElementById('strip');
  el.className = '';              // reset
  el.offsetWidth;                 // force reflow so re-adding same class re-triggers animation
  if (state !== 'idle') el.classList.add(state);
}

// ── Terminal ──────────────────────────────────────────────────────────────────
const MAX_LOG = 50;
function log(msg, type = '') {
  const term = document.getElementById('terminal');
  const now  = new Date();
  const ts   = now.toLocaleTimeString('en-US', { timeZone: 'America/Los_Angeles', hour12: false });

  const line = document.createElement('div');
  line.className = `t-line${type ? ' '+type : ''}`;
  line.innerHTML = `<span class="ts">${ts}</span>${escHtml(msg)}`;
  term.appendChild(line);

  // Trim to max
  while (term.children.length > MAX_LOG) term.removeChild(term.firstChild);
  term.scrollTop = term.scrollHeight;
}

function escHtml(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}
