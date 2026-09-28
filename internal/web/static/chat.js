// Streaming chat client (fetch + ReadableStream over SSE).
(function () {
  "use strict";
  var form = document.getElementById("chat-form");
  if (!form) return;
  var box = document.getElementById("messages");
  var text = document.getElementById("chat-text");
  var fileInput = document.getElementById("chat-file");
  var fileName = document.getElementById("chat-file-name");
  var provider = document.getElementById("provider");
  var sendBtn = document.getElementById("chat-send");
  var busy = false;

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

  function scroll() { box.scrollTop = box.scrollHeight; }

  function bubble(role) {
    var msg = document.createElement("div");
    msg.className = "msg " + role;
    var b = document.createElement("div");
    b.className = "bubble";
    msg.appendChild(b);
    box.appendChild(msg);
    var intro = box.querySelector(".intro");
    if (intro) intro.remove();
    return b;
  }

  fileInput.addEventListener("change", function () {
    fileName.textContent = fileInput.files.length ? "📎 " + fileInput.files[0].name : "";
  });

  text.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); form.requestSubmit(); }
  });

  form.addEventListener("submit", function (e) {
    e.preventDefault();
    if (busy) return;
    var msg = text.value.trim();
    if (!msg && !fileInput.files.length) return;
    var fd = new FormData();
    fd.append("message", msg);
    fd.append("provider", provider.value);
    if (fileInput.files.length) fd.append("file", fileInput.files[0]);

    var ub = bubble("user");
    ub.textContent = msg + (fileInput.files.length ? "\n📎 " + fileInput.files[0].name : "");
    text.value = ""; fileInput.value = ""; fileName.textContent = "";

    var ab = bubble("assistant");
    var ctxEl = document.createElement("div"); ctxEl.className = "ctx"; ctxEl.hidden = true;
    var councilEl = document.createElement("details"); councilEl.className = "council"; councilEl.hidden = true;
    var councilSum = document.createElement("summary"); councilEl.appendChild(councilSum);
    var councilCount = 0;
    var toolsEl = document.createElement("div");
    var body = document.createElement("div"); body.className = "cursor";
    var usage = document.createElement("div"); usage.className = "usage";
    ab.append(ctxEl, councilEl, toolsEl, body, usage);
    scroll();

    busy = true; sendBtn.disabled = true;
    var acc = "";
    var pending = false;
    function render() {
      if (pending) return;
      pending = true;
      requestAnimationFrame(function () { pending = false; body.innerHTML = md(acc); scroll(); });
    }
    function handle(event, data) {
      var d;
      try { d = JSON.parse(data); } catch (err) { return; }
      switch (event) {
        case "token": acc += d.t; render(); break;
        case "context":
          ctxEl.hidden = false;
          ctxEl.innerHTML = "🔎 contexto: " + d.map(function (n) { return '<a href="/?focus=' + n.id + '">' + esc(n.title) + "</a>"; }).join(" ");
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
          scroll(); break;
        case "tool_call":
          var t = document.createElement("div"); t.className = "toolev";
          t.textContent = "🔧 " + d.name + "(" + (d.arguments || "").slice(0, 160) + ")";
          toolsEl.appendChild(t); scroll(); break;
        case "tool_result":
          var r = document.createElement("div"); r.className = "toolev";
          r.textContent = "  ↳ " + (d.result || "").slice(0, 160);
          toolsEl.appendChild(r); break;
        case "done":
          usage.textContent = (d.provider || "") + " · " + (d.model || "") + " · " + d.input_tokens + "→" + d.output_tokens + " tokens";
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
        busy = false; sendBtn.disabled = false;
        body.classList.remove("cursor");
        if (!acc) body.textContent = "(sem resposta)";
        text.focus();
      });
  });

  scroll();
})();
