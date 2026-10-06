// SPDX-License-Identifier: MIT
//
// The thin kiosk menu, injected by the broker into the browser UI. It lives in
// a shadow root so neither page's styles leak into the other.
(() => {
  if (window.top !== window || document.getElementById("kiosk-overlay")) return;

  const host = document.createElement("div");
  host.id = "kiosk-overlay";
  host.style.cssText = "position:fixed;top:0;left:50%;transform:translateX(-50%);z-index:2147483647";
  // Keep keystrokes and clicks in the menu away from the remote browser.
  for (const ev of ["keydown", "keyup", "keypress", "mousedown", "mouseup", "pointerdown", "pointerup", "wheel", "contextmenu"]) {
    host.addEventListener(ev, (e) => e.stopPropagation());
  }
  const root = host.attachShadow({ mode: "open" });
  root.innerHTML = `
<style>
  * { box-sizing: border-box; font: 14px/1.4 system-ui, sans-serif; }
  .tab { display:block; margin:0 auto; padding:2px 14px; border:0; border-radius:0 0 8px 8px;
         background:rgba(30,30,30,.75); color:#fff; cursor:pointer; opacity:.6; }
  .tab:hover, .open .tab { opacity:1; }
  .panel { display:none; margin-top:4px; padding:12px 14px; width:min(320px, calc(100vw - 32px));
           background:#1f1f21; color:#eee; border-radius:10px; box-shadow:0 6px 24px rgba(0,0,0,.4); }
  .open .panel { display:block; }
  label { display:block; margin-bottom:4px; color:#bbb; }
  select, button.act { width:100%; padding:6px 8px; border-radius:6px; border:1px solid #444; background:#2a2a2d; color:#eee; }
  button.act { margin-top:12px; cursor:pointer; }
  button.danger { background:#8c1d18; border-color:#8c1d18; color:#fff; }
  .hint { margin-top:6px; color:#999; font-size:12px; }
  .cover { position:fixed; inset:0; display:grid; place-items:center; background:#18181a; color:#eee; text-align:center; padding:16px; }
  .cover a { color:#9ecaff; }
</style>
<div class="wrap">
  <button class="tab" title="Menu kiosk">kiosk ▾</button>
  <div class="panel">
    <label for="stall">Tieni acceso dopo l'ultimo download</label>
    <select id="stall"></select>
    <div class="hint">Se nessuno lo guarda e nessun download si muove per questo tempo, il browser viene distrutto. I file scaricati restano sul NAS.</div>
    <button class="act danger" id="done">Fatto, distruggi</button>
  </div>
</div>`;

  const wrap = root.querySelector(".wrap");
  const stall = root.getElementById("stall");
  const done = root.getElementById("done");
  root.querySelector(".tab").onclick = () => wrap.classList.toggle("open");

  const label = (m) => (m < 60 ? `${m} minuti` : m === 60 ? "1 ora" : `${m / 60} ore`);
  fetch("/_kiosk/settings").then((r) => r.json()).then((s) => {
    const choices = s.choices.includes(s.stall_minutes) ? s.choices : [s.stall_minutes, ...s.choices];
    for (const m of choices) stall.add(new Option(label(m), m, false, m === s.stall_minutes));
  });
  stall.onchange = () =>
    fetch("/_kiosk/settings", { method: "POST", body: new URLSearchParams({ stall_minutes: stall.value }) });

  // Two clicks, not a confirm(): a modal dialog would freeze the page.
  let armed = false;
  done.onclick = async () => {
    if (!armed) {
      armed = true;
      done.textContent = "Sicuro? Clicca di nuovo per distruggere";
      setTimeout(() => { armed = false; done.textContent = "Fatto, distruggi"; }, 4000);
      return;
    }
    const r = await fetch("/_kiosk/done", { method: "POST" });
    if (!r.ok) { done.textContent = "Errore, riprova"; armed = false; return; }
    host.style.cssText = "position:fixed;inset:0;z-index:2147483647";
    root.querySelector(".wrap").outerHTML = `<div class="cover"><div>
      <h2>Browser distrutto</h2>
      <p>Profilo e cookie sono spariti. I file scaricati restano sul NAS.</p>
      <p><a href="/">Apri un browser nuovo</a></p></div></div>`;
  };

  document.body.appendChild(host);
})();
