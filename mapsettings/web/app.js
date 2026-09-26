// map-settings — страница «Настройки МАП» (отдельная программа).
// Чтение/запись по Modbus TCP к mapgateway. Режим модели, IP и порт хранятся
// на клиенте (localStorage) и восстанавливаются при открытии.
(function () {
  'use strict';

  var DEF_MODE = 'dominator';
  var DEF_IP = '';
  var DEF_PORT = '502';
  var LS_MODE = 'mapSettings.mode';
  var LS_IP = 'mapSettings.ip';
  var LS_PORT = 'mapSettings.port';

  var modeEl = document.getElementById('msMode');
  var ipEl = document.getElementById('msIP');
  var portEl = document.getElementById('msPort');
  var readBtn = document.getElementById('msRead');
  var writeBtn = document.getElementById('msWrite');
  var statusEl = document.getElementById('msStatus');
  var settingsSec = document.getElementById('msSettings');
  var settingsBody = document.getElementById('msSettingsBody');
  var monitorSec = document.getElementById('msMonitor');
  var monitorBody = document.getElementById('msMonitorBody');
  var actionsEl = document.getElementById('msActions');
  var footEl = document.getElementById('msFoot');
  var timeHEl = document.getElementById('msTimeH');
  var timeMEl = document.getElementById('msTimeM');
  var timeReadBtn = document.getElementById('msTimeRead');
  var timeWriteBtn = document.getElementById('msTimeWrite');
  var timeStatusEl = document.getElementById('msTimeStatus');

  var lastSnapshot = null;

  function lsGet(k, d) { try { var v = localStorage.getItem(k); return v === null ? d : v; } catch (e) { return d; } }
  function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }

  modeEl.value = lsGet(LS_MODE, DEF_MODE);
  ipEl.value = lsGet(LS_IP, DEF_IP);
  portEl.value = lsGet(LS_PORT, DEF_PORT);
  var autoReadDone = false;
  function maybeAutoRead() {
    if (autoReadDone) return;
    if (!ipEl.value) return;
    autoReadDone = true;
    readAll();
  }
  // Серверные значения по умолчанию (флаги программы) — подставляем, только
  // если в браузере ещё ничего не сохранено.
  fetch('/api/config', { credentials: 'same-origin' }).then(function (r) { return r.json(); }).then(function (cfg) {
    if (lsGet(LS_MODE, '') === '') modeEl.value = cfg.mode || DEF_MODE;
    if (lsGet(LS_IP, '') === '') ipEl.value = cfg.ip || '';
    if (lsGet(LS_PORT, '') === '') portEl.value = String(cfg.port || 502);
    maybeAutoRead();
  }).catch(function () { maybeAutoRead(); });
  function persist() {
    var t = target();
    lsSet(LS_MODE, t.mode);
    lsSet(LS_IP, t.ip);
    lsSet(LS_PORT, String(t.port));
    // Сохраняем и серверный конфиг (mapsettings.json), чтобы режим/IP/порт
    // восстанавливались при следующем запуске программы.
    fetch('/api/config', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(t)
    }).catch(function () {});
  }
  modeEl.addEventListener('change', persist);
  ipEl.addEventListener('change', persist);
  portEl.addEventListener('change', persist);

  function target() {
    return { mode: modeEl.value, ip: ipEl.value.trim(), port: parseInt(portEl.value, 10) || 502 };
  }
  function api(path, opts) {
    opts = opts || {};
    opts.credentials = 'same-origin';
    if (opts.body && typeof opts.body !== 'string') {
      opts.headers = { 'Content-Type': 'application/json' };
      opts.body = JSON.stringify(opts.body);
    }
    return fetch(path, opts).then(function (r) {
      return r.text().then(function (t) {
        if (!r.ok) throw new Error(t || ('HTTP ' + r.status));
        try { return JSON.parse(t); } catch (e) { throw new Error('Некорректный ответ сервера'); }
      });
    });
  }
  function setStatus(el, text, cls) {
    el.textContent = text || '';
    el.className = 'ms-status' + (cls ? ' ' + cls : '');
  }
  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  }
  function fmt(v) {
    if (v === null || v === undefined) return '—';
    if (typeof v === 'number') return (Math.round(v * 1000) / 1000).toString();
    return String(v);
  }

  // ---- Подсказки из документации ----
  var popoverEl = null;
  function hidePopover() {
    if (popoverEl && popoverEl.parentNode) popoverEl.parentNode.removeChild(popoverEl);
    popoverEl = null;
  }
  function showPopover(anchor, text) {
    hidePopover();
    var p = el('div', 'ms-popover');
    text.split('\n').forEach(function (line, i) {
      if (i > 0) p.appendChild(document.createElement('br'));
      p.appendChild(document.createTextNode(line));
    });
    document.body.appendChild(p);
    var r = anchor.getBoundingClientRect();
    var top = r.bottom + window.scrollY + 6;
    var left = r.left + window.scrollX;
    p.style.top = top + 'px';
    p.style.left = left + 'px';
    var pr = p.getBoundingClientRect();
    if (pr.right > window.innerWidth - 8) {
      p.style.left = Math.max(8, window.innerWidth - pr.width - 8) + 'px';
    }
    popoverEl = p;
  }
  function helpBtn(text, title) {
    var b = el('button', 'ms-help', '?');
    b.type = 'button';
    b.title = title ? ('Подсказка: ' + title) : 'Подсказка';
    b.setAttribute('aria-label', 'Подсказка');
    b.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      if (popoverEl && popoverEl._anchor === b) { hidePopover(); return; }
      showPopover(b, text || 'Нет описания в документации.');
      if (popoverEl) popoverEl._anchor = b;
    });
    return b;
  }
  function attachHelp(anchorEl, text) {
    if (!anchorEl || !anchorEl.parentNode) return;
    anchorEl.parentNode.insertBefore(helpBtn(text), anchorEl.nextSibling);
  }
  document.addEventListener('click', function (e) {
    if (popoverEl && !popoverEl.contains(e.target) && !e.target.classList.contains('ms-help')) hidePopover();
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') hidePopover(); });

  // ---- Рендер ----
  function groupedTables(container, groups, editable) {
    container.innerHTML = '';
    if (!groups || !groups.length) {
      container.appendChild(el('div', 'missing', 'Нет данных'));
      return;
    }
    groups.forEach(function (g) {
      var box = el('div', 'ms-group');
      box.appendChild(el('h3', 'ms-group-title', g.name));
      var wrap = el('div', 'ms-table-wrap');
      var tbl = el('table', 'ms-table');
      var cg = el('colgroup');
      ['c-name', 'c-addr', 'c-val', 'c-unit', 'c-range', 'c-desc'].forEach(function (c) {
        var col = document.createElement('col');
        col.className = c;
        cg.appendChild(col);
      });
      tbl.appendChild(cg);
      var thead = el('thead');
      var hr = el('tr');
      ['Параметр', 'Ячейка', 'Значение', 'Ед.', 'Диапазон', 'Описание'].forEach(function (h) {
        hr.appendChild(el('th', null, h));
      });
      thead.appendChild(hr);
      tbl.appendChild(thead);
      var tbody = el('tbody');
      g.params.forEach(function (p) {
        var tr = el('tr', 'ms-row');
        var tdName = el('td', 'ms-name', p.name);
        if (p.help) tdName.appendChild(helpBtn(p.help, p.name));
        tr.appendChild(tdName);
        tr.appendChild(el('td', 'ms-addr', p.addr + (p.cell ? ' ' + p.cell : '')));
        var tdVal = el('td', 'ms-val');
        if (p.error) {
          tdVal.appendChild(el('span', 'ms-err', p.error));
        } else if (editable && p.writable) {
          var inp = document.createElement('input');
          inp.type = 'number';
          inp.step = 'any';
          inp.className = 'ms-input';
          inp.value = (p.value === null || p.value === undefined) ? '' : p.value;
          inp.dataset.key = p.key;
          inp.dataset.orig = (p.value === null || p.value === undefined) ? '' : String(p.value);
          inp.title = p.desc || '';
          inp.addEventListener('input', function () {
            tr.classList.toggle('changed', inp.value !== inp.dataset.orig);
          });
          tdVal.appendChild(inp);
          if (p.text) tdVal.appendChild(el('span', 'ms-text', p.text));
        } else {
          tdVal.appendChild(el('span', 'ms-ro', fmt(p.value)));
          if (p.text) tdVal.appendChild(el('span', 'ms-text', p.text));
        }
        tr.appendChild(tdVal);
        tr.appendChild(el('td', 'ms-unit', p.unit || ''));
        var rng = '';
        if (p.min !== null && p.min !== undefined) rng += p.min;
        if (p.max !== null && p.max !== undefined) rng += (rng ? '…' : '') + p.max;
        tr.appendChild(el('td', 'ms-range', rng));
        var note = p.desc || '';
        if (p.kind === 'eeprom') note = (note ? note + ' · ' : '') + 'EEPROM';
        tr.appendChild(el('td', 'ms-desc', note));
        tbody.appendChild(tr);
      });
      tbl.appendChild(tbody);
      wrap.appendChild(tbl);
      box.appendChild(wrap);
      container.appendChild(box);
    });
  }

  function renderActions(actions) {
    actionsEl.innerHTML = '';
    (actions || []).forEach(function (a) {
      var wrap = el('span', 'ms-action-wrap');
      var b = el('button', 'ms-action-btn', a.name);
      b.type = 'button';
      b.title = a.desc || '';
      if (a.confirm) b.className += ' danger';
      b.addEventListener('click', function () {
        if (a.confirm && !window.confirm('Выполнить «' + a.name + '»?\n\n' + (a.desc || ''))) return;
        setStatus(statusEl, 'Выполняется: ' + a.name + '…');
        httpAction(a.key);
      });
      wrap.appendChild(b);
      wrap.appendChild(helpBtn(a.desc || a.name, a.name));
      actionsEl.appendChild(wrap);
    });
  }

  function renderSnap(snap) {
    lastSnapshot = snap;
    groupedTables(settingsBody, snap.settings, true);
    groupedTables(monitorBody, snap.monitor, false);
    settingsSec.hidden = false;
    monitorSec.hidden = false;
    renderActions(snap.actions);
    var errs = (snap.errors && snap.errors.length) ? ' Ошибки чтения: ' + snap.errors.join('; ') : '';
    var st = 'Прочитано ' + (snap.read_at || '') + ' · ' + target().ip + ':' + target().port +
      ' · ' + (target().mode === 'titanator' ? 'Титанатор' : 'Доминатор') + errs;
    setStatus(statusEl, st, errs ? 'warn' : 'ok');
  }

  // ---- Действия ----
  function readAll() {
    persist();
    setStatus(statusEl, 'Чтение параметров МАП…');
    readBtn.disabled = true;
    var t = target();
    var qs = '?mode=' + encodeURIComponent(t.mode) + '&ip=' + encodeURIComponent(t.ip) + '&port=' + t.port;
    api('/api/settings' + qs).then(function (snap) {
      renderSnap(snap);
    }).catch(function (e) {
      setStatus(statusEl, 'Ошибка чтения: ' + e.message, 'err');
    }).finally(function () { readBtn.disabled = false; });
  }

  function collectChanges() {
    var changes = {};
    var inputs = settingsBody.querySelectorAll('input.ms-input');
    for (var i = 0; i < inputs.length; i++) {
      var inp = inputs[i];
      if (inp.value === '') continue;
      if (inp.value === inp.dataset.orig) continue;
      var v = parseFloat(inp.value);
      if (isNaN(v)) continue;
      changes[inp.dataset.key] = v;
    }
    return changes;
  }

  function writeChanged() {
    persist();
    var changes = collectChanges();
    var keys = Object.keys(changes);
    if (!keys.length) { setStatus(statusEl, 'Нет изменённых параметров.', 'warn'); return; }
    if (!window.confirm('Записать ' + keys.length + ' изменённ(ый/ых) параметр(ов) в МАП?')) return;
    writeBtn.disabled = true;
    setStatus(statusEl, 'Запись ' + keys.length + ' параметр(ов)…');
    var t = target();
    api('/api/apply', {
      method: 'POST',
      body: { mode: t.mode, ip: t.ip, port: t.port, changes: changes }
    }).then(function (resp) {
      var bad = [];
      var results = resp.results || {};
      Object.keys(results).forEach(function (k) { bad.push(k + ': ' + results[k]); });
      if (resp.error) bad.push(resp.error);
      if (bad.length) setStatus(statusEl, 'Записано с замечаниями: ' + bad.join('; '), 'warn');
      else setStatus(statusEl, 'Записано и проверено: ' + keys.length + ' параметр(ов).', 'ok');
    }).catch(function (e) {
      setStatus(statusEl, 'Ошибка записи: ' + e.message, 'err');
    }).finally(function () {
      writeBtn.disabled = false;
      readAll();
    });
  }

  function httpAction(key) {
    var t = target();
    api('/api/action', {
      method: 'POST',
      body: { mode: t.mode, ip: t.ip, port: t.port, key: key }
    }).then(function () {
      setStatus(statusEl, 'Действие выполнено.', 'ok');
    }).catch(function (e) {
      setStatus(statusEl, 'Ошибка действия: ' + e.message, 'err');
    });
  }

  // ---- Время ----
  function readTime() {
    persist();
    setStatus(timeStatusEl, 'Чтение времени МАП…');
    var t = target();
    var qs = '?mode=' + encodeURIComponent(t.mode) + '&ip=' + encodeURIComponent(t.ip) + '&port=' + t.port;
    api('/api/time' + qs).then(function (st) {
      timeHEl.value = st.hour;
      timeMEl.value = st.minute;
      setStatus(timeStatusEl, 'Время МАП: ' + pad(st.hour) + ':' + pad(st.minute) + ' (' + (st.raw || '') + ')', 'ok');
    }).catch(function (e) {
      setStatus(timeStatusEl, 'Ошибка чтения времени: ' + e.message, 'err');
    });
  }
  function writeTime() {
    persist();
    var h = parseInt(timeHEl.value, 10);
    var m = parseInt(timeMEl.value, 10);
    if (isNaN(h) || isNaN(m) || h < 0 || h > 23 || m < 0 || m > 59) {
      setStatus(timeStatusEl, 'Некорректное время.', 'err');
      return;
    }
    if (!window.confirm('Записать время МАП ' + pad(h) + ':' + pad(m) + '?')) return;
    var t = target();
    setStatus(timeStatusEl, 'Запись времени…');
    api('/api/time', {
      method: 'POST',
      body: { mode: t.mode, ip: t.ip, port: t.port, hour: h, minute: m }
    }).then(function () {
      setStatus(timeStatusEl, 'Время записано: ' + pad(h) + ':' + pad(m), 'ok');
    }).catch(function (e) {
      setStatus(timeStatusEl, 'Ошибка записи времени: ' + e.message, 'err');
    });
  }
  function pad(n) { return (n < 10 ? '0' : '') + n; }

  readBtn.addEventListener('click', readAll);
  writeBtn.addEventListener('click', writeChanged);
  timeReadBtn.addEventListener('click', readTime);
  timeWriteBtn.addEventListener('click', writeTime);

  // Подсказки для органов управления (из документации/описания модуля).
  attachHelp(modeEl, 'Модель МАП: Титанатор или Доминатор. От режима зависит набор отображаемых параметров. Последний выбранный режим сохраняется в браузере.');
  attachHelp(ipEl, 'IP-адрес МАП / mapgateway (Modbus TCP). По умолчанию берётся из конфигурации; можно изменить и сохранить в браузере.');
  attachHelp(portEl, 'Порт Modbus TCP (обычно 502). Сохраняется в браузере.');
  attachHelp(readBtn, 'Перечитать: снять с МАП полный снимок всех параметров (настройки и мониторинг). Ничего не записывается.');
  attachHelp(writeBtn, 'Записать: отправить на МАП только те параметры, которые вы изменили. Запись обрамляется служебными командами (03 → запись → 07).');
  attachHelp(timeHEl, 'Часы текущего времени МАП (0–23).');
  attachHelp(timeMEl, 'Минуты текущего времени МАП (0–59).');
  attachHelp(timeReadBtn, 'Прочитать текущее время МАП из ячеек _LCD_TimeCyr (0x1B6) и _TimeCyr_MINUT (0x44B).');
  attachHelp(timeWriteBtn, 'Записать время МАП. Отдельная форма, не входит в общую таблицу настроек.');

  // Предзаполняем форму времени текущим локальным временем (затем можно прочитать с МАП).
  var now = new Date();
  timeHEl.value = now.getHours();
  timeMEl.value = now.getMinutes();
  footEl.textContent = 'Каталог ячеек: protocol_MAP_cells_2026_07_15.doc. Запись — со служебным обрамлением (03 → запись → 07).';

  maybeAutoRead();
})();
