// Overview ("orbits"): the brain at the centre, then routines, node types, main
// topics and the sources everything comes from, as concentric rings (SVG, no deps).
// Links appear only on hover/selection; a click lists what is inside.
(function () {
  "use strict";
  var root = document.getElementById("orbit");
  if (!root) return;
  var NS = "http://www.w3.org/2000/svg";
  var tip = document.getElementById("o-tip");
  var legend = document.getElementById("o-legend");
  var wrap = document.getElementById("graph-wrap");
  var net = document.getElementById("network");
  var detail = document.getElementById("detail");
  var tabs = document.querySelectorAll(".view-switch [data-view]");
  var emptyDetail = detail ? detail.innerHTML : "";
  var data = null, pos = {}, selected = null, linksG = null, itemsByKey = {};
  var typeLabel = {};

  function el(name, attrs, parent) {
    var e = document.createElementNS(NS, name);
    for (var k in attrs) if (attrs[k] !== undefined && attrs[k] !== null) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }
  function trunc(s, n) { s = String(s || ""); return s.length > n ? s.slice(0, n - 1) + "…" : s; }
  function fmt(n) { return (n || 0).toLocaleString("pt-BR"); }
  function when(iso) {
    if (!iso || iso.indexOf("0001-") === 0) return "—";
    var d = new Date(iso);
    return d.toLocaleDateString("pt-BR", { day: "2-digit", month: "2-digit" }) + " " + d.toLocaleTimeString("pt-BR", { hour: "2-digit", minute: "2-digit" });
  }
  function sortedTypes(types) {
    return Object.keys(types || {}).map(function (t) { return [t, types[t]]; }).sort(function (a, b) { return b[1] - a[1]; });
  }

  function load() {
    return fetch("/api/overview", { credentials: "same-origin" })
      .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); })
      .then(function (d) {
        data = d;
        (d.types || []).forEach(function (t) { typeLabel[t.key] = t.label; });
        render();
      })
      .catch(function (err) { root.textContent = "Não foi possível carregar a visão geral: " + err.message; });
  }

  // arc draws a ring leaving a gap at the top for its title.
  function arc(cx, cy, r, gap) {
    var a0 = -Math.PI / 2 + gap, a1 = 1.5 * Math.PI - gap;
    return "M" + (cx + r * Math.cos(a0)) + " " + (cy + r * Math.sin(a0)) +
      " A" + r + " " + r + " 0 1 1 " + (cx + r * Math.cos(a1)) + " " + (cy + r * Math.sin(a1));
  }

  // place spreads n items over the ring, outside the title gap.
  function place(n, gap) {
    var span = 2 * Math.PI - 2 * gap, out = [];
    for (var i = 0; i < n; i++) out.push(-Math.PI / 2 + gap + (i + 0.5) * span / n);
    return out;
  }

  function ring(svg, cx, cy, r, title, cls) {
    var gap = (title.length * 8 / 2 + 10) / r;
    el("path", { d: arc(cx, cy, r, gap), class: "ring " + (cls || "") }, svg);
    var t = el("text", { x: cx, y: cy - r, class: "ring-title", "dominant-baseline": "middle" }, svg);
    t.textContent = title;
    return gap;
  }

  function item(parent, key, cls, x, y, label, delay) {
    var g = el("g", { class: "item " + cls, tabindex: 0, role: "button", "aria-label": label, "data-key": key }, parent);
    g.style.animationDelay = delay + "ms";
    g.style.transformOrigin = x + "px " + y + "px";
    pos[key] = { x: x, y: y };
    return g;
  }

  function render() {
    if (!data || root.hidden) return;
    var box = root.getBoundingClientRect();
    var w = box.width, h = box.height;
    if (w < 80 || h < 80) return;
    root.textContent = "";
    pos = {}; itemsByKey = {};
    var svg = el("svg", { viewBox: "0 0 " + w + " " + h, "aria-hidden": "false" }, root);
    var defs = el("defs", {}, svg);
    var grad = el("radialGradient", { id: "o-core", cx: "35%", cy: "30%", r: "75%" }, defs);
    el("stop", { offset: "0%", style: "stop-color: var(--o-core); stop-opacity: .95" }, grad);
    el("stop", { offset: "100%", style: "stop-color: var(--o-core); stop-opacity: .55" }, grad);

    var cx = w / 2, cy = h / 2 + 14;
    var R = Math.min(w, h - 50) / 2 - 24;
    var small = R < 240;
    var rc = R * 0.13, rr = R * 0.2, rt = R * 0.37, rg = R * 0.61, rs = R * 0.89;

    var gapR = ring(svg, cx, cy, rr, "ROTINAS", "routines");
    var gapT = ring(svg, cx, cy, rt, "TIPOS");
    var gapG = ring(svg, cx, cy, rg, "TEMAS");
    var gapS = ring(svg, cx, cy, rs, "FONTES");
    linksG = el("g", { class: "links" }, svg);
    var items = el("g", {}, svg);
    var delay = 0, count = (data.routines || []).length + (data.types || []).length + (data.topics || []).length + (data.sources || []).length;
    var step = Math.min(20, 400 / Math.max(1, count)); // whole entrance under ~0.9 s

    // centre
    var core = el("g", { class: "item core", tabindex: 0, role: "button", "aria-label": (data.brain || "Second Brain") + ": " + fmt(data.total) + " itens", "data-key": "core" }, items);
    el("circle", { cx: cx, cy: cy, r: rc, class: "core-disc mark" }, core);
    var name = el("text", { x: cx, y: cy - 3, "font-size": small ? 11 : 12.5, "font-weight": 700 }, core);
    name.textContent = trunc(data.brain || "Second Brain", small ? 10 : 14);
    var tot = el("text", { x: cx, y: cy + 13, "font-size": 10.5, opacity: 0.85 }, core);
    tot.textContent = fmt(data.total) + " itens";
    itemsByKey.core = { kind: "core" };

    // routines: small status dots around the core
    var routines = data.routines || [];
    place(routines.length, gapR).forEach(function (a, i) {
      var rt0 = routines[i], x = cx + rr * Math.cos(a), y = cy + rr * Math.sin(a);
      var state = rt0.running ? "running" : rt0.error ? "err" : rt0.last ? "ok" : "idle";
      var g = item(items, "r:" + rt0.key, "routine " + state, x, y, "Rotina " + rt0.label, delay += step);
      el("circle", { cx: x, cy: y, r: 10, class: "hit" }, g);
      el("circle", { cx: x, cy: y, r: small ? 3.5 : 4.5, class: "mark" }, g);
      itemsByKey["r:" + rt0.key] = { kind: "routine", d: rt0 };
    });

    // node types
    var types = data.types || [];
    var maxT = Math.max.apply(null, types.map(function (t) { return t.count; }).concat([1]));
    place(types.length, gapT).forEach(function (a, i) {
      var t = types[i], x = cx + rt * Math.cos(a), y = cy + rt * Math.sin(a);
      var rad = (small ? 6 : 8) + (small ? 10 : 15) * Math.sqrt(t.count / maxT);
      var g = item(items, "t:" + t.key, "type", x, y, t.label + ": " + fmt(t.count), delay += step);
      el("circle", { cx: x, cy: y, r: rad + 6, class: "hit" }, g);
      var c = el("circle", { cx: x, cy: y, r: rad, class: "mark" }, g);
      c.style.fill = t.color;
      if (!t.count) g.setAttribute("opacity", 0.45);
      var lab = el("text", { x: x, y: y + rad + 13, "text-anchor": "middle" }, g);
      lab.textContent = t.label;
      var cnt = el("text", { x: x, y: y + rad + 25, "text-anchor": "middle", class: "count" }, g);
      cnt.textContent = fmt(t.count);
      itemsByKey["t:" + t.key] = { kind: "type", d: t };
    });

    // topics: pills on the ring
    var fit = Math.max(4, Math.min(12, Math.floor(2 * Math.PI * rg / 108))); // pills that fit the ring
    var topics = (data.topics || []).slice(0, fit);
    var maxG = Math.max.apply(null, topics.map(function (t) { return t.count; }).concat([1]));
    place(topics.length, gapG).forEach(function (a, i) {
      var t = topics[i], x = cx + rg * Math.cos(a), y = cy + rg * Math.sin(a);
      var label = trunc(t.label, R < 300 ? 13 : 16);
      var fs = 11 + 2 * Math.sqrt(t.count / maxG);
      var pw = label.length * fs * 0.58 + 26, ph = fs + 11;
      var g = item(items, "g:" + t.key, "topic", x, y, "Tema " + t.label + ": " + fmt(t.count), delay += step);
      var rect = el("rect", { x: x - pw / 2, y: y - ph / 2, width: pw, height: ph, rx: ph / 2, class: "mark hex" }, g);
      rect.style.stroke = t.color;
      rect.style.strokeWidth = 1.5;
      rect.style.fill = "var(--panel)";
      var dot = el("circle", { cx: x - pw / 2 + 10, cy: y, r: 3.5 }, g);
      dot.style.fill = t.color;
      var lab = el("text", { x: x + 4, y: y, "text-anchor": "middle", "dominant-baseline": "central", "font-size": fs.toFixed(1) }, g);
      lab.style.stroke = "none";
      lab.textContent = label;
      itemsByKey["g:" + t.key] = { kind: "topic", d: t };
    });

    // sources: hexagons with an icon
    var sources = data.sources || [];
    var hr = small ? 14 : 18;
    place(sources.length, gapS).forEach(function (a, i) {
      var s = sources[i], x = cx + rs * Math.cos(a), y = cy + rs * Math.sin(a);
      var g = item(items, "s:" + s.key, "source", x, y, s.label + ": " + fmt(s.count), delay += step);
      var pts = [];
      for (var k = 0; k < 6; k++) {
        var ang = Math.PI / 6 + k * Math.PI / 3;
        pts.push((x + hr * Math.cos(ang)).toFixed(1) + "," + (y + hr * Math.sin(ang)).toFixed(1));
      }
      el("polygon", { points: pts.join(" "), class: "hex" }, g);
      var ic = el("text", { x: x, y: y + 1, "text-anchor": "middle", "dominant-baseline": "central", class: "emoji" }, g);
      ic.textContent = s.icon || "•";
      var below = Math.sin(a) > -0.2; // labels outward at the bottom, inward at the top
      var ly = below ? y + hr + 13 : y - hr - 17;
      var lab = el("text", { x: x, y: ly, "text-anchor": "middle" }, g);
      lab.textContent = trunc(s.label, 16);
      var cnt = el("text", { x: x, y: ly + 12, "text-anchor": "middle", class: "count" }, g);
      cnt.textContent = fmt(s.count);
      itemsByKey["s:" + s.key] = { kind: "source", d: s };
    });

    if (legend) {
      legend.innerHTML = "";
      [["", "Tamanho = quantidade"], ["ok", "rotina ok"], ["err", "rotina com erro"], ["idle", "rotina ainda não rodou"]].forEach(function (p) {
        var span = document.createElement("span");
        if (p[0]) { var i = document.createElement("i"); i.className = p[0]; span.appendChild(i); }
        span.appendChild(document.createTextNode(p[1]));
        legend.appendChild(span);
      });
    }
    if (selected && itemsByKey[selected]) highlight(selected);
    else selected = null;
  }

  // related returns [key, weight, colour] links for an item.
  function related(key) {
    var it = itemsByKey[key], out = [];
    if (!it || !data) return out;
    var d = it.d;
    if (it.kind === "source" || it.kind === "topic") {
      sortedTypes(d.types).forEach(function (p) { out.push(["t:" + p[0], p[1], "var(--t-" + p[0] + ")"]); });
    } else if (it.kind === "type") {
      (data.sources || []).forEach(function (s) { if (s.types && s.types[d.key]) out.push(["s:" + s.key, s.types[d.key], d.color]); });
      (data.topics || []).forEach(function (g) { if (g.types && g.types[d.key]) out.push(["g:" + g.key, g.types[d.key], d.color]); });
    }
    return out.filter(function (l) { return pos[l[0]]; });
  }

  function highlight(key) {
    if (!linksG) return;
    linksG.textContent = "";
    root.classList.remove("dim");
    root.querySelectorAll(".item.lit").forEach(function (n) { n.classList.remove("lit"); });
    if (!key || !pos[key]) return;
    var links = related(key);
    var max = Math.max.apply(null, links.map(function (l) { return l[1]; }).concat([1]));
    var box = root.getBoundingClientRect(), cx = box.width / 2, cy = box.height / 2 + 14;
    var a = pos[key];
    links.forEach(function (l) {
      var b = pos[l[0]];
      var mx = (a.x + b.x) / 2, my = (a.y + b.y) / 2;
      var qx = cx + (mx - cx) * 0.55, qy = cy + (my - cy) * 0.55; // bow towards the centre
      var p = el("path", { d: "M" + a.x + " " + a.y + " Q" + qx + " " + qy + " " + b.x + " " + b.y }, linksG);
      p.style.stroke = l[2];
      p.style.strokeWidth = (1 + 5 * Math.sqrt(l[1] / max)).toFixed(1);
      var n = root.querySelector('[data-key="' + cssEscape(l[0]) + '"]');
      if (n) n.classList.add("lit");
    });
    var self = root.querySelector('[data-key="' + cssEscape(key) + '"]');
    if (self) self.classList.add("lit");
    if (links.length || itemsByKey[key].kind !== "core") root.classList.add("dim");
  }

  function cssEscape(s) { return window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/["\\]/g, "\\$&"); }

  function row(parent, color, text, value) {
    var r = document.createElement("div");
    r.className = "t-row";
    if (color) { var d = document.createElement("span"); d.className = "dot"; d.style.setProperty("--c", color); r.appendChild(d); }
    r.appendChild(document.createTextNode(text));
    if (value !== undefined) { var b = document.createElement("b"); b.textContent = value; r.appendChild(b); }
    parent.appendChild(r);
  }

  function showTip(key, x, y) {
    var it = itemsByKey[key];
    if (!it || !tip) return;
    tip.textContent = "";
    var d = it.d;
    var head = document.createElement("strong");
    tip.appendChild(head);
    if (it.kind === "core") {
      head.textContent = data.brain || "Second Brain";
      row(tip, "", "Itens no cérebro", fmt(data.total));
    } else if (it.kind === "routine") {
      head.textContent = d.label;
      row(tip, "", d.running ? "⏳ executando agora" : d.error ? "⚠️ erro: " + trunc(d.error, 90) : d.last ? "✓ última execução: " + when(d.last) : "ainda não rodou desde que o servidor abriu");
      row(tip, "", "Próxima: " + when(d.next) + " (" + d.spec + ")");
    } else {
      head.textContent = (d.icon ? d.icon + " " : "") + d.label + " — " + fmt(d.count);
      if (it.kind === "type") {
        (data.sources || []).filter(function (s) { return s.types && s.types[d.key]; })
          .sort(function (p, q) { return q.types[d.key] - p.types[d.key]; }).slice(0, 4)
          .forEach(function (s) { row(tip, "", (s.icon || "•") + " " + s.label, fmt(s.types[d.key])); });
      } else {
        sortedTypes(d.types).slice(0, 5).forEach(function (p) { row(tip, "var(--t-" + p[0] + ")", typeLabel[p[0]] || p[0], fmt(p[1])); });
      }
      var hint = document.createElement("div");
      hint.className = "muted small";
      hint.textContent = "Clique para ver os itens";
      tip.appendChild(hint);
    }
    tip.hidden = false;
    var wb = wrap.getBoundingClientRect();
    var left = Math.min(x + 14, wb.width - tip.offsetWidth - 8), top = Math.min(y + 14, wb.height - tip.offsetHeight - 8);
    tip.style.left = Math.max(8, left) + "px";
    tip.style.top = Math.max(8, top) + "px";
  }

  function hideTip() { if (tip) tip.hidden = true; }

  function open(key) {
    var it = itemsByKey[key];
    if (!it) return;
    root.querySelectorAll(".item.sel").forEach(function (n) { n.classList.remove("sel"); });
    if (it.kind === "core") {
      selected = null;
      highlight(null);
      if (detail) detail.innerHTML = emptyDetail;
      return;
    }
    selected = key;
    var node = root.querySelector('[data-key="' + cssEscape(key) + '"]');
    if (node) node.classList.add("sel");
    highlight(key);
    var d = it.d;
    if (it.kind === "routine") { routinePanel(d); return; }
    var p = new URLSearchParams({ label: d.label, count: d.count });
    if (it.kind === "type") p.set("types", d.key);
    if (it.kind === "topic") p.set("tag", d.key);
    if (it.kind === "source") { p.set("source", (d.sources || [d.key]).join(",")); p.set("icon", d.icon || ""); }
    if (window.htmx) window.htmx.ajax("GET", "/overview/nodes?" + p.toString(), { target: "#detail", swap: "innerHTML" });
  }

  function routinePanel(d) {
    if (!detail) return;
    detail.textContent = "";
    var h = document.createElement("h3");
    h.textContent = "⏰ " + d.label;
    detail.appendChild(h);
    var dl = document.createElement("dl");
    dl.className = "update-box";
    [["Situação", d.running ? "executando agora" : d.error ? "erro" : d.last ? "ok" : "ainda não rodou desde que o servidor abriu"],
      ["Última execução", when(d.last)], ["Próxima", when(d.next)], ["Agenda (cron)", d.spec], ["Erro", d.error]].forEach(function (p) {
      if (!p[1]) return;
      var dt = document.createElement("dt"); dt.textContent = p[0];
      var dd = document.createElement("dd"); dd.textContent = p[1];
      dl.appendChild(dt); dl.appendChild(dd);
    });
    detail.appendChild(dl);
    var f = document.createElement("form");
    f.method = "post";
    f.action = "/jobs/" + encodeURIComponent(d.key) + "/run";
    var b = document.createElement("button");
    b.className = "btn small";
    b.textContent = "▶ Executar agora";
    f.appendChild(b);
    detail.appendChild(f);
    var note = document.createElement("p");
    note.className = "muted small";
    note.textContent = "Se o computador estiver desligado no horário, a rotina roda alguns minutos depois de ligar.";
    detail.appendChild(note);
  }

  // pointer & keyboard
  function keyOf(target) { var g = target.closest && target.closest(".item"); return g ? g.getAttribute("data-key") : null; }
  var hoverKey = null;
  root.addEventListener("pointermove", function (ev) {
    var key = keyOf(ev.target);
    var wb = wrap.getBoundingClientRect();
    if (key !== hoverKey) { hoverKey = key; highlight(key || selected); }
    if (key) showTip(key, ev.clientX - wb.left, ev.clientY - wb.top);
    else hideTip();
  });
  root.addEventListener("pointerleave", function () { hoverKey = null; hideTip(); highlight(selected); });
  root.addEventListener("click", function (ev) {
    var key = keyOf(ev.target);
    if (key) open(key);
    else if (selected) { selected = null; root.querySelectorAll(".item.sel").forEach(function (n) { n.classList.remove("sel"); }); highlight(null); }
  });
  root.addEventListener("keydown", function (ev) {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    var key = keyOf(ev.target);
    if (key) { ev.preventDefault(); open(key); }
  });
  root.addEventListener("focusin", function (ev) {
    var key = keyOf(ev.target);
    if (!key || !pos[key]) return;
    var ob = root.getBoundingClientRect(), wb = wrap.getBoundingClientRect();
    highlight(key);
    showTip(key, pos[key].x + ob.left - wb.left, pos[key].y + ob.top - wb.top);
  });
  root.addEventListener("focusout", function () { hideTip(); highlight(selected); });

  // view switch: overview ↔ network
  function setView(v, save) {
    var orbitOn = v !== "network";
    root.hidden = !orbitOn;
    if (legend) legend.hidden = !orbitOn;
    if (net) net.hidden = orbitOn;
    hideTip();
    tabs.forEach(function (b) { b.setAttribute("aria-selected", String(b.getAttribute("data-view") === (orbitOn ? "orbit" : "network"))); });
    if (save) { try { localStorage.setItem("sb.view", orbitOn ? "orbit" : "network"); } catch (e) { /* private mode */ } }
    if (orbitOn) { if (data) render(); else load(); }
    document.dispatchEvent(new CustomEvent("sb:view", { detail: orbitOn ? "orbit" : "network" }));
  }
  tabs.forEach(function (b) { b.addEventListener("click", function () { setView(b.getAttribute("data-view"), true); }); });

  document.addEventListener("click", function (ev) {
    var b = ev.target.closest && ev.target.closest("[data-network]");
    if (!b) return;
    var params = {};
    new URLSearchParams(b.getAttribute("data-network") || "").forEach(function (v, k) { params[k] = v; });
    document.dispatchEvent(new CustomEvent("sb:graph-load", { detail: params }));
    setView("network", true);
  });

  document.body.addEventListener("graph-refresh", function () { if (!root.hidden) load(); });
  var t;
  window.addEventListener("resize", function () { clearTimeout(t); t = setTimeout(render, 150); });

  var qs = new URLSearchParams(location.search), stored = null;
  try { stored = localStorage.getItem("sb.view"); } catch (e) { /* private mode */ }
  setView(qs.get("focus") || qs.get("q") ? "network" : (stored || "orbit"), false);
})();
