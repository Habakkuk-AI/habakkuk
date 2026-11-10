"use strict";
// Habakkuk Desktop's own page: the active connection, the local hk serve
// and its logs, the list of connections and the app's settings. It talks to
// the app over /desktop/api/; nothing here sees a token after it is saved.

const $ = id => document.getElementById(id);

function h(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "class") e.className = v;
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) if (kid != null && kid !== false) e.append(kid instanceof Node ? kid : String(kid));
  return e;
}

async function api(path, method = "GET", body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch("/desktop/api/" + path, opts);
  const text = await res.text();
  let j = null;
  try { j = text ? JSON.parse(text) : null; } catch { j = { error: text }; }
  if (!res.ok) throw new Error((j && j.error) || "HTTP " + res.status);
  return j;
}

let state = null;
let editing = null; // the connection being edited, or {} for a new one
let logNext = 0;
let flash = null; // { text, until }: the last action's error, shown for a while
const waiting = location.hash === "#wait"; // opened while connecting: go to the dashboard once up

function describe(st) {
  if (!st.connected) return st.unauthorized ? "The token is missing or wrong." : (st.error || "Not connected.");
  const parts = [];
  parts.push(st.channels ? `channels ${st.channels_up}/${st.channels} connected` : "no channels");
  if (st.approvals) parts.push(`${st.approvals} waiting for approval`);
  if (st.running) parts.push(`${st.running} running`);
  return parts.join(" · ");
}

function render() {
  const s = state;
  $("version").textContent = s.version ? "v" + s.version : "";
  const cur = s.connections.find(c => c.id === s.active);
  $("curName").textContent = cur ? cur.name : "No connection";
  $("dot").className = "dot" + (s.status.connected ? (s.status.channels_up === s.status.channels ? " ok" : " warn") : "");
  $("curDetail").textContent = cur ? (cur.kind === "remote" ? cur.url + " · " : "") + describe(s.status) : "Add a connection below.";
  $("openBtn").disabled = !s.status.connected;
  const shown = s.problem || (flash && Date.now() < flash.until ? flash.text : "");
  $("problem").hidden = !shown;
  $("problem").textContent = shown;

  const loc = s.local;
  $("localPanel").hidden = !loc;
  if (loc) {
    $("localState").textContent = loc.external ? "running outside the app" : loc.state;
    const bits = [];
    if (loc.home) bits.push("HABAKKUK_HOME " + loc.home);
    if (loc.pid) bits.push("pid " + loc.pid);
    if (loc.addr) bits.push("http://" + loc.addr);
    if (loc.restarts) bits.push(`${loc.restarts} restarts`);
    if (loc.last_exit) bits.push("last exit: " + loc.last_exit);
    if (loc.external) bits.push("started outside this app, so the app leaves it alone");
    $("localDetail").textContent = bits.join(" · ");
    $("startBtn").disabled = loc.external || loc.state === "running" || loc.state === "stopping";
    $("stopBtn").disabled = loc.external || loc.state === "stopped";
  }

  const list = $("conns");
  list.textContent = "";
  for (const c of s.connections) {
    list.append(h("li", { class: c.id === s.active ? "on" : "" },
      h("input", { type: "radio", name: "active", checked: c.id === s.active, "aria-label": "Use " + c.name,
        onchange: () => act(() => api(`connections/${encodeURIComponent(c.id)}/activate`, "POST", {}), true) }),
      h("span", { class: "grow" }, h("span", { class: "name" }, c.name), " ",
        h("span", { class: "muted small" }, c.kind === "remote" ? c.url : (c.home || "~/.habakkuk"))),
      h("span", { class: "badge" }, c.kind === "remote" ? (c.has_token ? "remote" : "remote, no token") : "local"),
      h("button", { type: "button", class: "link", onclick: () => edit(c) }, "Edit"),
      h("button", { type: "button", class: "link", onclick: e => remove(c, e.currentTarget) }, "Remove")));
  }
  if (!s.connections.length) list.append(h("li", { class: "muted" }, "No connections yet."));

  if (!document.activeElement || !$("settings").contains(document.activeElement)) {
    $("sHotkey").value = s.settings.hotkey || "";
    $("sLogin").checked = !!s.settings.start_at_login;
    $("sNotify").checked = !s.settings.quiet;
  }
  if (waiting && s.status.connected) location.replace("/app/");
}

async function refresh() {
  try {
    state = await api("state");
    render();
  } catch (err) {
    $("problem").hidden = false;
    $("problem").textContent = err.message;
  }
}

async function logs() {
  if (!state || !state.local) return;
  try {
    const j = await api("logs?after=" + logNext);
    if (!j.lines || !j.lines.length) return;
    const pre = $("logs");
    const atEnd = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 8;
    if (j.next < logNext) pre.textContent = ""; // a new run
    pre.append(j.lines.map(l => l.text).join("\n") + "\n");
    logNext = j.next;
    if (atEnd) pre.scrollTop = pre.scrollHeight;
  } catch {}
}

// act runs a change, shows its error in place of an alert (which a webview
// may not show), and refreshes.
async function act(fn, reload) {
  try {
    await fn();
  } catch (err) {
    flash = { text: err.message, until: Date.now() + 8000 };
  }
  if (reload) {
    logNext = 0;
    $("logs").textContent = "";
  }
  await refresh();
}

function showKind() {
  const kind = $("fKind").value;
  for (const el of document.querySelectorAll("[data-kind]")) el.hidden = el.dataset.kind !== kind;
}

function edit(c) {
  editing = c || {};
  $("editor").hidden = false;
  $("editTitle").textContent = c ? "Edit " + c.name : "Add a connection";
  $("fName").value = c?.name || "";
  $("fKind").value = c?.kind || "remote";
  $("fURL").value = c?.url || "";
  $("fToken").value = "";
  $("fToken").placeholder = c?.has_token ? "stored in the keychain; type to replace" : "the value of http.token_env";
  $("fHome").value = c?.home || "";
  $("fBinary").value = c?.binary || "";
  $("fManual").checked = !!c?.manual;
  $("formMsg").textContent = "";
  showKind();
  $("fName").focus();
}

function formBody() {
  const body = {
    id: editing?.id || "", name: $("fName").value, kind: $("fKind").value, url: $("fURL").value,
    home: $("fHome").value, binary: $("fBinary").value, manual: $("fManual").checked,
  };
  if ($("fToken").value) body.token = $("fToken").value;
  return body;
}

// remove asks for a second click instead of a confirm() dialog.
function remove(c, btn) {
  if (btn.dataset.sure !== "1") {
    btn.dataset.sure = "1";
    btn.textContent = "Click again to remove";
    setTimeout(() => { delete btn.dataset.sure; btn.textContent = "Remove"; }, 4000);
    return;
  }
  act(() => api("connections/" + encodeURIComponent(c.id), "DELETE"), true);
}

$("fKind").addEventListener("change", showKind);
$("addBtn").addEventListener("click", () => edit(null));
$("cancelBtn").addEventListener("click", () => { $("editor").hidden = true; editing = null; });
$("editForm").addEventListener("submit", async e => {
  e.preventDefault();
  try {
    await api("connections", "POST", formBody());
    $("editor").hidden = true;
    editing = null;
    refresh();
  } catch (err) {
    $("formMsg").className = "small err";
    $("formMsg").textContent = err.message;
  }
});
$("testBtn").addEventListener("click", async () => {
  const msg = $("formMsg");
  msg.className = "small muted";
  msg.textContent = "Testing…";
  try {
    const b = formBody();
    const j = await api("test", "POST", { id: b.id, url: b.url, token: b.token || "" });
    msg.className = "small " + (j.ok ? "" : "err");
    msg.textContent = j.ok ? `Connected: agents ${j.agents.join(", ")}` : j.error;
  } catch (err) {
    msg.className = "small err";
    msg.textContent = err.message;
  }
});
$("openBtn").addEventListener("click", () => location.assign("/app/"));
$("startBtn").addEventListener("click", () => act(() => api("local/start", "POST", {}), true));
$("stopBtn").addEventListener("click", () => act(() => api("local/stop", "POST", {})));
$("settings").addEventListener("submit", async e => {
  e.preventDefault();
  const msg = $("setMsg");
  try {
    const j = await api("settings", "PUT", { hotkey: $("sHotkey").value, start_at_login: $("sLogin").checked, quiet: !$("sNotify").checked });
    msg.className = "small " + (j.warning ? "err" : "muted");
    msg.textContent = j.warning || "Saved.";
  } catch (err) {
    msg.className = "small err";
    msg.textContent = err.message;
  }
  refresh();
});

refresh().then(logs);
setInterval(refresh, waiting ? 700 : 3000);
setInterval(logs, 1500);
