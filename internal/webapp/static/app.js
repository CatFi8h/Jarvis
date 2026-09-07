/* GR Train Watch — Telegram Mini App frontend.
   Flow: direction → date → train list → tap "Watch" → seat alerts in chat. */

const tg = window.Telegram?.WebApp;
tg?.ready();
tg?.expand();

const state = {
  meta: null,     // {directions, min_date, max_date}
  dir: null,      // selected direction {token, from, to}
  date: null,     // selected YYYY-MM-DD
};

// ── API ─────────────────────────────────────────────────────────────────────

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: {
      "Content-Type": "application/json",
      "Authorization": "tma " + (tg?.initData || ""),
      ...opts.headers,
    },
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || ("HTTP " + res.status));
  return body;
}

// ── helpers ─────────────────────────────────────────────────────────────────

const $ = (id) => document.getElementById(id);

function show(screenId) {
  for (const s of document.querySelectorAll(".screen")) s.classList.add("hidden");
  $(screenId).classList.remove("hidden");
  if (screenId === "screen-home") tg?.BackButton.hide();
  else tg?.BackButton.show();
  window.scrollTo(0, 0);
}

tg?.BackButton.onClick(() => {
  if (!$("screen-trains").classList.contains("hidden")) show("screen-date");
  else show("screen-home");
});

let toastTimer;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 2600);
}

function haptic(kind) {
  try { tg?.HapticFeedback.notificationOccurred(kind); } catch {}
}

function confirmDialog(msg, cb) {
  if (tg?.showConfirm) tg.showConfirm(msg, (ok) => ok && cb());
  else if (window.confirm(msg)) cb();
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

const DOW = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

function fmtDate(iso) {
  const [y, m, d] = iso.split("-").map(Number);
  const dt = new Date(y, m - 1, d);
  return { dow: DOW[dt.getDay()], label: `${MON[m - 1]} ${d}` };
}

// ── home: directions + watches ──────────────────────────────────────────────

function renderDirections() {
  const box = $("dir-buttons");
  box.replaceChildren();
  for (const d of state.meta.directions) {
    const b = el("button", "dir-btn");
    b.append(el("span", null, d.from), el("span", "arrow", "→"), el("span", null, d.to));
    b.onclick = () => { state.dir = d; openDateScreen(); };
    box.append(b);
  }
}

async function loadWatches() {
  try {
    const { searches } = await api("/api/searches");
    renderWatches(searches);
  } catch (e) {
    toast("Couldn't load watches: " + e.message);
  }
}

function renderWatches(searches) {
  const box = $("watch-list");
  box.replaceChildren();
  if (!searches.length) {
    box.append(el("p", "hint", "No active watches yet."));
    return;
  }
  for (const s of searches) {
    const card = el("div", "card");
    const row1 = el("div", "row1");
    row1.append(el("span", "train-no", `${s.route} · ${s.date}`));
    if (s.available) row1.append(el("span", "badge", "SEATS!"));
    card.append(row1);
    card.append(el("div", "times", s.target));
    card.append(el("div", "status" + (s.available ? " ok" : ""), s.last_result));

    const actions = el("div", "card-actions");
    if (s.available) {
      const book = el("button", "btn btn-primary", "Book now");
      book.onclick = () => tg?.openLink ? tg.openLink(s.book_url) : window.open(s.book_url);
      actions.append(book);
    }
    const check = el("button", "btn btn-muted", "Check now");
    check.onclick = async () => {
      check.disabled = true;
      try {
        await api(`/api/searches/${s.id}/check`, { method: "POST" });
        await loadWatches();
      } catch (e) { toast(e.message); check.disabled = false; }
    };
    const del = el("button", "btn btn-danger", "Stop");
    del.onclick = () => confirmDialog(`Stop watching ${s.target} on ${s.date}?`, async () => {
      try {
        await api(`/api/searches/${s.id}`, { method: "DELETE" });
        haptic("success");
        await loadWatches();
      } catch (e) { toast(e.message); }
    });
    actions.append(check, del);
    card.append(actions);
    box.append(card);
  }
}

// ── date screen ─────────────────────────────────────────────────────────────

function openDateScreen() {
  $("date-title").textContent = `${state.dir.from} → ${state.dir.to}`;
  const grid = $("date-grid");
  grid.replaceChildren();

  const [y, m, d] = state.meta.min_date.split("-").map(Number);
  const start = new Date(y, m - 1, d);
  const end = state.meta.max_date;
  for (let i = 0; ; i++) {
    const dt = new Date(start.getFullYear(), start.getMonth(), start.getDate() + i);
    const iso = `${dt.getFullYear()}-${String(dt.getMonth() + 1).padStart(2, "0")}-${String(dt.getDate()).padStart(2, "0")}`;
    if (iso > end) break;
    const b = el("button", "date-btn");
    if (i === 0) { b.classList.add("special"); b.append(el("span", null, "Today")); }
    else if (i === 1) { b.classList.add("special"); b.append(el("span", null, "Tomorrow")); }
    else {
      const f = fmtDate(iso);
      b.append(el("span", null, f.label));
      b.append(el("span", "dow", f.dow));
    }
    b.onclick = () => { state.date = iso; openTrainsScreen(); };
    grid.append(b);
  }
  show("screen-date");
}

// ── trains screen ───────────────────────────────────────────────────────────

async function openTrainsScreen() {
  $("trains-title").textContent = `${state.dir.from} → ${state.dir.to}`;
  const f = fmtDate(state.date);
  $("trains-sub").textContent = `${f.dow}, ${f.label} — tap a train to watch it`;
  const box = $("train-list");
  box.replaceChildren(el("p", "hint", "Loading trains…"));
  show("screen-trains");

  let data;
  try {
    data = await api(`/api/trains?dir=${state.dir.token}&date=${state.date}`);
  } catch (e) {
    box.replaceChildren(el("p", "hint", "⚠️ " + e.message));
    return;
  }
  renderTrains(data.trains);
}

function renderTrains(trains) {
  const box = $("train-list");
  box.replaceChildren();
  if (!trains.length) {
    box.append(el("p", "hint", "No trains returned for this day."));
    return;
  }
  for (const t of trains) {
    const card = el("div", "card");
    const row1 = el("div", "row1");
    row1.append(el("span", "train-no", `N${t.number}`));
    row1.append(el("span", "times", t.dep_time + (t.arr_time ? ` → ${t.arr_time}` : "")));
    card.append(row1);

    if (t.origin) {
      card.append(el("div", "classes", `departs ${t.origin} at ${t.dep_time}`));
    }
    if (!t.seats_known) {
      card.append(el("div", "seats", "Seats unknown — sales may not be open yet"));
    } else if (t.total_seats > 0) {
      card.append(el("div", "seats ok", `${t.total_seats} seats available`));
      card.append(el("div", "classes",
        t.classes.map((c) => `${c.name}: ${c.seats} @ ${c.price} ${c.currency}`).join(" · ")));
    } else {
      card.append(el("div", "seats bad", "Sold out"));
    }

    const actions = el("div", "card-actions");
    const btn = el("button", "btn " + (t.watched ? "btn-muted" : "btn-primary"),
      t.watched ? "✓ Watching" : (t.total_seats > 0 ? "Watch it" : "Watch for seats"));
    if (t.watched) btn.disabled = true;
    btn.onclick = async () => {
      btn.disabled = true;
      btn.textContent = "Starting…";
      try {
        const sv = await api("/api/searches", {
          method: "POST",
          body: JSON.stringify({
            dir: state.dir.token,
            date: state.date,
            train_number: String(t.number),
            dep_time: t.dep_time,
          }),
        });
        haptic("success");
        btn.textContent = "✓ Watching";
        btn.className = "btn btn-muted";
        toast(sv.available
          ? "Seats available right now — check My watches to book!"
          : "Watching started. You'll get a message when seats appear.");
        loadWatches();
      } catch (e) {
        haptic("error");
        btn.disabled = false;
        btn.textContent = "Watch it";
        toast(e.message);
      }
    };
    actions.append(btn);
    card.append(actions);
    box.append(card);
  }
}

// ── boot ────────────────────────────────────────────────────────────────────

(async function boot() {
  $("refresh-watches").onclick = loadWatches;
  try {
    state.meta = await api("/api/meta");
  } catch (e) {
    document.body.innerHTML = "";
    const p = el("p", "hint", "⚠️ " + e.message + " — open this app from Telegram.");
    p.style.padding = "24px";
    document.body.append(p);
    return;
  }
  renderDirections();
  loadWatches();
  show("screen-home");
})();
