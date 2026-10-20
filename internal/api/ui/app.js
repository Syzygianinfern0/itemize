// itemize review UI. Plain ES module, no build step. All text is inserted
// with textContent (never innerHTML), since order data comes from retailers.
//
// Nothing here writes to Monarch except "Apply", which sends an explicit list
// of approved proposal IDs after a confirmation dialog listing every write.

const app = document.getElementById('app');
const modalRoot = document.getElementById('modal-root');
const toastEl = document.getElementById('toast');

// ---------- helpers ----------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key === 'class') el.className = value;
    else if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
    else if (key === 'checked' || key === 'disabled' || key === 'selected' || key === 'value') el[key] = value;
    else el.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children.flat(Infinity)) {
    if (child === undefined || child === null || child === false) continue;
    el.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return el;
}

async function api(path, options = {}) {
  const init = { method: options.method || 'GET', headers: { Accept: 'application/json' } };
  if (init.method !== 'GET') {
    init.headers['X-Itemize-Request'] = '1';
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(options.body ?? {});
  }
  const res = await fetch(path, init);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { message: text }; }
  if (!res.ok) {
    const err = new Error((data && (data.message || data.error)) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    throw err;
  }
  return data;
}

const money = (n) => {
  if (n === undefined || n === null || Number.isNaN(n)) return '';
  const sign = n < 0 ? '-' : '';
  return `${sign}$${Math.abs(n).toFixed(2)}`;
};

function toast(message) {
  toastEl.textContent = message;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { toastEl.textContent = ''; }, 4000);
}

function badge(status) {
  const cls = {
    success: 'ok', applied: 'ok', completed: 'ok', approved: 'info', proposed: 'info', pending: 'warn',
    'dry-run': '', provisional: 'warn', rejected: '', failed: 'bad', stale: 'bad', running: 'info', cancelled: '',
  }[status] ?? '';
  return h('span', { class: `badge ${cls}` }, status || '—');
}

function errorPanel(err) {
  return h('div', { class: 'notice bad' }, err.message || String(err));
}

let pollTimer = null;
function stopPolling() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = null;
}

function closeModal() { modalRoot.replaceChildren(); }

function openModal(content) {
  const backdrop = h('div', { class: 'modal-backdrop', onclick: (e) => { if (e.target === backdrop) closeModal(); } },
    h('div', { class: 'modal', role: 'dialog', 'aria-modal': 'true' }, content));
  modalRoot.replaceChildren(backdrop);
}

async function refreshCounts() {
  try {
    const o = await api('/api/review/overview');
    const n = (o.proposals.pending || 0) + (o.proposals.approved || 0);
    document.getElementById('count-review').textContent = n ? String(n) : '';
  } catch { /* the badge is best-effort */ }
}

function proposalSummary(p) {
  if (p.kind === 'splits') {
    return (p.splits || []).map((s) => `${s.category_name || s.category_id} ${money(s.amount)}`).join(' · ');
  }
  return `→ ${p.category_name || p.category_id}`;
}

// ---------- proportional split preview (mirrors splitter.AllocateSplits) ----------

function planFromItems(items, amount, notePrefix) {
  const groups = [];
  const byId = new Map();
  for (const item of items) {
    let g = byId.get(item.category_id);
    if (!g) {
      g = { category_id: item.category_id, category_name: item.category_name, items: [], subtotal: 0 };
      byId.set(item.category_id, g);
      groups.push(g);
    }
    g.items.push(item);
    g.subtotal += item.price;
  }
  const noteFor = (g) => {
    const lines = g.items.map((i) => (i.quantity > 1
      ? `- ${i.name} (x${Math.trunc(i.quantity)}) $${i.price.toFixed(2)}`
      : `- ${i.name} $${i.price.toFixed(2)}`));
    const head = g.items.length > 3 ? `${g.category_name}: (${g.items.length} items)` : `${g.category_name}:`;
    return `${notePrefix || ''}${head}\n${lines.join('\n')}`;
  };
  if (groups.length > 1) {
    const total = groups.reduce((s, g) => s + g.subtotal, 0);
    const totalCents = Math.round(Math.abs(amount) * 100);
    const sign = amount > 0 ? 1 : -1;
    const exact = groups.map((g) => (totalCents * g.subtotal) / total);
    const cents = exact.map(Math.floor);
    let allocated = cents.reduce((a, b) => a + b, 0);
    const order = groups.map((_, i) => i).sort((a, b) => (exact[b] - cents[b]) - (exact[a] - cents[a]));
    for (let k = 0; allocated < totalCents; k++, allocated++) cents[order[k % order.length]]++;
    const splits = groups.map((g, i) => ({ category_id: g.category_id, category_name: g.category_name, amount: (sign * cents[i]) / 100, notes: noteFor(g) }))
      .filter((s) => s.amount !== 0);
    if (splits.length > 1) return { kind: 'splits', splits };
    if (splits.length === 1) return { kind: 'category', category_id: splits[0].category_id, category_name: splits[0].category_name, notes: splits[0].notes };
  }
  const all = { category_name: items[0].category_name, items };
  const lines = items.map((i) => (i.quantity > 1 ? `- ${i.name} (x${Math.round(i.quantity)}) $${i.price.toFixed(2)}` : `- ${i.name} $${i.price.toFixed(2)}`));
  return { kind: 'category', category_id: items[0].category_id, category_name: all.category_name, notes: `${notePrefix || ''}${all.category_name}:\n${lines.join('\n')}` };
}

// ---------- routing ----------

const routes = [
  [/^#\/review$/, renderReview, 'review'],
  [/^#\/orders$/, renderOrders, 'orders'],
  [/^#\/orders\/(.+)$/, (m) => renderOrderDetail(decodeURIComponent(m[1])), 'orders'],
  [/^#\/runs$/, renderRuns, 'runs'],
  [/^#\/applied$/, renderApplied, 'applied'],
];

async function route() {
  stopPolling();
  closeModal();
  const hash = location.hash || '#/review';
  for (const [pattern, render, tab] of routes) {
    const m = hash.match(pattern);
    if (m) {
      for (const a of document.querySelectorAll('.tabs a')) a.classList.toggle('active', a.dataset.tab === tab);
      app.replaceChildren(h('p', { class: 'muted' }, 'Loading…'));
      try {
        await render(m);
      } catch (err) {
        app.replaceChildren(errorPanel(err));
      }
      app.focus();
      refreshCounts();
      return;
    }
  }
  location.hash = '#/review';
}

window.addEventListener('hashchange', route);
route();

// ---------- review queue ----------

async function renderReview() {
  const [pending, approved, stale, failed, overview] = await Promise.all([
    api('/api/proposals?status=pending'),
    api('/api/proposals?status=approved'),
    api('/api/proposals?status=stale'),
    api('/api/proposals?status=failed'),
    api('/api/review/overview'),
  ]);

  const head = h('div', { class: 'page-head' },
    h('div', {},
      h('h1', {}, 'Review queue'),
      h('div', { class: 'muted' }, 'Approve the changes you want. Nothing is written to Monarch until you apply approved changes and confirm.')),
    h('div', { class: 'muted small' },
      `${overview.proposals.applied || 0} applied · ${overview.proposals.rejected || 0} rejected`));

  const sections = [head];
  sections.push(proposalTable('Pending review', pending.proposals, [
    { label: 'Approve selected', cls: 'ok', action: (ids) => decide(ids, 'approve') },
    { label: 'Reject selected', cls: 'danger', action: (ids) => decide(ids, 'reject') },
  ], 'No proposals are waiting. Start a propose run from Runs.'));

  sections.push(proposalTable('Approved, not yet applied', approved.proposals, [
    { label: 'Back to pending', action: (ids) => decide(ids, 'reset') },
    { label: 'Apply selected…', cls: 'primary', action: (ids) => confirmApply(ids), disabled: !overview.apply_available },
  ], 'Nothing approved yet.', overview.apply_available ? null : 'Applying is unavailable: the server has no Monarch client.'));

  const problems = [...stale.proposals, ...failed.proposals];
  if (problems.length) {
    sections.push(proposalTable('Stale or failed', problems, [
      { label: 'Back to pending', action: (ids) => decide(ids, 'reset') },
      { label: 'Reject', cls: 'danger', action: (ids) => decide(ids, 'reject') },
    ], '', 'Stale means the transaction changed in Monarch after the proposal was made; re-run propose to refresh it.'));
  }
  app.replaceChildren(...sections);
}

function proposalTable(title, proposals, actions, emptyText, note) {
  const selected = new Set();
  const buttons = actions.map((a) => h('button', {
    class: a.cls,
    disabled: true,
    onclick: async () => {
      if (!selected.size) return;
      try { await a.action([...selected]); } catch (err) { toast(err.message); }
    },
  }, a.label));
  const sync = () => {
    buttons.forEach((b, i) => { b.disabled = !selected.size || actions[i].disabled; });
    countEl.textContent = selected.size ? `${selected.size} selected` : '';
  };
  const countEl = h('span', { class: 'muted small' });

  const boxes = [];
  const all = h('input', {
    type: 'checkbox', 'aria-label': 'Select all',
    onchange: () => {
      boxes.forEach(([box, id]) => { box.checked = all.checked; if (all.checked) selected.add(id); else selected.delete(id); });
      sync();
    },
  });
  const rows = proposals.map((p) => {
    const box = h('input', {
      type: 'checkbox', 'aria-label': `Select proposal ${p.id}`,
      onclick: (e) => e.stopPropagation(),
      onchange: () => { if (box.checked) selected.add(p.id); else selected.delete(p.id); sync(); },
    });
    boxes.push([box, p.id]);
    return h('tr', { class: 'clickable', onclick: () => { location.hash = `#/orders/${encodeURIComponent(p.order_id)}`; } },
      h('td', {}, box),
      h('td', {}, p.order_date),
      h('td', { class: 'mono' }, p.order_id),
      h('td', { class: 'num' }, money(p.amount)),
      h('td', {}, p.kind === 'splits' ? h('span', { class: 'badge info' }, `${p.splits.length} splits`) : h('span', { class: 'badge' }, 'category')),
      h('td', { class: 'wrap' }, proposalSummary(p)),
      h('td', { class: 'num' }, p.confidence ? `${Math.round(p.confidence * 100)}%` : ''),
      h('td', { class: 'small muted wrap' }, p.error || p.reason || ''));
  });

  return h('section', { class: 'panel' },
    h('div', { class: 'toolbar' }, h('h2', {}, `${title} (${proposals.length})`), h('span', { class: 'spacer' }), countEl, buttons),
    note ? h('div', { class: 'notice warn small' }, note) : null,
    proposals.length
      ? h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {}, h('th', {}, all), h('th', {}, 'Date'), h('th', {}, 'Order'), h('th', { class: 'num' }, 'Amount'),
          h('th', {}, 'Change'), h('th', {}, 'Details'), h('th', { class: 'num' }, 'Confidence'), h('th', {}, 'Reason'))),
        h('tbody', {}, rows)))
      : h('p', { class: 'muted' }, emptyText));
}

async function decide(ids, action) {
  const res = await api('/api/proposals/decision', { method: 'POST', body: { ids, action } });
  toast(`${res.changed} of ${res.requested} marked ${res.status}`);
  route();
}

async function confirmApply(ids) {
  const plan = await api('/api/apply/plan', { method: 'POST', body: { ids } });
  const ack = h('input', { type: 'checkbox', id: 'ack' });
  const go = h('button', { class: 'primary', disabled: true }, `Write ${plan.count} change${plan.count === 1 ? '' : 's'} to Monarch`);
  ack.addEventListener('change', () => { go.disabled = !ack.checked; });

  const list = plan.writes.map((w) => h('div', { class: 'proposal' },
    h('div', { class: 'proposal-head' },
      h('strong', {}, `#${w.proposal_id}`),
      h('span', { class: 'mono' }, `order ${w.order_id}`),
      h('span', { class: 'mono' }, `transaction ${w.transaction_id}`),
      h('span', { class: 'spacer' }),
      h('span', {}, `${money(w.amount)} (amount unchanged)`)),
    w.kind === 'splits'
      ? h('table', {}, h('tbody', {}, w.splits.map((s) => h('tr', {},
        h('td', {}, 'split'), h('td', {}, s.category_name || s.category_id), h('td', { class: 'num' }, money(s.amount))))))
      : h('div', {}, h('div', {}, `Set category → ${w.category_name || w.category_id}`), h('div', { class: 'notes' }, `Notes: ${w.notes}`)),
    w.mark_reviewed ? h('div', { class: 'small muted' }, 'Also marks the transaction reviewed.') : null));

  go.addEventListener('click', async () => {
    go.disabled = true;
    go.textContent = 'Applying…';
    try {
      const job = await api('/api/apply', { method: 'POST', body: { ids } });
      await watchApply(job.id);
    } catch (err) {
      toast(err.message);
      go.textContent = 'Apply failed';
    }
  });

  openModal([
    h('h2', {}, 'Confirm writes to Monarch'),
    h('p', {}, 'Each transaction is re-read first; if it changed since the proposal was made, it is skipped and marked stale. Amounts are never changed and nothing is deleted.'),
    list,
    h('div', { class: 'modal-actions' },
      h('label', {}, ack, 'I reviewed these changes'),
      h('button', { onclick: closeModal }, 'Cancel'),
      go),
  ]);
}

async function watchApply(jobId) {
  const status = h('p', {}, 'Applying…');
  openModal([h('h2', {}, 'Applying approved changes'), status]);
  for (;;) {
    const job = await api(`/api/apply/${encodeURIComponent(jobId)}`);
    if (job.status !== 'running') {
      const r = job.result;
      const lines = [];
      if (job.error) lines.push(h('div', { class: 'notice bad' }, job.error));
      if (r) {
        lines.push(h('p', {}, `Applied ${r.applied} · stale ${r.stale} · failed ${r.failed} · skipped ${r.skipped}`));
        const problems = r.outcomes.filter((o) => o.status !== 'applied');
        if (problems.length) {
          lines.push(h('table', {}, h('tbody', {}, problems.map((o) => h('tr', {},
            h('td', {}, `#${o.proposal_id}`), h('td', {}, badge(o.status)), h('td', { class: 'small wrap' }, o.error || ''))))));
        }
      }
      openModal([h('h2', {}, 'Apply finished'), lines, h('div', { class: 'modal-actions' }, h('button', { class: 'primary', onclick: () => { closeModal(); route(); } }, 'Done'))]);
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
}

// ---------- orders ----------

const orderFilters = { status: '', type: '', from: '', to: '', search: '' };

async function renderOrders() {
  const data = await api('/api/review/orders');
  const statuses = [...new Set(data.orders.map((o) => o.status))].sort();

  const tableHost = h('div');
  const draw = () => {
    const f = orderFilters;
    const rows = data.orders.filter((o) => (!f.status || o.status === f.status)
      && (!f.type || (f.type === 'multi-charge' ? o.charge_count > 1 : o.order_type === f.type))
      && (!f.from || o.order_date >= f.from)
      && (!f.to || o.order_date <= f.to)
      && (!f.search || o.order_id.includes(f.search) || (o.reason || '').toLowerCase().includes(f.search.toLowerCase())));
    tableHost.replaceChildren(
      h('p', { class: 'muted small' }, `${rows.length} of ${data.orders.length} orders`),
      h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {}, ['Date', 'Order', 'Type', 'Total', 'Charges', 'Status', 'Proposals', 'Reason'].map((t, i) => h('th', { class: i === 3 || i === 4 ? 'num' : '' }, t)))),
        h('tbody', {}, rows.map((o) => h('tr', { class: 'clickable', onclick: () => { location.hash = `#/orders/${encodeURIComponent(o.order_id)}`; } },
          h('td', {}, o.order_date),
          h('td', { class: 'mono' }, o.order_id),
          h('td', {}, o.order_type),
          h('td', { class: 'num' }, money(o.order_total)),
          h('td', { class: 'num' }, o.charge_count || ''),
          h('td', {}, badge(o.status), o.dry_run && o.status !== 'proposed' && o.status !== 'dry-run' ? h('span', { class: 'small muted' }, ' (preview)') : null),
          h('td', {}, Object.entries(o.proposals || {}).map(([s, n]) => h('span', { class: 'small' }, `${n} ${s} `))),
          h('td', { class: 'small muted wrap' }, unmatchedReason(o))))))));
  };

  const select = (key, options) => h('select', { onchange: (e) => { orderFilters[key] = e.target.value; draw(); } },
    options.map(([value, label]) => h('option', { value, selected: orderFilters[key] === value }, label)));
  const input = (key, type, placeholder) => h('input', { type, placeholder, value: orderFilters[key], oninput: (e) => { orderFilters[key] = e.target.value; draw(); } });

  app.replaceChildren(
    h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, 'Orders'), h('div', { class: 'muted' }, 'Every order the last runs looked at, with why unmatched ones did not match.'))),
    h('section', { class: 'panel' },
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', {}, 'Status'), select('status', [['', 'All'], ...statuses.map((s) => [s, s])])),
        h('label', { class: 'field' }, h('span', {}, 'Type'), select('type', [['', 'All'], ['in-store', 'In-store'], ['online', 'Online'], ['multi-charge', 'Multi-charge'], ['return', 'Return']])),
        h('label', { class: 'field' }, h('span', {}, 'From'), input('from', 'date')),
        h('label', { class: 'field' }, h('span', {}, 'To'), input('to', 'date')),
        h('label', { class: 'field' }, h('span', {}, 'Search'), input('search', 'search', 'order ID or reason'))),
      tableHost));
  draw();
}

function unmatchedReason(o) {
  if (!o.reason) return '';
  if (o.reason === 'no matching transaction found') return `no Monarch transaction for ${money(o.order_total)}`;
  const m = o.reason.match(/could not find all transactions: expected (\d+), found (\d+)/);
  if (m) return `${m[1]} charges, ${m[2]} found in Monarch`;
  return o.reason;
}

// ---------- order detail ----------

async function renderOrderDetail(orderId) {
  const [data, cats] = await Promise.all([
    api(`/api/review/orders/${encodeURIComponent(orderId)}`),
    api('/api/categories').catch(() => ({ categories: [] })),
  ]);
  const o = data.order;

  const header = h('div', { class: 'page-head' },
    h('div', {},
      h('a', { href: '#/orders' }, '← Orders'),
      h('h1', {}, `Order ${o.order_id}`),
      h('div', { class: 'muted' }, `${o.order_date} · ${o.order_type} · ${money(o.order_total)} · `, badge(o.status))),
    o.reason ? h('div', { class: 'notice warn' }, unmatchedReason({ reason: o.reason, order_total: o.order_total })) : null);

  const items = h('section', { class: 'panel' }, h('h2', {}, 'Walmart items'),
    (o.items || []).length
      ? h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {}, h('th', {}, 'Item'), h('th', { class: 'num' }, 'Qty'), h('th', { class: 'num' }, 'Price'))),
        h('tbody', {}, o.items.map((i) => h('tr', {}, h('td', {}, i.name), h('td', { class: 'num' }, i.quantity), h('td', { class: 'num' }, money(i.total_price))))),
        h('tfoot', {},
          h('tr', {}, h('td', { class: 'muted' }, 'Subtotal'), h('td', {}), h('td', { class: 'num' }, money(o.order_subtotal))),
          h('tr', {}, h('td', { class: 'muted' }, 'Tax'), h('td', {}), h('td', { class: 'num' }, money(o.order_tax))),
          o.order_tip ? h('tr', {}, h('td', { class: 'muted' }, 'Tip'), h('td', {}), h('td', { class: 'num' }, money(o.order_tip))) : null)))
      : h('p', { class: 'muted' }, 'No items recorded.'));

  const charges = (data.ledger && data.ledger.charges) || [];
  const ledger = h('section', { class: 'panel' }, h('h2', {}, 'Ledger charges'),
    charges.length
      ? h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {}, h('th', {}, '#'), h('th', {}, 'Type'), h('th', {}, 'Payment'), h('th', {}, 'Charged'), h('th', { class: 'num' }, 'Amount'))),
        h('tbody', {}, charges.map((c) => h('tr', {},
          h('td', {}, c.charge_sequence), h('td', {}, c.charge_type), h('td', {}, [c.payment_method, c.card_type].filter(Boolean).join(' ')),
          h('td', {}, (c.charged_at || '').slice(0, 10)), h('td', { class: 'num' }, money(c.charge_amount)))))))
      : h('p', { class: 'muted' }, 'No ledger charges (in-store receipts use the receipt total, or the ledger was not needed).'));

  const proposals = h('section', { class: 'panel' }, h('h2', {}, `Proposed changes (${data.proposals.length})`),
    data.proposals.length ? data.proposals.map((p) => proposalCard(p, cats.categories)) : h('p', { class: 'muted' }, 'No proposals for this order. Run a propose run to create them.'));

  const audit = h('section', { class: 'panel' }, h('h2', {}, 'Monarch write log'),
    data.api_calls.length
      ? h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {}, h('th', {}, 'Time'), h('th', {}, 'Method'), h('th', {}, 'Phase'), h('th', {}, 'Transaction'), h('th', {}, 'Result'))),
        h('tbody', {}, data.api_calls.map((c) => h('tr', {},
          h('td', { class: 'small' }, c.timestamp), h('td', {}, c.method), h('td', {}, c.phase), h('td', { class: 'mono' }, c.transaction_id),
          h('td', { class: 'small wrap' }, c.dry_run ? 'dry run' : (c.error || 'ok')))))))
      : h('p', { class: 'muted' }, 'Nothing has been written to Monarch for this order.'));

  app.replaceChildren(header, h('div', { class: 'grid-2' }, items, ledger), proposals, audit);
}

function proposalCard(p, categories) {
  const before = (() => { try { return typeof p.before === 'string' ? JSON.parse(p.before) : p.before; } catch { return {}; } })() || {};
  const editable = p.status === 'pending' && (p.items || []).length > 0;
  const working = (p.items || []).map((i) => ({ ...i }));
  const afterHost = h('div');
  const saveBtn = h('button', { class: 'primary hidden' }, 'Save category changes');
  const resetBtn = h('button', { class: 'hidden' }, 'Undo changes');

  const drawAfter = () => {
    const dirty = working.some((w, i) => w.category_id !== p.items[i].category_id);
    saveBtn.classList.toggle('hidden', !dirty);
    resetBtn.classList.toggle('hidden', !dirty);
    const plan = dirty ? planFromItems(working, p.amount, p.note_prefix) : p;
    afterHost.replaceChildren(
      h('h3', {}, dirty ? 'After (preview, not saved)' : 'After'),
      plan.kind === 'splits'
        ? h('table', {}, h('tbody', {}, plan.splits.map((s) => h('tr', {},
          h('td', {}, s.category_name || s.category_id), h('td', { class: 'num' }, money(s.amount))))),
        h('tfoot', {}, h('tr', {}, h('td', { class: 'muted' }, 'Total (unchanged)'), h('td', { class: 'num' }, money(p.amount)))))
        : h('div', {}, h('div', {}, `Category: ${plan.category_name || plan.category_id}`), h('div', { class: 'small muted' }, `Amount ${money(p.amount)} (unchanged)`)),
      plan.kind === 'splits'
        ? h('details', {}, h('summary', { class: 'small' }, 'Split notes'), plan.splits.map((s) => h('div', { class: 'notes' }, s.notes)))
        : h('div', { class: 'notes' }, plan.notes));
  };

  const options = categories.map((c) => [c.id, c.group ? `${c.group} › ${c.name}` : c.name]);
  const itemRows = working.map((item, idx) => {
    const cell = editable && options.length
      ? h('select', {
        'aria-label': `Category for ${item.name}`,
        onchange: (e) => {
          const cat = categories.find((c) => c.id === e.target.value);
          working[idx].category_id = cat.id;
          working[idx].category_name = cat.name;
          row.classList.toggle('changed', working[idx].category_id !== p.items[idx].category_id);
          drawAfter();
        },
      }, options.some(([id]) => id === item.category_id) ? null : h('option', { value: item.category_id, selected: true }, item.category_name),
      options.map(([id, label]) => h('option', { value: id, selected: id === item.category_id }, label)))
      : item.category_name;
    const row = h('tr', {},
      h('td', {}, item.name),
      h('td', { class: 'num' }, money(item.price)),
      h('td', {}, cell),
      h('td', { class: 'num small muted' }, item.confidence ? `${Math.round(item.confidence * 100)}%` : ''));
    return row;
  });

  saveBtn.addEventListener('click', async () => {
    try {
      await api(`/api/proposals/${p.id}/items`, { method: 'PUT', body: { categories: working.map((w) => w.category_id) } });
      toast('Saved');
      route();
    } catch (err) { toast(err.message); }
  });
  resetBtn.addEventListener('click', () => route());

  const actions = [];
  if (p.status === 'pending') {
    actions.push(h('button', { class: 'ok', onclick: () => decide([p.id], 'approve').catch((e) => toast(e.message)) }, 'Approve'));
    actions.push(h('button', { class: 'danger', onclick: () => decide([p.id], 'reject').catch((e) => toast(e.message)) }, 'Reject'));
  } else if (p.status === 'approved') {
    actions.push(h('button', { onclick: () => decide([p.id], 'reset').catch((e) => toast(e.message)) }, 'Back to pending'));
    actions.push(h('button', { class: 'primary', onclick: () => confirmApply([p.id]).catch((e) => toast(e.message)) }, 'Apply…'));
  } else if (p.status === 'rejected' || p.status === 'stale' || p.status === 'failed') {
    actions.push(h('button', { onclick: () => decide([p.id], 'reset').catch((e) => toast(e.message)) }, 'Back to pending'));
  }

  const card = h('div', { class: 'proposal' },
    h('div', { class: 'proposal-head' },
      h('strong', {}, `#${p.id}`), badge(p.status),
      h('span', { class: 'mono' }, `transaction ${p.transaction_id}`),
      h('span', {}, money(p.amount)),
      h('span', { class: 'spacer' }),
      actions),
    p.reason ? h('div', { class: 'small muted' }, p.reason) : null,
    p.error ? h('div', { class: 'notice bad small' }, p.error) : null,
    h('div', { class: 'before-after' },
      h('div', { class: 'box' },
        h('h3', {}, 'Before (in Monarch when proposed)'),
        h('dl', { class: 'kv small' },
          h('dt', {}, 'Date'), h('dd', {}, before.date || ''),
          h('dt', {}, 'Merchant'), h('dd', {}, before.merchant || before.plaid_name || ''),
          h('dt', {}, 'Account'), h('dd', {}, before.account || ''),
          h('dt', {}, 'Category'), h('dd', {}, before.category_name || '—'),
          h('dt', {}, 'Notes'), h('dd', { class: 'notes' }, before.notes || '—'))),
      h('div', { class: 'arrow', 'aria-hidden': 'true' }, '→'),
      h('div', { class: 'box' }, afterHost)),
    working.length
      ? h('details', { open: editable },
        h('summary', {}, editable ? 'Items and categories (edit to change the split)' : 'Items and categories'),
        h('div', { class: 'table-wrap' }, h('table', {},
          h('thead', {}, h('tr', {}, h('th', {}, 'Item'), h('th', { class: 'num' }, 'Price'), h('th', {}, 'Category'), h('th', { class: 'num' }, 'Confidence'))),
          h('tbody', {}, itemRows))),
        h('div', { class: 'toolbar' }, h('span', { class: 'spacer' }), resetBtn, saveBtn))
      : null);
  drawAfter();
  return card;
}

// ---------- runs ----------

async function renderRuns() {
  const startForm = (() => {
    const provider = h('select', {}, ['walmart', 'costco', 'amazon'].map((p) => h('option', { value: p }, p)));
    const propose = h('input', { type: 'radio', name: 'mode', value: 'propose', checked: true });
    const dry = h('input', { type: 'radio', name: 'mode', value: 'dry' });
    const days = h('input', { type: 'number', min: 1, max: 730, value: 30 });
    const max = h('input', { type: 'number', min: 0, value: 0 });
    const force = h('input', { type: 'checkbox' });
    const start = h('button', { class: 'primary' }, 'Start run');
    start.addEventListener('click', async () => {
      start.disabled = true;
      try {
        await api('/api/sync', {
          method: 'POST',
          body: {
            provider: provider.value, propose: propose.checked, dry_run: dry.checked,
            lookback_days: Number(days.value) || 30, max_orders: Number(max.value) || 0, force: force.checked,
          },
        });
        toast('Run started');
        route();
      } catch (err) {
        toast(err.status === 404 ? 'Runs are unavailable: the server could not initialize its clients.' : err.message);
        start.disabled = false;
      }
    });
    return h('section', { class: 'panel' },
      h('h2', {}, 'Start a run'),
      h('p', { class: 'muted small' }, 'Neither mode writes to Monarch. A propose run stores every change for review here; a dry run only logs what it would do. Walmart runs take about 30 s per order that needs a ledger lookup.'),
      h('div', { class: 'form-grid' },
        h('label', { class: 'field' }, h('span', {}, 'Provider'), provider),
        h('div', { class: 'field' }, h('span', {}, 'Mode'), h('div', { class: 'toolbar' }, h('label', {}, propose, 'Propose'), h('label', {}, dry, 'Dry run'))),
        h('label', { class: 'field' }, h('span', {}, 'Days back'), days),
        h('label', { class: 'field' }, h('span', {}, 'Max orders (0 = all)'), max),
        h('label', { class: 'field' }, h('span', {}, 'Reprocess'), h('label', {}, force, 'force')),
        start));
  })();

  const active = h('section', { class: 'panel' }, h('h2', {}, 'Active run'), h('p', { class: 'muted' }, 'Checking…'));
  const history = h('section', { class: 'panel' }, h('h2', {}, 'History'), h('p', { class: 'muted' }, 'Loading…'));
  app.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Runs')), startForm, active, history);

  const drawHistory = async () => {
    const data = await api('/api/runs?limit=50');
    history.replaceChildren(h('h2', {}, 'History'),
      data.runs.length
        ? h('div', { class: 'table-wrap' }, h('table', {},
          h('thead', {}, h('tr', {}, ['#', 'Started', 'Mode', 'Provider', 'Days', 'Found', 'Processed', 'Skipped', 'Errors', 'Status'].map((t) => h('th', {}, t)))),
          h('tbody', {}, data.runs.map((r) => h('tr', {},
            h('td', {}, r.id), h('td', { class: 'small' }, r.started_at), h('td', {}, r.mode || (r.dry_run ? 'dry-run' : 'live')),
            h('td', {}, r.provider), h('td', { class: 'num' }, r.lookback_days), h('td', { class: 'num' }, r.orders_found),
            h('td', { class: 'num' }, r.orders_processed), h('td', { class: 'num' }, r.orders_skipped), h('td', { class: 'num' }, r.orders_errored),
            h('td', {}, badge(r.status)))))))
        : h('p', { class: 'muted' }, 'No runs yet.'));
  };

  const drawActive = async () => {
    let data;
    try {
      data = await api('/api/sync/active');
    } catch (err) {
      active.replaceChildren(h('h2', {}, 'Active run'), h('p', { class: 'muted' }, err.status === 404 ? 'Runs are unavailable on this server.' : err.message));
      stopPolling();
      return;
    }
    if (!data.jobs.length) {
      active.replaceChildren(h('h2', {}, 'Active run'), h('p', { class: 'muted' }, 'No run in progress.'));
      if (drawActive.wasRunning) { drawActive.wasRunning = false; drawHistory(); refreshCounts(); }
      return;
    }
    drawActive.wasRunning = true;
    active.replaceChildren(h('h2', {}, 'Active run'), data.jobs.map((j) => {
      const p = j.progress;
      const bar = h('div');
      const pct = p.total_orders ? Math.round((100 * p.current_index) / p.total_orders) : 0;
      bar.style.width = `${pct}%`;
      return h('div', {},
        h('div', { class: 'toolbar' },
          h('strong', {}, `${j.provider} · ${j.propose ? 'propose' : 'dry run'}`), badge(j.status),
          h('span', { class: 'muted small' }, p.current_phase.replaceAll('_', ' ')),
          h('span', { class: 'spacer' }),
          h('button', { class: 'danger', onclick: async () => { try { await api(`/api/sync/${encodeURIComponent(j.job_id)}`, { method: 'DELETE' }); toast('Cancelled'); } catch (e) { toast(e.message); } } }, 'Cancel')),
        h('div', { class: 'progress' }, bar),
        h('div', { class: 'small' },
          p.total_orders ? `Order ${p.current_index} of ${p.total_orders}` : 'Fetching…',
          p.current_order_id ? h('span', { class: 'mono muted' }, ` (${p.current_order_id})`) : null,
          ` · processed ${p.processed_orders} · skipped ${p.skipped_orders} · errors ${p.errored_orders}`));
    }));
  };

  await Promise.all([drawHistory(), drawActive()]);
  pollTimer = setInterval(drawActive, 2000);
}

// ---------- applied log ----------

async function renderApplied() {
  const data = await api('/api/applied?limit=300');
  app.replaceChildren(
    h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, 'Applied log'), h('div', { class: 'muted' }, 'Every write itemize made to Monarch, newest first, with the state before it.'))),
    h('section', { class: 'panel' },
      data.writes.length
        ? h('div', { class: 'table-wrap' }, h('table', {},
          h('thead', {}, h('tr', {}, ['Time', 'Order', 'Transaction', 'Write', 'Result', 'Before / request'].map((t) => h('th', {}, t)))),
          h('tbody', {}, data.writes.map((w) => {
            let before = null;
            if (w.proposal) {
              try { before = typeof w.proposal.before === 'string' ? JSON.parse(w.proposal.before) : w.proposal.before; } catch { before = null; }
            }
            return h('tr', {},
              h('td', { class: 'small' }, w.timestamp),
              h('td', { class: 'mono' }, h('a', { href: `#/orders/${encodeURIComponent(w.order_id)}` }, w.order_id)),
              h('td', { class: 'mono' }, w.transaction_id),
              h('td', {}, w.method.replace('Transactions.', ''), w.proposal ? h('div', { class: 'small muted' }, proposalSummary(w.proposal)) : null),
              h('td', {}, w.error ? h('span', { class: 'badge bad' }, 'error') : h('span', { class: 'badge ok' }, 'ok'), w.error ? h('div', { class: 'small wrap' }, w.error) : null),
              h('td', {}, h('details', {}, h('summary', { class: 'small' }, 'show'),
                before ? h('div', { class: 'small' }, `Before: ${before.category_name || '—'}, ${money(before.amount)}, notes: ${before.notes || '—'}`) : null,
                h('pre', {}, JSON.stringify(w.request, null, 2)))));
          }))))
        : h('p', { class: 'muted' }, 'Nothing has been written to Monarch yet.')));
}
