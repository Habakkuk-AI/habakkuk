"use strict";
// Device nodes in the browser. This page (the dashboard, or pair.html on a
// phone) can be a node itself: it pairs with a one-time code, keeps its
// node secret in this browser, and holds a Server-Sent Events stream at
// /nodes/events that carries invocations; results go back to /nodes/result.
// It offers only what the user ticked when pairing, and each one asks the
// browser for permission (notifications, location, camera).
(() => {
  const KEY = "habakkuk.node";
  const base = new URL("../", location.href); // the server root, from /app/
  const node = { status: "off", detail: "", abort: null, watchers: new Set() };

  const load = () => { try { return JSON.parse(localStorage.getItem(KEY) || "null"); } catch { return null; } };
  const save = v => { try { v ? localStorage.setItem(KEY, JSON.stringify(v)) : localStorage.removeItem(KEY); } catch {} };
  function setStatus(status, detail) {
    node.status = status;
    node.detail = detail || "";
    for (const fn of node.watchers) { try { fn(); } catch {} }
  }

  // What a browser can offer, and the permission prompt each one needs.
  const CAPS = {
    "system.notify": {
      label: "Notifications",
      async ask() {
        if (!("Notification" in window)) throw new Error("this browser has no notifications");
        if (await Notification.requestPermission() !== "granted") throw new Error("notifications were not allowed");
      },
      async run(args) {
        const title = args.title || "Habakkuk", body = String(args.message || "");
        const reg = "serviceWorker" in navigator ? await navigator.serviceWorker.getRegistration() : null;
        if (reg && reg.showNotification) await reg.showNotification(title, { body, icon: new URL("icon-192.png", location.href).href });
        else new Notification(title, { body });
        return { text: "notification shown" };
      },
    },
    "location.get": {
      label: "Location",
      ask: () => position().then(() => {}),
      async run() {
        const p = await position();
        const c = p.coords;
        return { text: `${c.latitude}, ${c.longitude} (accuracy ${Math.round(c.accuracy)} m)` +
          (c.altitude != null ? `, altitude ${Math.round(c.altitude)} m` : "") + `, at ${new Date(p.timestamp).toISOString()}` };
      },
    },
    "camera.snap": {
      label: "Camera",
      async ask() { const s = await navigator.mediaDevices.getUserMedia({ video: true }); s.getTracks().forEach(t => t.stop()); },
      async run(args) {
        const facing = args.facing === "user" ? "user" : "environment";
        const stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: facing, width: { ideal: 1920 } } });
        try {
          const v = document.createElement("video");
          v.muted = true; v.playsInline = true; v.srcObject = stream;
          await v.play();
          await new Promise(r => setTimeout(r, 600)); // let exposure settle
          const c = document.createElement("canvas");
          c.width = v.videoWidth; c.height = v.videoHeight;
          c.getContext("2d").drawImage(v, 0, 0);
          const data = c.toDataURL("image/jpeg", 0.85).split(",")[1];
          return { text: `photo ${c.width}x${c.height}`, files: [{ name: "camera.jpg", media_type: "image/jpeg", data }] };
        } finally { stream.getTracks().forEach(t => t.stop()); }
      },
    },
  };
  function position() {
    return new Promise((ok, bad) => {
      if (!navigator.geolocation) return bad(new Error("this browser has no geolocation"));
      navigator.geolocation.getCurrentPosition(ok, e => bad(new Error(e.message || "location unavailable")),
        { enableHighAccuracy: true, timeout: 20000, maximumAge: 60000 });
    });
  }

  function headers(n, json) {
    const h = { Authorization: "Bearer " + n.secret, "X-Habakkuk-Node": n.name };
    if (json) h["Content-Type"] = "application/json";
    return h;
  }

  async function answer(n, f) {
    let result;
    try {
      const cap = CAPS[f.command];
      if (!cap || !n.caps.includes(f.command)) throw new Error("this browser does not offer " + f.command);
      result = await cap.run(f.args || {});
    } catch (err) {
      result = { error: err.message || String(err) };
    }
    await fetch(new URL("nodes/result", base), { method: "POST", headers: headers(n, true),
      body: JSON.stringify({ type: "result", id: f.id, result }) }).catch(() => {});
  }

  // start follows the event stream until stopped, reconnecting with backoff.
  // A Web Lock keeps it to one tab: a second tab waits instead of taking
  // the connection over again and again.
  function start() {
    stop();
    const n = load();
    if (!n) { setStatus("off"); return; }
    const ctl = new AbortController();
    node.abort = ctl;
    if (navigator.locks) {
      setStatus("waiting", "another tab is this node");
      navigator.locks.request(KEY, { signal: ctl.signal }, () => follow(n, ctl)).catch(() => {});
    } else {
      follow(n, ctl);
    }
  }
  async function follow(n, ctl) {
    let wait = 1000;
    while (!ctl.signal.aborted) {
      setStatus("connecting");
      try {
        const q = new URLSearchParams({ caps: n.caps.join(","), platform: "browser " + (navigator.platform || "") });
        const res = await fetch(new URL("nodes/events?" + q, base), { headers: headers(n), signal: ctl.signal });
        if (res.status === 401) {
          const j = await res.json().catch(() => ({}));
          setStatus("rejected", j.error || "this browser's node is no longer paired");
          return;
        }
        if (!res.ok) throw new Error("HTTP " + res.status);
        setStatus("online");
        wait = 1000;
        const reader = res.body.getReader(), dec = new TextDecoder();
        let buf = "";
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let i;
          while ((i = buf.indexOf("\n\n")) >= 0) {
            const block = buf.slice(0, i);
            buf = buf.slice(i + 2);
            let data = "";
            for (const line of block.split("\n")) if (line.startsWith("data:")) data += line.slice(5).trim();
            if (!data) continue;
            let f;
            try { f = JSON.parse(data); } catch { continue; }
            if (f.type === "invoke") answer(n, f);
          }
        }
      } catch (err) {
        if (ctl.signal.aborted) return;
        setStatus("offline", err.message);
      }
      if (ctl.signal.aborted) return;
      setStatus("offline", "reconnecting");
      await new Promise(r => setTimeout(r, wait));
      wait = Math.min(wait * 2, 60000);
    }
  }
  function stop() { if (node.abort) node.abort.abort(); node.abort = null; }

  // pair asks the browser for each chosen permission (inside the click that
  // started it), redeems the code and starts the node.
  async function pair(code, name, wanted) {
    const caps = [];
    for (const c of wanted) {
      try { await CAPS[c].ask(); caps.push(c); } catch (err) { throw new Error(CAPS[c].label + ": " + err.message); }
    }
    const res = await fetch(new URL("nodes/pair", base), { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code, name, platform: "browser" }) });
    const j = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(j.error || "HTTP " + res.status);
    save({ name, secret: j.secret, caps });
    start();
  }
  function forget() { stop(); save(null); setStatus("off"); }

  // el is a tiny element builder for pair.html, which has no app.js.
  function el(tag, attrs, ...kids) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v == null || v === false) continue;
      if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
      else if (k === "class") e.className = v;
      else if (k === "value" || k === "checked") e[k] = v;
      else e.setAttribute(k, v === true ? "" : v);
    }
    for (const kid of kids.flat()) if (kid != null && kid !== false) e.append(kid instanceof Node ? kid : String(kid));
    return e;
  }

  // browserPanel is the "this browser as a node" form and status, shared by
  // the dashboard page and pair.html. getCode returns a pairing code (typed,
  // or made through the API on the dashboard).
  function browserPanel(getCode, needsCode) {
    const box = el("div", { class: "panel" });
    const render = () => {
      box.textContent = "";
      box.append(el("h2", {}, "This browser as a node"));
      const n = load();
      if (n) {
        const kind = { online: "ok", connecting: "warn", offline: "bad", rejected: "bad" }[node.status] || "";
        box.append(el("div", { class: "row" }, el("strong", {}, n.name), el("span", { class: "badge " + kind }, node.status),
          el("span", { class: "muted small grow" }, node.detail)),
          el("div", { class: "small muted" }, "Offers: " + (n.caps.join(", ") || "nothing")),
          el("div", { class: "small muted" }, "It works while this page is open. Habakkuk asks you before using any of it unless config nodes.allow says otherwise."),
          el("div", { class: "row end" }, el("button", { type: "button", class: "danger", onclick: forget }, "Stop being a node")));
        return;
      }
      const name = el("input", { placeholder: "name, e.g. phone", value: "phone", "aria-label": "Node name" });
      const code = needsCode ? el("input", { placeholder: "pairing code", "aria-label": "Pairing code", value: getCode.initial || "" }) : null;
      const boxes = Object.entries(CAPS).map(([id, c]) => el("label", { class: "row" }, el("input", { type: "checkbox", value: id, checked: id !== "camera.snap" }), c.label));
      const err = el("div", { class: "small err" });
      const go = el("button", { type: "button", class: "primary", onclick: async () => {
        go.disabled = true; err.textContent = "";
        try {
          const wanted = boxes.map(b => b.querySelector("input")).filter(i => i.checked).map(i => i.value);
          await pair(code ? code.value.trim() : await getCode(), name.value.trim(), wanted);
          render();
        } catch (e) { err.textContent = e.message; go.disabled = false; }
      } }, "Pair this browser");
      box.append(el("p", { class: "small muted" }, "Let Habakkuk use this device. The browser asks for each permission you tick; you can stop at any time."),
        el("label", { class: "field" }, "Name", name), code && el("label", { class: "field" }, "Code", code), ...boxes, err,
        el("div", { class: "row end" }, go));
    };
    node.watchers.add(render);
    render();
    return { box, done: () => node.watchers.delete(render) };
  }

  // The dashboard's Nodes page (app.js is loaded).
  if (typeof pages !== "undefined") {
    pages.nodes = view => {
      const list = h("div", { class: "panel" });
      const refresh = async () => {
        let j;
        try { j = await api("nodes"); } catch (err) {
          list.textContent = "";
          const off = err.message === "HTTP 404";
          list.append(h("div", { class: off ? "empty" : "empty err" }, off ? "Nodes are off. Add a nodes section to config.yaml (for example `nodes: {}`) and restart hk serve." : err.message));
          mine.box.hidden = off;
          return;
        }
        mine.box.hidden = false;
        list.textContent = "";
        list.append(h("div", { class: "row" }, h("h2", { class: "grow" }, "Paired devices"),
          h("button", { type: "button", onclick: newCode }, "Pair a device")));
        if (!j.nodes.length) { list.append(h("div", { class: "empty" }, "No devices are paired yet.")); return; }
        list.append(table(["Node", "Platform", "State", "Offers", ""], j.nodes.map(n => h("tr", {},
          h("td", {}, h("strong", {}, n.name)), h("td", { class: "small" }, n.platform || ""),
          h("td", {}, badge(n.online ? "online" : "offline", n.online ? "ok" : ""),
            !n.online && n.last_seen && n.last_seen.slice(0, 4) !== "0001" ? h("span", { class: "muted small", title: when(n.last_seen) }, " " + ago(n.last_seen)) : null),
          h("td", { class: "small" }, (n.caps || []).join(", ")),
          h("td", {}, h("button", { type: "button", class: "danger", onclick: async () => {
            if (!confirm(`Revoke ${n.name}? It will have to pair again.`)) return;
            try { await api("nodes/" + encodeURIComponent(n.name), { method: "DELETE" }); toast("Revoked " + n.name); refresh(); } catch (err) { fail(err); }
          } }, "Revoke"))))));
      };
      const newCode = async () => {
        try {
          const j = await api("nodes/pair", { method: "POST", body: {} });
          const server = base.href.replace(/\/$/, "");
          modal(h("h2", {}, "Pair a device"),
            h("p", {}, "Code ", h("code", {}, j.code), ", works once until ", when(j.expires), "."),
            h("p", { class: "small" }, "On a computer, run:"),
            h("pre", {}, `hk node run -server ${server} -code ${j.code} -name NAME`),
            h("p", { class: "small" }, "On a phone, open this link (or run `hk node pair` on the server for a QR code):"),
            h("pre", {}, new URL("pair.html#" + j.code, location.href).href),
            h("div", { class: "row end" }, h("button", { type: "button", onclick: closeModal }, "Done")));
        } catch (err) { fail(err); }
      };
      const mine = browserPanel(async () => (await api("nodes/pair", { method: "POST", body: {} })).code, false);
      onLeave(mine.done);
      view.append(list, mine.box);
      every(5000, refresh);
    };
  }

  // pair.html: pairing on a device that has no dashboard token.
  const root = document.getElementById("pairRoot");
  if (root) {
    const code = decodeURIComponent(location.hash.replace(/^#/, ""));
    const getCode = () => Promise.resolve(code);
    getCode.initial = code;
    root.append(browserPanel(getCode, true).box);
  }

  if (load()) start();
})();
