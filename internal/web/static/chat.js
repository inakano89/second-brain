// Chat client: one tab per conversation, streaming over fetch + ReadableStream (SSE).
// Every tab keeps its own pane, so an answer still being written keeps streaming while you
// work in another tab.
(function () {
  "use strict";
  var form = document.getElementById("chat-form");
  if (!form) return;
  var tabsEl = document.getElementById("chat-tabs");
  var panes = document.getElementById("panes");
  var text = document.getElementById("chat-text");
  var fileInput = document.getElementById("chat-file");
  var fileName = document.getElementById("chat-file-name");
  var provider = document.getElementById("provider");
  var sendBtn = document.getElementById("chat-send");
  var personaLine = document.getElementById("chat-persona");
  var addBtn = document.getElementById("chat-new");
  var dlg = document.getElementById("new-chat");
  var dlgForm = document.getElementById("new-chat-form");
  var dlgErr = document.getElementById("new-chat-err");

  var chats = {};   // id -> {id, btn, pane, busy, draft}
  var active = 0;

  try {
    var saved = localStorage.getItem("sb.provider");
    if (saved !== null && provider.querySelector('option[value="' + CSS.escape(saved) + '"]')) provider.value = saved;
  } catch (e) {}
  provider.addEventListener("change", function () {
    try { localStorage.setItem("sb.provider", provider.value); } catch (e) {}
  });

  function esc(s) {
    return s.replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; });
  }

  function inline(s) {
    var codes = [];
    s = s.replace(/`([^`\n]+)`/g, function (_, c) { codes.push("<code>" + c + "</code>"); return "\u0000" + (codes.length - 1) + "\u0000"; });
    s = s.replace(/\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]/g, function (_, t, l) { return '<a class="wikilink" href="/?q=' + encodeURIComponent(t) + '">' + (l || t) + "</a>"; });
    s = s.replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
    s = s.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/(^|[\s(])[*_]([^*_\n]+)[*_]/g, "$1<em>$2</em>");
    return s.replace(/\u0000(\d+)\u0000/g, function (_, i) { return codes[Number(i)]; });
  }

  function md(src) {
    var out = [], inCode = false, list = null, para = [];
    function flush() { if (para.length) { out.push("<p>" + para.join("<br>") + "</p>"); para = []; } }
    function closeList() { if (list) { out.push("</" + list + ">"); list = null; } }
    src.split("\n").forEach(function (raw) {
      var line = raw.trim();
      if (line.indexOf("```") === 0) { flush(); closeList(); out.push(inCode ? "</code></pre>" : "<pre><code>"); inCode = !inCode; return; }
      if (inCode) { out.push(esc(raw) + "\n"); return; }
      var e = esc(line), m;
      if (!line) { flush(); closeList(); }
      else if ((m = line.match(/^(#{1,6})\s+(.*)$/))) { flush(); closeList(); var l = Math.min(6, m[1].length + 2); out.push("<h" + l + ">" + inline(esc(m[2])) + "</h" + l + ">"); }
      else if (/^[-*•]\s+/.test(line)) { flush(); if (list !== "ul") { closeList(); out.push("<ul>"); list = "ul"; } out.push("<li>" + inline(esc(line.replace(/^[-*•]\s+/, ""))) + "</li>"); }
      else if (/^\d+[.)]\s+/.test(line)) { flush(); if (list !== "ol") { closeList(); out.push("<ol>"); list = "ol"; } out.push("<li>" + inline(esc(line.replace(/^\d+[.)]\s+/, ""))) + "</li>"); }
      else if (line[0] === ">") { flush(); closeList(); out.push("<blockquote>" + inline(esc(line.slice(1).trim())) + "</blockquote>"); }
      else { closeList(); para.push(inline(e)); }
    });
    if (inCode) out.push("</code></pre>");
    flush(); closeList();
    return out.join("");
  }

  function scroll(pane) { pane.scrollTop = pane.scrollHeight; }

  function bubble(pane, role) {
    var msg = document.createElement("div");
    msg.className = "msg " + role;
    var b = document.createElement("div");
    b.className = "bubble";
    msg.appendChild(b);
    pane.appendChild(msg);
    var intro = pane.querySelector(".intro");
    if (intro) intro.remove();
    return b;
  }

  function api(path, opts) {
    return fetch(path, Object.assign({ credentials: "same-origin" }, opts || {})).then(function (res) {
      if (!res.ok) return res.text().then(function (t) { throw new Error(t.trim() || ("HTTP " + res.status)); });
      return res;
    });
  }

  // ---- tabs ----
  function register(btn) {
    var id = Number(btn.getAttribute("data-chat"));
    chats[id] = { id: id, btn: btn, pane: null, busy: false, draft: "" };
    return chats[id];
  }

  function setTab(c, t) {
    c.btn.querySelector(".ti").textContent = t.icon;
    c.btn.querySelector(".tt").textContent = t.title;
    c.btn.setAttribute("data-name", t.name);
    c.btn.setAttribute("data-blurb", t.blurb);
    c.btn.title = t.name;
    if (c.id === active) showPersona(c);
  }

  function showPersona(c) {
    var b = document.createElement("b");
    b.textContent = c.btn.querySelector(".ti").textContent + " " + c.btn.getAttribute("data-name");
    personaLine.textContent = " — " + c.btn.getAttribute("data-blurb");
    personaLine.insertBefore(b, personaLine.firstChild);
  }

  function refreshSend() { sendBtn.disabled = !!(chats[active] && chats[active].busy); }

  function setBusy(c, on) {
    c.busy = on;
    c.btn.classList.toggle("busy", on);
    c.btn.querySelector(".tb").hidden = !on && !c.btn.classList.contains("unread");
    if (c.id === active) refreshSend();
  }

  function loadPane(c) {
    if (c.pane) return Promise.resolve();
    var pane = document.createElement("div");
    pane.className = "messages";
    pane.hidden = true;
    pane.setAttribute("data-pane", c.id);
    pane.textContent = "Carregando…";
    panes.appendChild(pane);
    c.pane = pane;
    return api("/api/chats/" + c.id + "/history").then(function (r) { return r.text(); })
      .then(function (html) { pane.innerHTML = html; })
      .catch(function (err) { pane.textContent = "❌ " + err.message; c.pane = null; pane.remove(); });
  }

  function select(id) {
    var c = chats[id];
    if (!c) return;
    var prev = chats[active];
    if (prev) prev.draft = text.value;
    active = id;
    Object.keys(chats).forEach(function (k) {
      var on = Number(k) === id;
      chats[k].btn.classList.toggle("on", on);
      chats[k].btn.setAttribute("aria-selected", on ? "true" : "false");
    });
    c.btn.classList.remove("unread");
    c.btn.querySelector(".tb").hidden = !c.busy;
    showPersona(c);
    text.value = c.draft;
    refreshSend();
    try { history.replaceState(null, "", "/chat?c=" + id); } catch (e) {}
    loadPane(c).then(function () {
      if (active !== id || !c.pane) return;
      Array.prototype.forEach.call(panes.children, function (p) { p.hidden = p !== c.pane; });
      scroll(c.pane);
      text.focus();
    });
  }

  function addTab(t) {
    var b = document.createElement("button");
    b.type = "button"; b.className = "chat-tab"; b.setAttribute("role", "tab"); b.setAttribute("aria-selected", "false");
    b.setAttribute("data-chat", t.id);
    var ti = document.createElement("span"); ti.className = "ti";
    var tt = document.createElement("span"); tt.className = "tt";
    var tb = document.createElement("span"); tb.className = "tb"; tb.hidden = true;
    b.append(ti, tt, tb);
    tabsEl.insertBefore(b, addBtn);
    var c = register(b);
    setTab(c, t);
    return c;
  }

  tabsEl.addEventListener("click", function (e) {
    var b = e.target.closest(".chat-tab");
    if (!b) return;
    if (b === addBtn) { openDialog(); return; }
    select(Number(b.getAttribute("data-chat")));
  });

  Array.prototype.forEach.call(tabsEl.querySelectorAll(".chat-tab[data-chat]"), function (b) {
    var c = register(b);
    if (b.classList.contains("on")) active = c.id;
  });
  (function () {
    var first = panes.querySelector("[data-pane]");
    if (first && chats[active]) chats[active].pane = first;
  })();

  // ---- new chat ----
  function openDialog() {
    dlgErr.hidden = true;
    if (typeof dlg.showModal === "function") dlg.showModal(); else dlg.setAttribute("open", "");
  }
  function closeDialog() { if (typeof dlg.close === "function") dlg.close(); else dlg.removeAttribute("open"); }
  document.getElementById("new-chat-cancel").addEventListener("click", closeDialog);
  dlgForm.addEventListener("submit", function (e) {
    e.preventDefault();
    api("/api/chats", { method: "POST", body: new FormData(dlgForm) })
      .then(function (r) { return r.json(); })
      .then(function (t) {
        dlgForm.reset();
        closeDialog();
        var c = addTab(t);
        select(c.id);
        c.btn.scrollIntoView({ block: "nearest", inline: "nearest" });
      })
      .catch(function (err) { dlgErr.textContent = err.message; dlgErr.hidden = false; });
  });

  // ---- rename / clear / delete ----
  document.getElementById("chat-rename").addEventListener("click", function () {
    var c = chats[active];
    if (!c) return;
    var name = window.prompt("Nome deste chat (vazio = automático):", c.btn.querySelector(".tt").textContent);
    if (name === null) return;
    var fd = new FormData(); fd.append("title", name);
    api("/api/chats/" + c.id, { method: "POST", body: fd }).then(function (r) { return r.json(); })
      .then(function (t) { setTab(c, t); })
      .catch(function (err) { window.alert(err.message); });
  });

  document.getElementById("chat-clear").addEventListener("click", function () {
    var c = chats[active];
    if (!c) return;
    if (c.busy) { window.alert("Aguarde a resposta terminar."); return; }
    if (!window.confirm("Apagar todas as mensagens deste chat? O chat e a persona continuam.")) return;
    api("/api/chats/" + c.id + "/clear", { method: "POST" })
      .then(function () { return api("/api/chats/" + c.id + "/history"); })
      .then(function (r) { return r.text(); })
      .then(function (html) { c.pane.innerHTML = html; })
      .catch(function (err) { window.alert(err.message); });
  });

  document.getElementById("chat-delete").addEventListener("click", function () {
    var c = chats[active];
    if (!c) return;
    if (c.busy) { window.alert("Aguarde a resposta terminar."); return; }
    if (!window.confirm("Excluir este chat e todas as suas mensagens? Não dá para desfazer.")) return;
    api("/api/chats/" + c.id + "/delete", { method: "POST" })
      .then(function () {
        var ids = Object.keys(chats).map(Number), i = ids.indexOf(c.id);
        var next = ids[i + 1] || ids[i - 1];
        c.btn.remove();
        if (c.pane) c.pane.remove();
        delete chats[c.id];
        active = 0;
        if (next) select(next); else window.location.href = "/chat";
      })
      .catch(function (err) { window.alert(err.message); });
  });

  // ---- composer ----
  fileInput.addEventListener("change", function () {
    fileName.textContent = fileInput.files.length ? "📎 " + fileInput.files[0].name : "";
  });

  text.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); form.requestSubmit(); }
  });

  form.addEventListener("submit", function (e) {
    e.preventDefault();
    var c = chats[active];
    if (!c || c.busy || !c.pane) return;
    var msg = text.value.trim();
    if (!msg && !fileInput.files.length) return;
    var fd = new FormData();
    fd.append("chat", c.id);
    fd.append("message", msg);
    fd.append("provider", provider.value);
    if (fileInput.files.length) fd.append("file", fileInput.files[0]);

    var pane = c.pane;
    var ub = bubble(pane, "user");
    ub.textContent = msg + (fileInput.files.length ? "\n📎 " + fileInput.files[0].name : "");
    text.value = ""; c.draft = ""; fileInput.value = ""; fileName.textContent = "";

    var ab = bubble(pane, "assistant");
    var ctxEl = document.createElement("div"); ctxEl.className = "ctx"; ctxEl.hidden = true;
    var councilEl = document.createElement("details"); councilEl.className = "council"; councilEl.hidden = true;
    var councilSum = document.createElement("summary"); councilEl.appendChild(councilSum);
    var councilCount = 0;
    var toolsEl = document.createElement("div");
    var body = document.createElement("div"); body.className = "cursor";
    var usage = document.createElement("div"); usage.className = "usage";
    ab.append(ctxEl, councilEl, toolsEl, body, usage);
    scroll(pane);

    setBusy(c, true);
    var acc = "";
    var pending = false;
    function render() {
      if (pending) return;
      pending = true;
      requestAnimationFrame(function () { pending = false; body.innerHTML = md(acc); scroll(pane); });
    }
    function handle(event, data) {
      var d;
      try { d = JSON.parse(data); } catch (err) { return; }
      switch (event) {
        case "token": acc += d.t; render(); break;
        case "context":
          ctxEl.hidden = false;
          ctxEl.innerHTML = "🔎 fontes: " + d.map(function (n) { return '<a href="/?focus=' + n.id + '">' + esc(n.title) + "</a>"; }).join(" ");
          break;
        case "council_start":
          councilEl.hidden = false;
          councilSum.textContent = "🤝 Conselho deliberando: " + (d.members || []).join(" · ") + " → moderador " + (d.judge || "");
          break;
        case "council":
          councilCount++;
          var op = document.createElement("div"); op.className = "opinion" + (d.error ? " failed" : "");
          var h = document.createElement("div"); h.className = "op-head";
          h.textContent = (d.round === 0 ? "Resposta inicial" : "Revisão " + d.round) + " · " + d.label + " — " + d.spec;
          var b = document.createElement("div"); b.className = "op-body";
          if (d.error) b.textContent = "❌ " + d.error; else b.innerHTML = md(d.text || "");
          op.append(h, b); councilEl.appendChild(op);
          councilSum.textContent = councilSum.textContent.replace(/ \(\d+ respostas\)$/, "") + " (" + councilCount + " respostas)";
          scroll(pane); break;
        case "tool_call":
          var t = document.createElement("div"); t.className = "toolev";
          t.textContent = "🔧 " + d.name + "(" + (d.arguments || "").slice(0, 160) + ")";
          toolsEl.appendChild(t); scroll(pane); break;
        case "tool_result":
          var r = document.createElement("div"); r.className = "toolev";
          r.textContent = "  ↳ " + (d.result || "").slice(0, 160);
          toolsEl.appendChild(r); break;
        case "done":
          usage.textContent = (d.provider || "") + " · " + (d.model || "") + " · " + d.input_tokens + "→" + d.output_tokens + " tokens";
          if (d.title && chats[c.id]) c.btn.querySelector(".tt").textContent = d.title;
          break;
        case "error":
          acc += "\n\n❌ " + d.message; render(); break;
      }
    }

    fetch("/api/chat", { method: "POST", body: fd, credentials: "same-origin" })
      .then(function (res) {
        if (!res.ok || !res.body) return res.text().then(function (t) { throw new Error(t || ("HTTP " + res.status)); });
        var reader = res.body.getReader();
        var dec = new TextDecoder();
        var buf = "";
        function pump() {
          return reader.read().then(function (r) {
            if (r.done) return;
            buf += dec.decode(r.value, { stream: true });
            var idx;
            while ((idx = buf.indexOf("\n\n")) >= 0) {
              var chunk = buf.slice(0, idx); buf = buf.slice(idx + 2);
              var ev = "message", data = [];
              chunk.split("\n").forEach(function (l) {
                if (l.indexOf("event:") === 0) ev = l.slice(6).trim();
                else if (l.indexOf("data:") === 0) data.push(l.slice(5).trim());
              });
              handle(ev, data.join("\n"));
            }
            return pump();
          });
        }
        return pump();
      })
      .catch(function (err) { acc += "\n\n❌ " + err.message; render(); })
      .finally(function () {
        body.classList.remove("cursor");
        if (!acc) body.textContent = "(sem resposta)";
        if (!chats[c.id]) return; // chat deleted meanwhile
        setBusy(c, false);
        if (active === c.id) text.focus();
        else { c.btn.classList.add("unread"); c.btn.querySelector(".tb").hidden = false; }
      });
  });

  if (chats[active] && chats[active].pane) scroll(chats[active].pane);
})();
