// Скрипт веб-интерфейса hcgateway. Страницы работают и без него: скрипт только прячет
// лишние поля, показывает вкладки, обновляет состояние приборов и бережёт введённое.
"use strict";

// Поля с data-sources видны только при выбранном источнике из списка. Скрытые поля
// отключаются: браузер их не отправляет.
function bindSources(select) {
  const form = select.form;
  const update = () => {
    form.querySelectorAll("[data-sources]").forEach((el) => {
      const on = el.dataset.sources.split(" ").includes(select.value);
      el.hidden = !on;
      el.querySelectorAll("input, select, textarea").forEach((i) => { i.disabled = !on; });
    });
  };
  select.addEventListener("change", update);
  update();
}

// Поле с data-when видно, только когда поле формы с этим именем равно одному из data-is
// (через пробел) или начинается с data-prefix. Скрытые поля отключаются: браузер их не
// отправляет.
function bindWhen(el) {
  const field = el.closest("form").elements.namedItem(el.dataset.when);
  if (!field) {
    return;
  }
  const update = () => {
    const v = field.value;
    const on = (el.dataset.is || "").split(" ").includes(v) || (el.dataset.prefix !== undefined && v.startsWith(el.dataset.prefix));
    el.hidden = !on;
    el.querySelectorAll("input, select, textarea").forEach((i) => { i.disabled = !on; });
    el.querySelectorAll("[data-when]").forEach((inner) => inner.dispatchEvent(new Event("refresh")));
  };
  field.addEventListener("change", update);
  // Внешнее поле с data-when только что показало или спрятало это: условие проверяется снова.
  el.addEventListener("refresh", () => {
    if (!el.parentElement.closest("[hidden]")) {
      update();
    }
  });
  update();
}

// Выбор прибора аккаунта (data-fill-name) подставляет его имя, если имя пустое или
// подставлено раньше.
function bindName(select) {
  const name = select.form.querySelector("input[name=name]");
  if (!name) {
    return;
  }
  let auto = "";
  select.addEventListener("change", () => {
    const option = select.selectedOptions[0];
    const text = option && option.value ? (option.dataset.name || option.textContent.split(",")[0].trim()) : "";
    if (name.value === "" || name.value === auto) {
      name.value = text;
      auto = text;
      name.dispatchEvent(new Event("input", { bubbles: true }));
    }
  });
}

// Идентификатор прибора из имени латиницей, пока владелец не ввёл свой. Правила - те же,
// что у шлюза: он сам так составляет идентификатор, если поле пустое.
const translit = {
  "а": "a", "б": "b", "в": "v", "г": "g", "д": "d", "е": "e", "ё": "e", "ж": "zh", "з": "z", "и": "i",
  "й": "y", "к": "k", "л": "l", "м": "m", "н": "n", "о": "o", "п": "p", "р": "r", "с": "s", "т": "t",
  "у": "u", "ф": "f", "х": "kh", "ц": "ts", "ч": "ch", "ш": "sh", "щ": "shch", "ъ": "", "ы": "y", "ь": "",
  "э": "e", "ю": "yu", "я": "ya",
};

function slug(name) {
  let out = "";
  let dash = false;
  for (const ch of name.toLowerCase()) {
    let part;
    if (ch in translit) {
      part = translit[ch];
    } else if (/^[a-z0-9]$/.test(ch)) {
      part = ch;
    } else {
      dash = out.length > 0;
      continue;
    }
    if (part === "") {
      continue;
    }
    if (dash) {
      out += "-";
      dash = false;
    }
    out += part;
  }
  if (out.length > 64) {
    out = out.slice(0, 64).replace(/-+$/, "");
  }
  return out || "device";
}

function bindSlug(input) {
  const name = input.form.querySelector("input[name=name]");
  const taken = new Set((input.dataset.taken || "").split(" ").filter(Boolean));
  let auto = input.value === "";
  const free = (base) => {
    let id = base;
    for (let n = 2; taken.has(id); n++) {
      const suffix = "-" + n;
      id = base.slice(0, 64 - suffix.length).replace(/-+$/, "") + suffix;
    }
    return id;
  };
  const update = () => {
    if (auto) {
      input.value = name.value.trim() ? free(slug(name.value)) : "";
    }
  };
  name.addEventListener("input", update);
  input.addEventListener("input", () => { auto = input.value === ""; });
  update();
}

// Вкладки страницы прибора. Без скрипта ссылки вкладок ведут к разделам. Вкладка - в адресе
// (#tab-…), поэтому обновление страницы её не сбрасывает. Адрес раздела (#modes) открывает
// вкладку раздела; data-active и data-focus - вкладка и раздел страницы с ошибкой формы.
function bindTabs(nav) {
  const links = Array.from(nav.querySelectorAll("a[href^='#']"));
  const panels = links.map((a) => document.getElementById(a.hash.slice(1))).filter(Boolean);
  if (panels.length === 0) {
    return;
  }
  const show = (panel) => {
    links.forEach((a) => {
      const on = a.hash.slice(1) === panel.id;
      a.classList.toggle("on", on);
      if (on) {
        a.setAttribute("aria-current", "page");
      } else {
        a.removeAttribute("aria-current");
      }
    });
    panels.forEach((p) => { p.hidden = p !== panel; });
  };
  const panelOf = (el) => el && panels.find((p) => p.contains(el));
  const focus = nav.dataset.focus ? document.getElementById(nav.dataset.focus) : null;
  show(panelOf(target()) || panelOf(focus) || document.getElementById(nav.dataset.active || "") || panels[0]);
  if (!location.hash && focus) {
    focus.scrollIntoView();
  }
  links.forEach((a) => a.addEventListener("click", (e) => {
    e.preventDefault();
    show(document.getElementById(a.hash.slice(1)));
    history.replaceState(null, "", a.hash);
  }));
  window.addEventListener("hashchange", () => {
    const el = target();
    const panel = panelOf(el);
    if (panel) {
      show(panel);
      if (!panels.includes(el)) {
        el.scrollIntoView();
      }
    }
  });
}

// target - элемент из адреса страницы (#раздел) или null.
function target() {
  if (!location.hash) {
    return null;
  }
  try {
    return document.getElementById(decodeURIComponent(location.hash.slice(1)));
  } catch {
    return null;
  }
}

// Раздел из адреса виден: свёрнутые разделы вокруг него раскрываются.
function reveal(el) {
  for (let p = el; p; p = p.parentElement) {
    if (p.tagName === "DETAILS") {
      p.open = true;
    }
  }
}

// Сообщение о сделанном - в разделе, где была кнопка: после действия шлюз ведёт на адрес
// раздела (#раздел). Плитка прибора получает сообщение внизу.
function placeFlash(el) {
  const flash = document.querySelector("main > .flash");
  if (!flash || !el || el.classList.contains("tab")) {
    return;
  }
  if (el.classList.contains("tile")) {
    el.append(flash);
    return;
  }
  const head = el.querySelector(":scope > summary, :scope > h2, :scope > h3");
  if (head) {
    head.after(flash);
  } else {
    el.prepend(flash);
  }
}

// Страница прибора и «Всё о приборе»: поток SSE присылает снимок состояния при каждом
// изменении. Строки собираются через textContent: значения прибора не становятся разметкой.
function cell(row, text, code) {
  const td = row.insertCell();
  if (code) {
    const c = document.createElement("code");
    c.textContent = text;
    td.appendChild(c);
  } else {
    td.textContent = text;
  }
  return td;
}

function span(className, text) {
  const s = document.createElement("span");
  s.className = className;
  s.textContent = text;
  return s;
}

function fill(tbody, items, empty, columns, render) {
  if (!tbody) {
    return;
  }
  tbody.replaceChildren();
  if (!items || items.length === 0) {
    const td = tbody.insertRow().insertCell();
    td.colSpan = columns;
    td.textContent = empty;
  } else {
    items.forEach((item) => render(tbody.insertRow(), item));
  }
  bindCards(tbody.closest("table"));
}

// aliceCells - устройство Алисы и его умения: как в шаблоне страницы.
function aliceCells(row, a) {
  const name = row.insertCell();
  name.append(a.name + " ", span("tech", a.id));
  const td = row.insertCell();
  if (a.error) {
    td.append(span("err", a.error), " ", span("tech", a.code), document.createElement("br"));
  }
  (a.items || []).forEach((it) => {
    const b = document.createElement("b");
    b.textContent = it.value;
    td.append(it.label + ": ", b, " ", span("tech", it.tech), document.createElement("br"));
  });
}

// compactCells - то же коротко, для страницы прибора: имена Яндекса - в подсказке при
// наведении. Недоступность устройства и так видна по метке состояния.
function compactCells(row, a) {
  const name = row.insertCell();
  name.textContent = a.name;
  name.title = a.id;
  const td = row.insertCell();
  if (a.error && a.code !== "DEVICE_UNREACHABLE") {
    const err = span("err", a.error);
    err.title = a.code;
    td.append(err, document.createElement("br"));
  }
  (a.items || []).forEach((it) => {
    const item = document.createElement("span");
    item.title = it.tech;
    const b = document.createElement("b");
    b.textContent = it.value;
    item.append(it.label + ": ", b);
    td.append(item, document.createElement("br"));
  });
}

// chip - метка состояния: run - работает, idle - на связи, off - нет связи, warn - требует
// внимания, err - ошибка.
function chip(tone, text) {
  return span("chip " + tone, text);
}

// stateChips - метка состояния и, если прибор требует внимания, метка с причиной.
function stateChips(tone, text, notice) {
  return notice ? [chip(tone, text), chip("warn", notice)] : [chip(tone, text)];
}

function bindLive(live) {
  const source = new EventSource(live.dataset.stream);
  source.addEventListener("state", (e) => {
    const s = JSON.parse(e.data);
    document.querySelectorAll("[data-online]").forEach((el) => el.replaceChildren(...stateChips(s.tone, s.summary, s.notice)));
    document.querySelectorAll("[data-offline]").forEach((el) => {
      el.hidden = s.online;
    });
    const valueCells = (row, v) => {
      row.classList.toggle("stale", Boolean(v.last));
      cell(row, v.key, true);
      const value = cell(row, v.value);
      if (v.last) {
        value.append(" ", span("tech", v.last));
      }
      cell(row, v.mapping);
    };
    fill(document.getElementById("values"), s.values, "Прибор пока ничего не сообщил.", 3, valueCells);
    fill(document.getElementById("values-empty"), s.emptyFavorites, "", 3, valueCells);
    document.querySelectorAll("[data-empty-favorites]").forEach((el) => {
      const n = s.emptyFavorites ? s.emptyFavorites.length : 0;
      el.hidden = n === 0;
      el.querySelector("[data-count]").textContent = n;
    });
    fill(document.getElementById("mappings"), s.mappings, "", 6, mapCells);
    fill(document.getElementById("history"), s.history, "Изменений пока не было.", 4, (row, h) => {
      cell(row, h.time);
      cell(row, h.kind);
      cell(row, h.key, true);
      cell(row, h.value);
    });
    const alice = document.getElementById("alice");
    if (alice && alice.hasAttribute("data-compact")) {
      // Страница прибора: без связи таблица скрыта, устройства без значений не показаны.
      alice.closest(".wrap").hidden = !s.online;
      const shown = (s.alice || []).filter((a) => (a.items && a.items.length > 0) || (a.error && a.code !== "DEVICE_UNREACHABLE"));
      fill(alice, shown, "", 2, compactCells);
    } else {
      fill(alice, s.alice, "", 2, aliceCells);
    }
  });
}

// setSwitch показывает на кнопке питания положение on.
function setSwitch(button, on) {
  button.value = on ? "false" : "true";
  button.classList.toggle("on", on);
  button.setAttribute("aria-checked", String(on));
  button.querySelector("span").textContent = on ? "Выключить" : "Включить";
}

// Кнопка питания на плитке - без перезагрузки страницы. Кнопка сразу показывает новое
// положение, а пока команда в пути - кольцо ожидания. Если прибор не выполнил команду, кнопка
// возвращается, а под плиткой - причина. Без скрипта форма отправляется как обычно.
function bindSwitch(form) {
  const button = form.querySelector("button.switch");
  if (!button || !window.fetch) {
    return;
  }
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    // Общая отправка форм (bindForms) не нужна: страница не уходит.
    e.stopPropagation();
    if (form.dataset.busy) {
      return;
    }
    const on = button.value === "true";
    const body = new URLSearchParams(new FormData(form));
    body.set("value", button.value);
    form.dataset.busy = "1";
    setSwitch(button, on);
    button.classList.add("pending");
    const tile = form.closest(".tile");
    tile.querySelectorAll(".flash").forEach((el) => el.remove());
    let reason = "";
    try {
      const resp = await fetch(form.action, { method: "POST", body, headers: { Accept: "application/json" }, credentials: "same-origin" });
      const r = await resp.json();
      if (!r.ok) {
        reason = r.text;
      }
    } catch (err) {
      reason = "Не выполнено: шлюз не ответил.";
    }
    button.classList.remove("pending");
    delete form.dataset.busy;
    if (reason) {
      setSwitch(button, !on);
      const p = document.createElement("p");
      p.className = "flash err";
      p.setAttribute("role", "status");
      p.textContent = reason;
      tile.append(p);
    }
  });
}

// Полоса «Требует внимания»: сколько приборов по каждой причине. data-problems - причины
// «ключ:слова» через «|» в порядке полосы; data-filter - показаны только такие приборы, и
// полоса не прячется.
function updateAttention(bar, rows) {
  const count = {};
  rows.forEach((d) => {
    if (d.problem) {
      count[d.problem] = (count[d.problem] || 0) + 1;
    }
  });
  const parts = [];
  bar.dataset.problems.split("|").forEach((p) => {
    const i = p.indexOf(":");
    const n = count[p.slice(0, i)];
    if (n) {
      parts.push(p.slice(i + 1) + ": " + n);
    }
  });
  const filter = bar.hasAttribute("data-filter");
  bar.querySelector("[data-counts]").textContent = parts.join(" · ") || (filter ? "сейчас таких нет" : "");
  bar.hidden = !filter && parts.length === 0;
}

// Список приборов: поток SSE присылает состояние всех приборов, метки и кнопки питания на
// плитках обновляются на месте.
function bindDevices(list) {
  const source = new EventSource(list.dataset.stream);
  const bar = document.getElementById("attention");
  const filter = bar && bar.hasAttribute("data-filter");
  source.addEventListener("list", (e) => {
    const rows = JSON.parse(e.data);
    if (bar) {
      updateAttention(bar, rows);
    }
    rows.forEach((d) => {
      const tile = document.getElementById("d-" + d.id);
      if (!tile) {
        return;
      }
      tile.classList.remove("run", "idle", "off", "warn", "err");
      tile.classList.add(d.tone);
      if (filter) {
        tile.hidden = !d.problem;
      }
      tile.querySelector(".state").replaceChildren(...stateChips(d.tone, d.state, d.notice));
      const form = tile.querySelector(".tile-foot form");
      if (form && !form.dataset.busy) {
        form.hidden = !d.switch;
        setSwitch(form.querySelector("button"), d.on);
      }
    });
  });
}

// Тема: выбор сразу меняет страницу и запоминается в cookie браузера на год. По cookie шлюз
// сам ставит тему в разметку следующих страниц. Кнопка «Применить» нужна только без скрипта.
function bindTheme(select) {
  select.form.querySelector("[data-theme-apply]").hidden = true;
  select.addEventListener("change", () => {
    const theme = select.value;
    if (theme) {
      document.documentElement.dataset.theme = theme;
      document.cookie = "hcgateway-theme=" + theme + "; Path=/; Max-Age=31536000; SameSite=Strict; Secure";
    } else {
      delete document.documentElement.dataset.theme;
      document.cookie = "hcgateway-theme=; Path=/; Max-Age=0; SameSite=Strict; Secure";
    }
    document.querySelectorAll("select[data-theme-select]").forEach((other) => { other.value = theme; });
  });
}

// Редактор сопоставлений: поле имени с подсказками прибора (input[list]) становится списком.
// Последний вариант «другое…» показывает поле для имени, которого прибор не сообщал.
function bindChoices(input) {
  const list = document.getElementById(input.getAttribute("list"));
  if (!list || list.options.length === 0 || input.closest("[data-sources]")) {
    return;
  }
  const select = document.createElement("select");
  const empty = document.createElement("option");
  empty.value = "";
  empty.textContent = input.placeholder || "-";
  select.append(empty);
  let found = input.value === "";
  // У функций прибора (data-keys) подпись - подсказка к имени: имя первым.
  const keys = list.hasAttribute("data-keys");
  Array.from(list.options).forEach((o) => {
    const opt = document.createElement("option");
    opt.value = o.value;
    const label = o.getAttribute("label") || "";
    if (keys) {
      opt.textContent = label ? o.value + " · " + label : o.value;
    } else {
      opt.textContent = label && label !== o.value ? label + " (" + o.value + ")" : o.value;
    }
    if (o.value === input.value) {
      opt.selected = true;
      found = true;
    }
    select.append(opt);
  });
  const other = document.createElement("option");
  other.value = "\u0000other";
  other.textContent = "другое…";
  select.append(other);
  if (!found) {
    other.selected = true;
  }
  const sync = () => {
    const custom = select.value === other.value;
    input.hidden = !custom;
    if (!custom) {
      input.value = select.value;
    }
  };
  select.addEventListener("change", () => {
    sync();
    input.dispatchEvent(new Event("input", { bubbles: true }));
    if (!input.hidden) {
      input.focus();
    }
  });
  input.before(select);
  sync();
}

// option - подсказка из списка поля input для его значения или null.
function option(input) {
  const list = document.getElementById(input.getAttribute("list"));
  return list ? Array.from(list.options).find((o) => o.value === input.value) || null : null;
}

// Подсказка у поля функции прибора (data-hint): значение сейчас и пределы - из подписи
// варианта в списке поля. Выбор другой функции сразу меняет подсказку.
function bindHint(hint) {
  const input = hint.parentElement.querySelector("input[list]");
  if (!input) {
    return;
  }
  const update = () => {
    const o = option(input);
    hint.textContent = o ? o.getAttribute("label") || "" : "";
  };
  input.addEventListener("input", update);
  input.addEventListener("change", update);
}

// Имя для голоса (data-names - имена и комнаты приборов, data-self - сам прибор): подсказка
// «В Алисе: …» и предупреждение, если в той же комнате уже есть прибор с таким именем.
function bindNames(form) {
  const name = form.querySelector('input[name="name"]');
  const room = form.querySelector('input[name="room"]');
  const voice = form.querySelector("[data-voice]");
  const dupe = form.querySelector("[data-dupe]");
  if (!name || !room || !voice || !dupe) {
    return;
  }
  let list = [];
  try {
    list = JSON.parse(form.dataset.names || "[]") || [];
  } catch (e) {
    list = [];
  }
  const key = (s) => s.trim().toLowerCase();
  const update = () => {
    const r = room.value.trim();
    voice.textContent = name.value.trim() ? "В Алисе: «" + name.value.trim() + "», " + (r ? "комната «" + r + "»" : "без комнаты") : "";
    const other = list.find((d) => d.id !== form.dataset.self && key(d.name) === key(name.value) && key(d.room) === key(r));
    dupe.textContent = other ? (r ? "В комнате «" + r + "» уже есть прибор «" : "Без комнаты уже есть прибор «") + other.name + "»: голосом их не различить." : "";
    dupe.hidden = !other;
  };
  name.addEventListener("input", update);
  room.addEventListener("input", update);
  update();
}

// formulas - готовые пересчёты показания: множитель и слагаемое, как у шлюза.
const formulas = {
  minutes_to_seconds: [60, 0],
  hours_to_seconds: [3600, 0],
  fahrenheit_to_celsius: [5 / 9, -160 / 9],
  fraction_to_percent: [100, 0],
};

function decimal(text, fallback) {
  const x = parseFloat(String(text).replace(",", "."));
  return Number.isNaN(x) ? fallback : x;
}

// Пересчёт показания до сохранения (data-result): значение прибора сейчас - data-now у
// варианта в списке состояний, формула - выбранный пересчёт. Результат - до сотых, как у шлюза.
function bindResult(p) {
  const box = p.closest("fieldset");
  const field = (name) => box.querySelector('[name$=".' + name + '"]');
  const status = field("status");
  const convert = field("convert");
  const scale = field("scale");
  const offset = field("offset");
  const unit = field("unit");
  if (!status || !convert) {
    return;
  }
  const update = () => {
    const o = option(status);
    const raw = o && o.dataset.now !== undefined ? decimal(o.dataset.now, NaN) : NaN;
    if (Number.isNaN(raw)) {
      p.textContent = "";
      return;
    }
    let f = formulas[convert.value];
    if (convert.value === "custom") {
      f = [decimal(scale && scale.value, 1), decimal(offset && offset.value, 0)];
    }
    const v = f ? raw * f[0] + f[1] : raw;
    const u = unit && unit.value ? option(unit) : null;
    const label = u ? u.getAttribute("label") || u.value : unit ? unit.value : "";
    p.textContent = "сейчас " + raw + " → " + Math.round(v * 100) / 100 + (label ? " " + label : "");
  };
  [status, convert, scale, offset, unit].forEach((el) => {
    if (el) {
      el.addEventListener("input", update);
      el.addEventListener("change", update);
    }
  });
}

// mapCells - строка сводки сопоставлений: как в шаблоне страницы прибора.
function mapCells(row, m) {
  if (m.program) {
    cell(row, "программа прибора");
  } else {
    cell(row, m.label, true).firstChild.title = m.key;
  }
  cell(row, m.now || "-");
  cell(row, m.convert || "-");
  cell(row, m.alice);
  cell(row, m.value || "-");
  const a = document.createElement("a");
  a.href = "#" + m.section;
  a.textContent = "Изменить";
  row.insertCell().append(a);
}

// Ссылка «Сопоставить» (?map=имя#раздел) вписывает имя в первое пустое поле имени функции
// прибора (data-map) в разделе.
function prefill(section) {
  const key = new URLSearchParams(location.search).get("map");
  if (!key || !section) {
    return;
  }
  const input = Array.from(section.querySelectorAll("input[data-map]")).find((i) => i.value === "");
  if (input) {
    input.value = key;
    input.hidden = false;
    input.focus();
  }
}

// Таблицы с заголовком из трёх и более колонок на узком экране - карточки: у каждой ячейки
// подпись её колонки.
function bindCards(table) {
  const head = table && table.rows[0];
  if (!head || head.cells.length < 3 || !Array.from(head.cells).every((c) => c.tagName === "TH")) {
    return;
  }
  table.classList.add("cards");
  head.classList.add("head");
  const labels = Array.from(head.cells).map((c) => c.textContent.trim());
  Array.from(table.rows).slice(1).forEach((r) => {
    Array.from(r.cells).forEach((c, i) => {
      if (labels[i] && r.cells.length === labels.length) {
        c.dataset.label = labels[i];
      }
    });
  });
}

// Кнопка «Скопировать» у адреса или секрета. Без https браузер не даёт доступа к буферу
// обмена: тогда текст выделяется, и владелец копирует его сам.
function bindCopy(el) {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = "Скопировать";
  const select = () => {
    if (el.tagName === "INPUT") {
      el.select();
      return;
    }
    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  };
  button.addEventListener("click", async () => {
    const text = el.tagName === "INPUT" ? el.value : el.textContent;
    try {
      await navigator.clipboard.writeText(text);
      button.textContent = "Скопировано";
    } catch {
      select();
      button.textContent = "Выделено, скопируйте";
    }
    setTimeout(() => { button.textContent = "Скопировать"; }, 3000);
  });
  el.after(" ", button);
}

// random - n случайных знаков из alphabet без перекоса: байты больше кратного длине
// алфавита отбрасываются.
function random(n, alphabet) {
  const limit = 256 - (256 % alphabet.length);
  let out = "";
  while (out.length < n) {
    for (const b of crypto.getRandomValues(new Uint8Array(n))) {
      if (b < limit && out.length < n) {
        out += alphabet[b % alphabet.length];
      }
    }
  }
  return out;
}

// «Создать идентификатор и секрет» для связки аккаунтов: случайные значения в поля формы
// Яндекса. Секрет остаётся в браузере, пока владелец не сохранит форму.
function bindGenerate(p) {
  const form = p.closest("form");
  const id = form.querySelector("input[name=client_id]");
  const secret = form.querySelector("input[name=client_secret]");
  let copy = false;
  p.hidden = false;
  p.querySelector("button").addEventListener("click", () => {
    id.value = "hcgateway-" + random(8, "0123456789abcdef");
    secret.type = "text";
    secret.value = random(32, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789");
    if (!copy) {
      bindCopy(id);
      bindCopy(secret);
      copy = true;
    }
    id.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

// Несохранённые правки. Форма с правкой помечает свой раздел; уход со страницы - ссылкой
// или кнопкой другой формы - браузер переспрашивает. Пока форма отправляется, её кнопки
// заняты, а на нажатой написано «Подождите…»: второе нажатие не отправит её ещё раз.
function bindForms() {
  const dirty = new Set();
  let leaving = false;
  document.querySelectorAll("main form").forEach((form) => {
    if (!form.querySelector("input[name=version]")) {
      return;
    }
    const mark = () => {
      dirty.add(form);
      const section = form.closest("section, details");
      if (section) {
        section.classList.add("dirty");
      }
    };
    form.addEventListener("input", mark);
    form.addEventListener("change", mark);
  });
  document.addEventListener("submit", (e) => {
    const form = e.target;
    if (form.dataset.busy) {
      e.preventDefault();
      return;
    }
    // «Проверить» отправляет форму режимов на другой адрес: её правка не сохраняется.
    const saves = form.method === "post" && !(e.submitter && e.submitter.hasAttribute("formaction"));
    const lost = Array.from(dirty).some((f) => f !== form || !saves);
    if (lost && !window.confirm("На странице есть несохранённые правки. Уйти без них?")) {
      e.preventDefault();
      return;
    }
    leaving = true;
    form.dataset.busy = "1";
    const submitter = e.submitter;
    // Кнопки отключаются после того, как браузер собрал форму: иначе он не отправит
    // значение нажатой кнопки.
    setTimeout(() => {
      form.querySelectorAll("button").forEach((b) => { b.disabled = true; });
      if (submitter && submitter.tagName === "BUTTON") {
        submitter.dataset.label = submitter.textContent;
        submitter.textContent = "Подождите…";
      }
    }, 0);
  });
  window.addEventListener("beforeunload", (e) => {
    if (!leaving && dirty.size > 0) {
      e.preventDefault();
      e.returnValue = "";
    }
  });
  // Страница из кэша браузера после «Назад» - с теми же кнопками, что до отправки.
  window.addEventListener("pageshow", (e) => {
    if (!e.persisted) {
      return;
    }
    leaving = false;
    document.querySelectorAll("form[data-busy]").forEach((form) => {
      delete form.dataset.busy;
      form.querySelectorAll("button").forEach((b) => {
        b.disabled = false;
        if (b.dataset.label) {
          b.textContent = b.dataset.label;
          delete b.dataset.label;
        }
      });
    });
  });
}

document.querySelectorAll("select[name=source]").forEach(bindSources);
document.querySelectorAll("[data-when]").forEach(bindWhen);
document.querySelectorAll("select[data-fill-name]").forEach(bindName);
document.querySelectorAll("input[data-slug]").forEach(bindSlug);
document.querySelectorAll("nav[data-tabs]").forEach(bindTabs);
const here = target();
reveal(here);
placeFlash(here);
prefill(here);
window.addEventListener("hashchange", () => reveal(target()));
document.querySelectorAll("input[list]").forEach(bindChoices);
document.querySelectorAll("[data-hint]").forEach(bindHint);
document.querySelectorAll("form[data-names]").forEach(bindNames);
document.querySelectorAll(".tile-foot form").forEach(bindSwitch);
document.querySelectorAll("[data-result]").forEach(bindResult);
document.querySelectorAll(".wrap > table").forEach(bindCards);
document.querySelectorAll("[data-copy]").forEach(bindCopy);
document.querySelectorAll("[data-generate]").forEach(bindGenerate);
bindForms();
if (here && !here.classList.contains("tab")) {
  here.scrollIntoView();
}
document.querySelectorAll("select[data-theme-select]").forEach(bindTheme);
if (window.EventSource) {
  document.querySelectorAll("[data-stream]").forEach((el) => {
    if (el.id === "list") {
      bindDevices(el);
    } else {
      bindLive(el);
    }
  });
}
