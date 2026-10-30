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
    // manual SKUs may ask the buyer for something (充值账号) and explain delivery
    const askField = form.querySelector('[data-ask-field]');
    if (askField) {
      const ask = sku ? sku.getAttribute('data-ask') : '';
      askField.hidden = !ask;
      askField.querySelector('input').disabled = !ask;
      askField.querySelector('[data-ask-label]').textContent = ask;
      const noteField = form.querySelector('[data-note-field]');
      const note = sku ? sku.getAttribute('data-note') : '';
      noteField.hidden = !note;
      noteField.querySelector('[data-note-text]').textContent = note;
    }
    const qty = form.querySelector('input[name=qty]');
    if (sku && qty) {
      qty.max = sku.getAttribute('data-limit');
      if (+qty.value > +qty.max) qty.value = qty.max;
    }
    total();
  };
  // Reference total: unit price × quantity in the chosen method's currency,
  // with the wholesale tier the server will apply. Coupons are applied by the server.
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
    let pct = 100, best = 0;
    (sku.getAttribute('data-tiers') || '').split(',').forEach((t) => {
      const [q, p] = t.split(':').map(Number);
      if (q && p && q <= qty && q > best) { best = q; pct = p; }
    });
    out.textContent = unit > 0 ? fmt(Math.round(unit * qty * pct / 100), cur) + (pct < 100 ? '（已享批发价）' : '') : '—';
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

// Catalog search updates results and category anchors together. Clear restores
// every category and returns keyboard focus to the search field.
(function () {
  const input = document.querySelector('input[data-filter]');
  if (!input) return;
  const cards = [...document.querySelectorAll(input.getAttribute('data-filter'))];
  const empty = document.querySelector('[data-filter-empty]');
  const status = document.querySelector('[data-filter-status]');
  const clears = document.querySelectorAll('[data-filter-clear]');
  const update = () => {
    const q = input.value.trim().toLocaleLowerCase();
    let shown = 0;
    cards.forEach((card) => {
      card.hidden = !!q && !(card.getAttribute('data-title') || '').toLocaleLowerCase().includes(q);
      if (!card.hidden) shown++;
    });
    document.querySelectorAll('[data-section]').forEach((section) => {
      section.hidden = !section.querySelector('.product-card:not([hidden])');
      document.querySelectorAll('.chips a').forEach((link) => {
        if (link.hash === '#' + section.id) link.hidden = section.hidden;
      });
    });
    if (empty) empty.hidden = shown > 0;
    clears.forEach((button) => { button.hidden = !input.value; });
    if (status) {
      status.hidden = !q;
      status.textContent = q ? '找到 ' + shown + ' 件商品' : '';
    }
  };
  input.addEventListener('input', update);
  input.addEventListener('search', update);
  clears.forEach((button) => button.addEventListener('click', () => {
    input.value = '';
    update();
    input.focus();
  }));
  update(); // also restore consistency after a browser restores form values
  window.addEventListener('pageshow', update);
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

// Settings used to be one long page with #anchors; send old links to the tab.
(function () {
  const m = location.pathname.match(/\/settings\/?$/);
  const tab = location.hash.slice(1);
  if (m && /^(site|appearance|payment|mail|telegram|login|panel|polaris)$/.test(tab)) {
    location.replace(location.pathname.replace(/\/?$/, '/') + tab + location.search);
  }
  // phones: bring the current tab into view in the scrolling tab bar
  const on = document.querySelector('.tabs a.on');
  if (on && on.parentElement.scrollWidth > on.parentElement.clientWidth) {
    on.parentElement.scrollLeft = on.offsetLeft - (on.parentElement.clientWidth - on.offsetWidth) / 2;
  }
})();

// The mobile drawer has one focus boundary. Desktop navigation and the
// checkbox/label fallback keep working without depending on this controller.
(function () {
  const toggle = document.getElementById('nav-toggle');
  const sidebar = document.getElementById('admin-navigation');
  const opener = document.querySelector('[data-nav-open]');
  if (!toggle || !sidebar || !opener) return;
  const close = sidebar.querySelector('[data-nav-close]');
  const fallback = document.querySelector('label[for="nav-toggle"].mbar-btn');
  const content = document.querySelector('.shell > .content');
  const bar = document.querySelector('.mbar');
  const media = window.matchMedia('(max-width: 900px)');
  const focusable = () => [...sidebar.querySelectorAll('a[href], button:not([disabled]), input:not([disabled])')].filter((el) => el.getClientRects().length && !el.hidden);
  const sync = (restoreFocus = false) => {
    const open = media.matches && toggle.checked;
    opener.setAttribute('aria-expanded', String(open));
    document.body.classList.toggle('nav-open', open);
    sidebar.inert = media.matches && !open;
    if (media.matches && !open) sidebar.setAttribute('aria-hidden', 'true'); else sidebar.removeAttribute('aria-hidden');
    if (open) { sidebar.setAttribute('role', 'dialog'); sidebar.setAttribute('aria-modal', 'true'); }
    else { sidebar.removeAttribute('role'); sidebar.removeAttribute('aria-modal'); }
    content.inert = open;
    bar.inert = open;
    if (restoreFocus && media.matches) opener.focus();
  };
  const dismiss = (restoreFocus = true) => { toggle.checked = false; sync(restoreFocus); };
  opener.hidden = false;
  close.hidden = false;
  if (fallback) fallback.hidden = true;
  opener.addEventListener('click', () => {
    toggle.checked = true;
    sync();
    close.focus();
  });
  close.addEventListener('click', () => dismiss());
  toggle.addEventListener('change', () => sync(!toggle.checked));
  document.querySelector('.scrim').addEventListener('click', (e) => {
    e.preventDefault(); // do not let the label toggle the checkbox again
    dismiss();
  });
  sidebar.addEventListener('click', (e) => { if (e.target.closest('a[href]') && media.matches) dismiss(false); });
  document.addEventListener('keydown', (e) => {
    if (!media.matches || !toggle.checked) return;
    if (e.key === 'Escape') { e.preventDefault(); dismiss(); return; }
    if (e.key !== 'Tab') return;
    const items = focusable(), first = items[0], last = items[items.length - 1];
    if (e.shiftKey && (document.activeElement === first || !sidebar.contains(document.activeElement))) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && (document.activeElement === last || !sidebar.contains(document.activeElement))) { e.preventDefault(); first.focus(); }
  });
  media.addEventListener('change', () => dismiss(false));
  window.addEventListener('pageshow', () => dismiss(false));
  sync();
})();

// Selects that apply a filter as soon as they change.
document.addEventListener('change', (e) => {
  if (e.target.matches('select[data-autosubmit]') && e.target.form) e.target.form.submit();
});

// SKU form: each delivery method has its own section; the raw JSON box is
// only for Polaris "JSON" mode.
(function () {
  const kind = document.querySelector('select[name=deliverer_kind]');
  const generic = document.querySelector('[data-cfg-generic]');
  if (!kind || !generic) return;
  const pc = document.querySelector('[data-cfg-polaris]');
  const sections = {
    stock: document.querySelector('[data-cfg-stock]'),
    text: document.querySelector('[data-cfg-text]'),
    webhook: document.querySelector('[data-cfg-webhook]'),
    manual: document.querySelector('[data-cfg-manual]'),
    panelcode: document.querySelector('[data-cfg-panel]'),
    polaris: pc,
  };
  const mode = () => (pc && pc.querySelector('input[name=pc_mode]:checked') || {}).value || 'custom';
  const sync = () => {
    for (const [k, el] of Object.entries(sections)) {
      if (!el) continue;
      const on = kind.value === k;
      el.hidden = !on;
      el.querySelectorAll('input,select,textarea').forEach((f) => { f.disabled = !on; });
    }
    document.querySelectorAll('[data-stock-only]').forEach((el) => { el.hidden = kind.value !== 'stock'; });
    const isPolaris = kind.value === 'polaris';
    generic.hidden = isPolaris ? mode() !== 'json' : !!sections[kind.value];
    // panel SKUs keep their saved JSON when the panel cannot list its plans
    generic.querySelector('textarea').disabled = generic.hidden && kind.value !== 'panelcode';
    if (pc) {
      pc.querySelectorAll('[data-pc-pane]').forEach((p) => {
        p.hidden = !p.getAttribute('data-pc-pane').split(' ').includes(mode());
      });
      const all = pc.querySelector('[data-pc-all]');
      pc.querySelectorAll('[data-pc-features] input').forEach((f) => { f.disabled = !isPolaris || (all && all.checked); });
    }
    // fixed content: show the fields of the chosen format (网盘 / 链接 / 文章 / 纯文本)
    const tx = sections.text;
    if (tx) {
      const fmt = (tx.querySelector('input[name=tx_format]:checked') || {}).value || '';
      tx.querySelectorAll('[data-tx-pane]').forEach((p) => {
        p.hidden = !p.getAttribute('data-tx-pane').split(' ').includes(fmt);
      });
    }
  };
  kind.addEventListener('change', sync);
  if (sections.text) sections.text.addEventListener('change', sync);
  if (pc) {
    pc.addEventListener('change', sync);
    pc.querySelectorAll('[data-set-days]').forEach((b) => b.addEventListener('click', () => {
      const v = b.getAttribute('data-set-days');
      pc.querySelector('[data-term=days]').value = v;
      pc.querySelector('[data-term=updates]').value = v;
    }));
  }
  sync();
})();

// Product templates: suggest a name for the picked type until the owner types one.
(function () {
  const form = document.querySelector('form[data-quick]');
  if (!form) return;
  const title = form.querySelector('input[name=title]');
  const sku = form.querySelector('input[name=sku_name]');
  let last = {};
  const fill = () => {
    const t = form.querySelector('input[name=template]:checked');
    if (!t) return;
    for (const [el, key] of [[title, 'title'], [sku, 'sku']]) {
      if (!el.value || el.value === last[key]) el.value = t.getAttribute('data-' + key);
      last[key] = t.getAttribute('data-' + key);
    }
  };
  form.addEventListener('change', (e) => { if (e.target.name === 'template') fill(); });
  fill();
})();

// Product editor and settings: image pickers (cover, logo) and the
// description editor (toolbar, preview, pasting / dropping / picking images).
(function () {
  const cover = document.querySelector('[data-cover]');
  const md = document.querySelector('[data-md]');
  if (!cover && !md) return;
  const form = (cover || md).closest('form');
  const base = (form.getAttribute('action') || '').replace(/\/products$/, '');

  // image pickers: product cover, shop logo
  document.querySelectorAll('[data-cover]').forEach((cover) => {
    const img = cover.querySelector('[data-cover-img]');
    const ph = cover.querySelector('[data-cover-ph]');
    const file = cover.querySelector('[data-cover-file]');
    const url = cover.querySelector('[data-cover-url]');
    const remove = cover.querySelector('[data-cover-remove]');
    const help = cover.querySelector('.cover-help');
    const helpText = help ? help.textContent : '';
    const note = (msg, bad) => { if (help) { help.textContent = msg || helpText; help.classList.toggle('err-text', !!bad); } };
    let blob = '';
    let timer = 0;
    const show = (src) => {
      clearTimeout(timer);
      remove.hidden = !src;
      if (!src) { img.hidden = true; ph.hidden = false; img.removeAttribute('src'); note(); return; }
      note(src.startsWith('blob:') ? '' : '正在加载图片…');
      img.src = src;
    };
    // results for an older address are ignored: only the current one counts
    img.addEventListener('load', () => {
      if (!img.getAttribute('src')) return;
      img.hidden = false; ph.hidden = true; note();
    });
    const failed = () => {
      if (!img.getAttribute('src')) return;
      img.hidden = true; ph.hidden = false;
      note('图片加载失败：请确认链接能在浏览器里直接打开（有的图床禁止外站引用），或改用「上传图片」。', true);
    };
    img.addEventListener('error', failed);
    if (img.getAttribute('src') && img.complete && img.naturalWidth === 0) failed();
    file.addEventListener('change', () => {
      if (blob) URL.revokeObjectURL(blob);
      const f = file.files[0];
      if (!f) return show(url.value.trim());
      if (f.size > 5 << 20) { alert('图片最大 5 MB'); file.value = ''; return; }
      blob = URL.createObjectURL(f);
      show(blob);
    });
    cover.querySelector('[data-cover-link]').addEventListener('click', () => {
      url.hidden = false;
      url.focus();
    });
    url.addEventListener('input', () => {
      if (file.files.length) return;
      const v = url.value.trim();
      clearTimeout(timer);
      // wait until typing / pasting is done before loading
      timer = setTimeout(() => {
        const okURL = /^(https:\/\/|\/)/.test(v);
        show(okURL ? v : '');
        if (v && !okURL) note('图片链接需以 https:// 开头。', true);
      }, 400);
    });
    remove.addEventListener('click', () => {
      file.value = '';
      url.value = '';
      url.hidden = true;
      show('');
    });
  });
  // the cover placeholder follows the product name
  const ph = cover && cover.querySelector('[data-cover-ph][data-initial]');
  const title = ph && form.querySelector('input[name=title]');
  title?.addEventListener('input', () => { ph.textContent = (title.value.trim()[0] || 'K').toUpperCase(); });

  if (md) {
    const ta = md.querySelector('[data-md-input]');
    const pv = md.querySelector('[data-md-preview]');
    const status = md.querySelector('[data-md-status]');
    const picker = md.querySelector('[data-md-file]');
    const hint = status.textContent;
    md.setAttribute('data-tab', 'edit');
    let timer = 0, last = null;
    const render = async () => {
      if (ta.value === last) return;
      last = ta.value;
      const body = new URLSearchParams({ text: ta.value });
      try {
        const r = await fetch(base + '/markdown/preview', { method: 'POST', body, credentials: 'same-origin' });
        if (r.ok) pv.innerHTML = await r.text(); // rendered and escaped by the server
      } catch (e) { /* keep the old preview */ }
    };
    const visible = () => pv.offsetParent !== null;
    ta.addEventListener('input', () => {
      clearTimeout(timer);
      if (visible()) timer = setTimeout(render, 300);
    });
    md.querySelectorAll('[data-md-tab]').forEach((t) => t.addEventListener('click', (e) => {
      e.preventDefault();
      md.setAttribute('data-tab', t.getAttribute('data-md-tab'));
      md.querySelectorAll('[data-md-tab]').forEach((x) => x.classList.toggle('on', x === t));
      if (t.getAttribute('data-md-tab') === 'preview') render(); else ta.focus();
    }));
    if (visible()) render();
    window.matchMedia('(min-width: 1100px)').addEventListener('change', () => { if (visible()) render(); });

    // insert text around the selection
    const wrap = (before, after, placeholder) => {
      const s = ta.selectionStart, e = ta.selectionEnd;
      const sel = ta.value.slice(s, e) || placeholder;
      ta.setRangeText(before + sel + after, s, e, 'end');
      ta.selectionStart = s + before.length;
      ta.selectionEnd = s + before.length + sel.length;
      ta.focus();
      ta.dispatchEvent(new Event('input'));
    };
    // prefix every selected line (lists, headings, quotes)
    const prefix = (fn) => {
      const v = ta.value;
      const s = v.lastIndexOf('\n', ta.selectionStart - 1) + 1;
      let e = v.indexOf('\n', ta.selectionEnd);
      if (e < 0) e = v.length;
      const lines = v.slice(s, e).split('\n').map((l, i) => fn(l, i));
      ta.setRangeText(lines.join('\n'), s, e, 'select');
      ta.focus();
      ta.dispatchEvent(new Event('input'));
    };
    // insert on its own line after the selection, never replacing it
    const insertBlock = (text) => {
      const at = ta.selectionEnd;
      const before = ta.value.slice(0, at);
      const pad = before && !before.endsWith('\n') ? '\n' : '';
      ta.setRangeText(pad + text + '\n', at, at, 'end');
      ta.focus();
      ta.dispatchEvent(new Event('input'));
    };
    const strip = (l) => l.replace(/^(#{1,3} |[-*] |\d{1,3}[.、)] |> ?)/, '');
    const cmds = {
      h: () => prefix((l) => '## ' + strip(l)),
      b: () => wrap('**', '**', '加粗文字'),
      ul: () => prefix((l) => '- ' + strip(l)),
      ol: () => prefix((l, i) => (i + 1) + '. ' + strip(l)),
      quote: () => prefix((l) => '> ' + strip(l)),
      code: () => wrap('`', '`', '代码'),
      link: () => {
        const u = prompt('链接地址（https://…）', 'https://');
        if (u && /^https?:\/\/\S+$/.test(u)) wrap('[', '](' + u + ')', '链接文字');
      },
      img: () => picker.click(),
    };
    md.querySelectorAll('[data-md-cmd]').forEach((b) => b.addEventListener('click', () => cmds[b.getAttribute('data-md-cmd')]()));
    ta.addEventListener('keydown', (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'b') { e.preventDefault(); cmds.b(); }
    });

    // images: upload, then insert ![](url)
    const upload = async (files) => {
      for (const f of files) {
        if (!/^image\/(png|jpeg|gif|webp)$/.test(f.type)) { status.textContent = '只支持 PNG / JPG / GIF / WebP 图片'; continue; }
        if (f.size > 5 << 20) { status.textContent = '图片最大 5 MB'; continue; }
        status.textContent = '正在上传 ' + (f.name || '图片') + '…';
        const fd = new FormData();
        fd.append('file', f);
        try {
          const r = await fetch(base + '/uploads/image', { method: 'POST', body: fd, credentials: 'same-origin' });
          const j = await r.json();
          if (!r.ok) throw new Error(j.error || '上传失败');
          insertBlock('![' + (f.name || '').replace(/\.[^.]+$/, '').replace(/[\[\]]/g, '') + '](' + j.url + ')');
          status.textContent = '已插入图片';
        } catch (err) {
          status.textContent = '上传失败：' + err.message;
        }
      }
      setTimeout(() => { status.textContent = hint; }, 4000);
    };
    picker.addEventListener('change', () => { upload([...picker.files]); picker.value = ''; });
    ta.addEventListener('paste', (e) => {
      const files = [...(e.clipboardData?.files || [])].filter((f) => f.type.startsWith('image/'));
      if (files.length) { e.preventDefault(); upload(files); }
    });
    md.addEventListener('dragover', (e) => { if ([...e.dataTransfer.types].includes('Files')) { e.preventDefault(); md.classList.add('dragging'); } });
    md.addEventListener('dragleave', () => md.classList.remove('dragging'));
    md.addEventListener('drop', (e) => {
      const files = [...e.dataTransfer.files].filter((f) => f.type.startsWith('image/'));
      md.classList.remove('dragging');
      if (files.length) { e.preventDefault(); upload(files); }
    });
  }
})();

// Settings actions (测试连接, 测试邮件, Webhook): run in place and show the
// result right under the button instead of a flash at the top of the page.
document.querySelectorAll('form[data-action-form]').forEach((form) => {
  const box = form.querySelector('[data-result]');
  const btn = form.querySelector('button:not([type=button])');
  if (!box || !btn) return;
  const icon = box.querySelector('[data-result-icon] use');
  const set = (state, title, msg, when) => {
    box.hidden = false;
    box.className = 'action-result ' + state;
    icon.setAttribute('href', state === 'ok' ? '#i-check' : state === 'err' ? '#i-alert' : '#i-refresh');
    box.querySelector('[data-result-title]').textContent = title;
    box.querySelector('[data-result-msg]').textContent = msg;
    box.querySelector('[data-result-when]').textContent = when ? '时间：' + when : '';
  };
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const label = btn.innerHTML;
    btn.disabled = true;
    btn.textContent = btn.getAttribute('data-busy') || '处理中…';
    set('busy', '处理中', '请稍候，最长约 20 秒…', '');
    try {
      const r = await fetch(form.action, { method: 'POST', body: new URLSearchParams(new FormData(form)), headers: { Accept: 'application/json' }, credentials: 'same-origin' });
      const j = await r.json();
      set(j.ok ? 'ok' : 'err', j.ok ? '成功' : '失败', j.message || (j.ok ? '' : '请求失败'), j.when);
    } catch (err) {
      set('err', '失败', '请求没有完成：' + err.message + '（检查网络后重试）', '');
    } finally {
      btn.disabled = false;
      btn.innerHTML = label;
      box.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    }
  });
});

// Category picker: 「新建分类」 shows a name field.
document.querySelectorAll('[data-cat-picker]').forEach((box) => {
  const sel = box.querySelector('[data-cat-select]');
  const field = box.querySelector('[data-cat-new]');
  const sync = () => {
    field.hidden = sel.value !== 'new';
    field.querySelector('input').required = sel.value === 'new';
  };
  sel.addEventListener('change', () => { sync(); if (sel.value === 'new') field.querySelector('input').focus(); });
  sync();
});

// Appearance settings: try a theme, accent or mode on this page at once.
(function () {
  const form = document.querySelector('form[data-theme-preview]');
  const style = document.getElementById('theme-css');
  if (!form || !style) return;
  const url = form.getAttribute('data-theme-preview');
  const pending = form.querySelector('[data-theme-pending]');
  const original = style.textContent;
  const pick = (n) => (form.querySelector('input[name=' + n + ']:checked') || {}).value || '';
  const start = { theme: pick('theme'), accent: pick('accent'), mode: pick('mode') };
  let seq = 0;
  const apply = async () => {
    const now = { theme: pick('theme'), accent: pick('accent'), mode: pick('mode') };
    const changed = now.theme !== start.theme || now.accent !== start.accent || now.mode !== start.mode;
    pending.hidden = !changed;
    if (!changed) { style.textContent = original; return; }
    const my = ++seq;
    try {
      const r = await fetch(url + '?' + new URLSearchParams(now), { credentials: 'same-origin' });
      if (r.ok && my === seq) style.textContent = await r.text();
    } catch (e) { /* keep the current look */ }
  };
  form.addEventListener('change', (e) => { if (['theme', 'accent', 'mode'].includes(e.target.name)) apply(); });
  form.querySelector('[data-theme-revert]').addEventListener('click', (e) => {
    e.preventDefault();
    for (const [n, v] of Object.entries(start)) {
      const el = form.querySelector('input[name=' + n + '][value="' + v + '"]');
      if (el) el.checked = true;
    }
    apply();
  });
})();

// Product page: long descriptions fold on phones, with 展开全部.
(function () {
  const desc = document.querySelector('[data-desc]');
  if (!desc) return;
  const body = desc.querySelector('[data-desc-body]');
  const more = desc.querySelector('[data-desc-more]');
  if (window.matchMedia('(max-width: 900px)').matches && body.scrollHeight > 260) {
    body.classList.add('clamped');
    more.hidden = false;
    more.addEventListener('click', () => { body.classList.remove('clamped'); more.hidden = true; });
  }
})();

// Styled file buttons: show the picked file's name.
document.addEventListener('change', (e) => {
  const inp = e.target;
  if (!inp.matches || !inp.matches('.file-btn input[type=file]')) return;
  const name = inp.closest('.file-btn').querySelector('[data-file-name]');
  if (name) name.textContent = inp.files[0] ? inp.files[0].name : '选择文件';
});

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
