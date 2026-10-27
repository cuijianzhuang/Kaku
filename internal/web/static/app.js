// Small progressive enhancements; every page works without JavaScript.
document.addEventListener('click', (e) => {
  const btn = e.target.closest('[data-copy]');
  if (!btn) return;
  e.preventDefault();
  const text = btn.getAttribute('data-copy');
  const done = () => { const t = btn.textContent; btn.textContent = '已复制'; setTimeout(() => (btn.textContent = t), 1200); };
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
      if (d.status && d.status !== start) location.reload();
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
    if (sku && qty) qty.max = sku.getAttribute('data-limit');
  };
  form.addEventListener('change', update);
  update();
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
