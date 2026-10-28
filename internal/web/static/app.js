// Small progressive enhancements; every page works without JavaScript.
document.addEventListener('click', (e) => {
  const btn = e.target.closest('[data-copy]');
  if (!btn) return;
  e.preventDefault();
  const text = btn.getAttribute('data-copy');
  const done = () => { const t = btn.innerHTML; btn.textContent = '已复制'; setTimeout(() => (btn.innerHTML = t), 1200); };
  if (navigator.clipboard) navigator.clipboard.writeText(text).then(done, () => {});
});

document.addEventListener('submit', (e) => {
  const msg = e.target.getAttribute('data-confirm');
  if (msg && !confirm(msg)) e.preventDefault();
});

// Poll an order's status and reload when it changes (pay and order pages).
(function () {
  const el = document.querySelector('[data-poll]');
  if (!el) return;
  const url = el.getAttribute('data-poll');
  const start = el.getAttribute('data-status');
  let tries = 0;
  const tick = () => {
    if (++tries > 400) return;
    fetch(url, { cache: 'no-store' }).then((r) => r.json()).then((d) => {
      if (d.status && d.status !== start) {
        const to = el.getAttribute('data-reload');
        if (to) location.href = to; else location.reload();
      }
      else setTimeout(tick, 3000);
    }).catch(() => setTimeout(tick, 5000));
  };
  setTimeout(tick, 3000);
})();

// Countdown for unpaid orders.
(function () {
  const el = document.querySelector('[data-countdown]');
  if (!el) return;
  let left = parseInt(el.getAttribute('data-countdown'), 10);
  const show = () => {
    if (left <= 0) { el.textContent = '已超时'; return; }
    el.textContent = Math.floor(left / 60) + ' 分 ' + String(left % 60).padStart(2, '0') + ' 秒';
    left--; setTimeout(show, 1000);
  };
  show();
})();

// Product page: enable only the payment methods the chosen SKU is priced in.
(function () {
  const form = document.querySelector('form[data-order]');
  if (!form) return;
  const update = () => {
    const sku = form.querySelector('input[name=sku_id]:checked');
    const allowed = sku ? (sku.getAttribute('data-methods') || '').split(',') : [];
    form.querySelectorAll('input[name=method]').forEach((m) => {
      const ok = allowed.includes(m.value);
      m.disabled = !ok;
      m.closest('.option').classList.toggle('disabled', !ok);
      if (!ok && m.checked) m.checked = false;
    });
    if (!form.querySelector('input[name=method]:checked')) {
      const first = form.querySelector('input[name=method]:not(:disabled)');
      if (first) first.checked = true;
    }
    const qty = form.querySelector('input[name=qty]');
    if (sku && qty) {
      qty.max = sku.getAttribute('data-limit');
      if (+qty.value > +qty.max) qty.value = qty.max;
    }
    total();
  };
  // Reference total: unit price × quantity in the chosen method's currency.
  // Wholesale tiers and coupons are applied by the server.
  const places = { CNY: 2, USDT: 6, XTR: 0 };
  const fmt = (v, cur) => {
    const n = v / Math.pow(10, places[cur]);
    let s = n.toFixed(places[cur]);
    if (s.includes('.')) s = s.replace(/\.?0+$/, '');
    return cur === 'CNY' ? '¥' + s : cur === 'XTR' ? s + ' ⭐' : s + ' USDT';
  };
  const total = () => {
    const out = form.querySelector('[data-total]');
    if (!out) return;
    const sku = form.querySelector('input[name=sku_id]:checked');
    const m = form.querySelector('input[name=method]:checked');
    const qty = Math.max(1, parseInt(form.querySelector('input[name=qty]')?.value || '1', 10));
    if (!sku || !m) { out.textContent = '—'; return; }
    const cur = m.getAttribute('data-currency') || 'CNY';
    const unit = parseInt(sku.getAttribute('data-' + cur.toLowerCase()) || '0', 10);
    out.textContent = unit > 0 ? fmt(unit * qty, cur) : '—';
  };
  form.addEventListener('change', update);
  form.addEventListener('input', total);
  form.querySelectorAll('[data-step]').forEach((b) => b.addEventListener('click', () => {
    const q = form.querySelector('input[name=qty]');
    const v = (parseInt(q.value, 10) || 1) + parseInt(b.getAttribute('data-step'), 10);
    q.value = Math.min(Math.max(v, 1), parseInt(q.max || '99', 10));
    total();
  }));
  update();
})();

// Storefront search: filter product cards by title, hide empty sections.
(function () {
  const input = document.querySelector('input[data-filter]');
  if (!input) return;
  const cards = document.querySelectorAll(input.getAttribute('data-filter'));
  const empty = document.querySelector('[data-filter-empty]');
  input.addEventListener('input', () => {
    const q = input.value.trim().toLowerCase();
    let shown = 0;
    cards.forEach((c) => {
      const hit = !q || (c.getAttribute('data-title') || '').toLowerCase().includes(q);
      c.hidden = !hit;
      if (hit) shown++;
    });
    document.querySelectorAll('[data-section]').forEach((s) => {
      s.hidden = !s.querySelector('.product-card:not([hidden])');
    });
    if (empty) empty.hidden = shown > 0;
  });
})();

// SKU form: show an example configuration for the chosen delivery method.
(function () {
  const sel = document.querySelector('select[name=deliverer_kind]');
  const cfg = document.querySelector('textarea[name=deliverer_cfg]');
  const hint = document.querySelector('[data-cfg-hint]');
  if (!sel || !cfg || !hint) return;
  const examples = {
    stock: '无需配置（留空）。在「卡密」页导入库存；Polaris 预生成的激活码也可以这样导入。',
    text: '{"text": "下载地址：https://…\\n提取码：…", "per_unit": false}',
    manual: '无需配置（留空）。付款后在订单页手动填写发货内容。',
    webhook: '{"url": "https://api.example.com/kaku/deliver", "secret": "至少16位的签名密钥", "revoke_url": ""}',
    polaris: '{"plan": "pro", "days": 365, "updates_days": 365, "limits": {"nodes": 20, "users": 0, "plugins": 0}, "features": ["*"], "offline": false, "max_activations": 1, "renewable": true}',
  };
  const show = () => { hint.textContent = examples[sel.value] || ''; };
  sel.addEventListener('change', show);
  show();
  document.querySelector('[data-cfg-fill]')?.addEventListener('click', (e) => {
    e.preventDefault();
    const ex = examples[sel.value] || '';
    cfg.value = ex.startsWith('{') ? JSON.stringify(JSON.parse(ex), null, 2) : '';
  });
})();

// Phones show list tables as cards: copy each column's header onto its
// cells (CSS shows it as a label). Tables whose first row is not a header
// row, such as key-value tables, are left alone.
(function () {
  document.querySelectorAll('.table-wrap table, table.list').forEach((t) => {
    const head = t.rows[0];
    if (!head || t.rows.length < 2 || [...head.cells].some((c) => c.tagName !== 'TH')) return;
    const labels = [...head.cells].map((c) => c.textContent.trim());
    for (const row of [...t.rows].slice(1)) {
      if ([...row.cells].some((c) => c.tagName === 'TH')) return;
      [...row.cells].forEach((c, i) => {
        if (c.colSpan > 1) return;
        c.setAttribute('data-label', labels[i] || '');
        if (i > 0 && !c.textContent.trim() && !c.querySelector('img,input,button,svg')) c.classList.add('blank');
        // keep the cell's content together as one value next to its label
        const v = document.createElement('span');
        v.className = 'cv';
        while (c.firstChild) v.appendChild(c.firstChild);
        c.appendChild(v);
      });
    }
    t.classList.add('rt');
  });
})();

// Close the admin menu after picking a page (back/forward cache keeps it open otherwise).
window.addEventListener('pageshow', () => { const t = document.getElementById('nav-toggle'); if (t) t.checked = false; });

// Selects that apply a filter as soon as they change.
document.addEventListener('change', (e) => {
  if (e.target.matches('select[data-autosubmit]') && e.target.form) e.target.form.submit();
});

// SKU form: the Polaris section replaces the JSON box for Polaris SKUs.
(function () {
  const kind = document.querySelector('select[name=deliverer_kind]');
  const pc = document.querySelector('[data-cfg-polaris]');
  const pk = document.querySelector('[data-cfg-panel]');
  const generic = document.querySelector('[data-cfg-generic]');
  if (!kind || !pc || !generic) return;
  const mode = () => (pc.querySelector('input[name=pc_mode]:checked') || {}).value || 'custom';
  const sync = () => {
    const isPolaris = kind.value === 'polaris';
    pc.hidden = !isPolaris;
    pc.querySelectorAll('input,select').forEach((el) => { el.disabled = !isPolaris; });
    const isPanel = kind.value === 'panelcode';
    if (pk) {
      pk.hidden = !isPanel;
      pk.querySelectorAll('input,select').forEach((el) => { el.disabled = !isPanel; });
    }
    generic.hidden = isPanel || (isPolaris && mode() !== 'json');
    const ex = generic.querySelector('[data-cfg-example]');
    if (ex) ex.hidden = isPolaris || isPanel;
    pc.querySelectorAll('[data-pc-pane]').forEach((p) => {
      p.hidden = !p.getAttribute('data-pc-pane').split(' ').includes(mode());
    });
    const all = pc.querySelector('[data-pc-all]');
    pc.querySelectorAll('[data-pc-features] input').forEach((f) => { f.disabled = !isPolaris || (all && all.checked); });
  };
  kind.addEventListener('change', sync);
  pc.addEventListener('change', sync);
  pc.querySelectorAll('[data-set-days]').forEach((b) => b.addEventListener('click', () => {
    const v = b.getAttribute('data-set-days');
    pc.querySelector('[data-term=days]').value = v;
    pc.querySelector('[data-term=updates]').value = v;
  }));
  sync();
})();

// Settings: generate a random API secret and show it (it is needed on the
// license server too); copy a block of text.
document.addEventListener('click', (e) => {
  const gen = e.target.closest('[data-gen-secret]');
  if (gen) {
    const input = gen.parentElement.querySelector('[data-secret]');
    const bytes = new Uint8Array(32);
    crypto.getRandomValues(bytes);
    const v = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
    input.type = 'text';
    input.value = v;
    document.querySelectorAll('[data-secret-echo]').forEach((el) => { el.textContent = v; });
    return;
  }
  const cp = e.target.closest('[data-copy-from]');
  if (cp) {
    const src = document.querySelector(cp.getAttribute('data-copy-from'));
    if (src && navigator.clipboard) navigator.clipboard.writeText(src.textContent).then(() => {
      const t = cp.innerHTML; cp.textContent = '已复制'; setTimeout(() => (cp.innerHTML = t), 1200);
    });
  }
});
