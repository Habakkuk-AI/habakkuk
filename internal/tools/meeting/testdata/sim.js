// A stand-in for a meeting platform's web client, for Habakkuk's tests. A
// page defines window.SIM and one <template> per stage (launcher, prejoin,
// waiting, incall, denied, ended, left, invalid) plus the pieces the call
// adds (captions-container, caption-item, chat-panel, menus). What Habakkuk
// does that matters is posted back to the test server: /joined (the name
// and the microphone and camera), /chat (messages sent), /left.
(function () {
  const S = window.SIM;
  const app = document.getElementById('app');
  const params = new URLSearchParams(location.search);
  const devices = {mic: true, cam: true};
  let captionsOn = false;

  function clone(id) { return document.getElementById(id).content.cloneNode(true); }
  function render(name) {
    app.innerHTML = '';
    app.appendChild(clone(name));
    document.title = S.title + ' - ' + name;
  }
  function report(path, data) {
    fetch(path + '?p=' + S.title, {method: 'POST', body: JSON.stringify(data || {})}).catch(() => {});
  }
  function swap(el) {
    const alt = el.dataset.alt;
    if (!alt) return;
    if (el.hasAttribute('aria-label')) { el.dataset.alt = el.getAttribute('aria-label'); el.setAttribute('aria-label', alt); }
    else { el.dataset.alt = el.textContent; el.textContent = alt; }
  }
  function into(slotID, tplID) {
    const slot = document.getElementById(slotID);
    if (slot && !slot.firstElementChild) slot.appendChild(clone(tplID));
  }

  // Speech: each utterance grows word by word in a caption block, with one
  // misheard word corrected on the next update; at most two blocks show.
  const script = [
    {s: 'Alice Chen', t: "Good morning everyone, let's review the budget for the launch.", typo: [6, 'budjet']},
    {s: 'Bob Ortiz', t: 'We decided to move the launch to March fifteenth.'},
    {s: 'Alice Chen', t: 'Bob will send the revised plan by Habakkuk.'},
  ];
  function speak() {
    const items = document.querySelector('[data-items]');
    let u = 0;
    function next() {
      if (u >= script.length) { setTimeout(() => { if (!params.has('stay')) render('ended'); }, S.endAfter || 1500); return; }
      const line = script[u++], words = line.t.split(' ');
      const item = clone('caption-item').firstElementChild;
      const slot = sel => item.matches(sel) ? item : item.querySelector(sel);
      const who = slot('[data-speaker-slot]'), text = slot('[data-text-slot]');
      if (who) who.textContent = line.s;
      items.appendChild(item);
      while (items.children.length > 2) items.removeChild(items.firstElementChild);
      let w = 0;
      const grow = setInterval(() => {
        w++;
        let shown = words.slice(0, w);
        if (line.typo && w === line.typo[0] + 1) shown[line.typo[0]] = line.typo[1];
        text.textContent = shown.join(' ');
        if (w >= words.length) { clearInterval(grow); setTimeout(next, 300); }
      }, 80);
    }
    next();
  }

  document.addEventListener('click', ev => {
    const el = ev.target.closest('[data-go],[data-act],[data-dev]');
    if (!el) return;
    if (el.dataset.dev) {
      devices[el.dataset.dev] = !devices[el.dataset.dev];
      if (el.getAttribute('role') === 'switch') el.setAttribute('aria-checked', String(devices[el.dataset.dev]));
      else swap(el);
      return;
    }
    const act = el.dataset.act, go = el.dataset.go;
    if (act === 'join') {
      const name = document.querySelector('[data-name]');
      report('/joined', {name: name ? name.value : '', mic: devices.mic, cam: devices.cam});
      render('waiting');
      setTimeout(() => render(params.has('deny') ? 'denied' : 'incall'), S.lobbyMs || 600);
    } else if (act === 'menu') {
      into('menu-slot', 'more-menu');
    } else if (act === 'submenu') {
      into('submenu-slot', 'captions-menu');
    } else if (act === 'captions' && !captionsOn) {
      captionsOn = true;
      for (const b of document.querySelectorAll('[data-act="captions"]')) swap(b);
      into('captions-slot', 'captions-container');
      const menu = document.getElementById('menu-slot');
      if (menu) menu.innerHTML = '';
      speak();
    } else if (act === 'chat') {
      into('chat-slot', 'chat-panel');
    } else if (act === 'leave') {
      if (S.confirmLeave && !document.getElementById('confirm-leave')) {
        const b = document.createElement('button');
        b.id = 'confirm-leave'; b.textContent = 'Leave Meeting'; b.dataset.go = 'left';
        app.appendChild(b);
      } else { report('/left'); render('left'); }
    } else if (go) {
      if (go === 'left') report('/left');
      render(go);
    }
  });

  document.addEventListener('keydown', ev => {
    const f = ev.target.closest && ev.target.closest('[data-chat]');
    if (!f || ev.key !== 'Enter') return;
    ev.preventDefault();
    const text = ('value' in f && f.tagName !== 'DIV') ? f.value : f.innerText;
    report('/chat', {text: text.trim()});
    const log = document.querySelector('[data-chat-log]');
    if (log) { const p = document.createElement('p'); p.textContent = text.trim(); log.appendChild(p); }
    if ('value' in f && f.tagName !== 'DIV') f.value = ''; else f.textContent = '';
  });

  render(params.get('stage') || S.start || 'prejoin');
})();
