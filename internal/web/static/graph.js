// Dependency-free force-directed knowledge graph renderer (Canvas 2D).
(function () {
  "use strict";
  var canvas = document.getElementById("graph");
  if (!canvas) return;
  var ctx = canvas.getContext("2d");
  var tip = document.getElementById("g-tip");
  var statusEl = document.getElementById("g-status");
  var form = document.getElementById("search-form");
  var net = document.getElementById("network"); // hidden while the overview is shown
  var started = false, pending = null;
  function shown() { return !net || !net.hidden; }

  var nodes = [], edges = [], byId = new Map(), adj = new Map();
  var colors = {}, highlight = new Set(), hiddenTypes = new Set();
  var view = { x: 0, y: 0, k: 1 };
  var alpha = 0, running = false, showLabels = true, needFit = true;
  var hover = null, selected = null, drag = null, pan = null, moved = false;
  var dpr = window.devicePixelRatio || 1;
  var lastParams = {};

  function css(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

  function resize() {
    var r = canvas.getBoundingClientRect();
    dpr = window.devicePixelRatio || 1;
    canvas.width = Math.max(1, r.width * dpr);
    canvas.height = Math.max(1, r.height * dpr);
    draw();
  }

  function radius(n) { return 4 + Math.min(14, Math.sqrt(n.degree || 0) * 2.2); }
  function visible(n) { return !hiddenTypes.has(n.type); }

  function load(params) {
    lastParams = params || {};
    var qs = new URLSearchParams(lastParams);
    statusEl.textContent = "carregando…";
    return fetch("/api/graph?" + qs.toString(), { credentials: "same-origin" })
      .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); })
      .then(function (data) {
        var raw = data.colors || {};
        colors = {};
        Object.keys(raw).forEach(function (t) { colors[t] = css("--t-" + t) || raw[t]; });
        var old = byId;
        byId = new Map();
        adj = new Map();
        var spread = 30 * Math.sqrt((data.nodes || []).length + 1);
        nodes = (data.nodes || []).map(function (n) {
          var o = old.get(n.id);
          n.x = o ? o.x : (Math.random() - 0.5) * spread;
          n.y = o ? o.y : (Math.random() - 0.5) * spread;
          n.vx = 0; n.vy = 0;
          byId.set(n.id, n);
          adj.set(n.id, new Set());
          return n;
        });
        edges = [];
        (data.edges || []).forEach(function (e) {
          var s = byId.get(e.s), t = byId.get(e.t);
          if (!s || !t) return;
          edges.push({ s: s, t: t, rel: e.rel, w: e.w });
          adj.get(s.id).add(t.id);
          adj.get(t.id).add(s.id);
        });
        highlight = new Set(data.highlight || []);
        if (selected && !byId.has(selected.id)) selected = null;
        statusEl.textContent = nodes.length + " nós · " + edges.length + " arestas";
        if (!old.size || lastParams.q || lastParams.focus) needFit = true;
        kick(1);
      })
      .catch(function (err) { statusEl.textContent = "erro: " + err.message; });
  }

  function kick(a) {
    alpha = Math.max(alpha, a || 0.5);
    if (!running) { running = true; requestAnimationFrame(loop); }
  }

  function tick() {
    var vis = nodes.filter(visible);
    var n = vis.length, i, j, a, b, dx, dy, d2, d, f;
    var rep = 900 * alpha;
    if (n <= 900) {
      for (i = 0; i < n; i++) {
        a = vis[i];
        for (j = i + 1; j < n; j++) {
          b = vis[j];
          dx = a.x - b.x; dy = a.y - b.y;
          d2 = dx * dx + dy * dy + 0.01;
          if (d2 > 250000) continue;
          f = rep / d2;
          a.vx += dx * f; a.vy += dy * f;
          b.vx -= dx * f; b.vy -= dy * f;
        }
      }
    } else { // coarse grid approximation for large graphs
      var cell = 120, grid = new Map();
      vis.forEach(function (p) {
        var key = Math.floor(p.x / cell) + ":" + Math.floor(p.y / cell);
        var c = grid.get(key);
        if (!c) { c = { x: 0, y: 0, m: 0 }; grid.set(key, c); }
        c.x += p.x; c.y += p.y; c.m++;
      });
      grid.forEach(function (c) { c.x /= c.m; c.y /= c.m; });
      vis.forEach(function (p) {
        grid.forEach(function (c) {
          dx = p.x - c.x; dy = p.y - c.y; d2 = dx * dx + dy * dy + 25;
          f = rep * c.m / d2;
          p.vx += dx * f; p.vy += dy * f;
        });
      });
    }
    edges.forEach(function (e) {
      if (!visible(e.s) || !visible(e.t)) return;
      dx = e.t.x - e.s.x; dy = e.t.y - e.s.y;
      d = Math.sqrt(dx * dx + dy * dy) + 0.01;
      var len = 70 + 8 * Math.sqrt((e.s.degree || 0) + (e.t.degree || 0));
      f = (d - len) / d * 0.04 * alpha * (0.5 + Math.min(1, e.w || 1));
      e.s.vx += dx * f; e.s.vy += dy * f;
      e.t.vx -= dx * f; e.t.vy -= dy * f;
    });
    vis.forEach(function (p) {
      p.vx -= p.x * 0.004 * alpha;
      p.vy -= p.y * 0.004 * alpha;
      if (p === (drag && drag.node)) { p.vx = 0; p.vy = 0; return; }
      p.vx *= 0.55; p.vy *= 0.55;
      p.x += Math.max(-40, Math.min(40, p.vx));
      p.y += Math.max(-40, Math.min(40, p.vy));
    });
    alpha *= 0.985;
  }

  function loop() {
    if (alpha > 0.02) tick();
    if (needFit && alpha < 0.35) { fit(); needFit = false; }
    draw();
    if (alpha > 0.02 || drag) requestAnimationFrame(loop);
    else running = false;
  }

  function fit() {
    var vis = nodes.filter(visible);
    if (!vis.length) return;
    var minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    vis.forEach(function (n) { minX = Math.min(minX, n.x); minY = Math.min(minY, n.y); maxX = Math.max(maxX, n.x); maxY = Math.max(maxY, n.y); });
    var w = canvas.width / dpr, h = canvas.height / dpr;
    var k = Math.min(w / (maxX - minX + 120), h / (maxY - minY + 160), 2.2);
    view.k = Math.max(0.1, k);
    view.x = w / 2 - ((minX + maxX) / 2) * view.k;
    view.y = h / 2 - ((minY + maxY) / 2) * view.k + 20;
    draw();
  }

  function draw() {
    var w = canvas.width, h = canvas.height;
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, w, h);
    ctx.setTransform(dpr * view.k, 0, 0, dpr * view.k, dpr * view.x, dpr * view.y);
    var focusSet = null;
    var f = hover || selected;
    if (f) { focusSet = new Set(adj.get(f.id) || []); focusSet.add(f.id); }
    var line = css("--muted") || "#888";
    ctx.lineWidth = 1 / view.k;
    edges.forEach(function (e) {
      if (!visible(e.s) || !visible(e.t)) return;
      var lit = focusSet && focusSet.has(e.s.id) && focusSet.has(e.t.id) && (e.s === f || e.t === f);
      ctx.globalAlpha = lit ? 0.9 : focusSet ? 0.08 : 0.18 + 0.3 * Math.min(1, e.w || 0.5);
      ctx.strokeStyle = lit ? (colors[f.type] || line) : line;
      ctx.lineWidth = (lit ? 2 : 1) / view.k;
      ctx.beginPath(); ctx.moveTo(e.s.x, e.s.y); ctx.lineTo(e.t.x, e.t.y); ctx.stroke();
    });
    var text = css("--text") || "#111";
    ctx.font = (12 / view.k) + "px system-ui, sans-serif";
    ctx.textAlign = "center";
    nodes.forEach(function (n) {
      if (!visible(n)) return;
      var r = radius(n);
      ctx.globalAlpha = focusSet && !focusSet.has(n.id) ? 0.15 : 1;
      ctx.fillStyle = colors[n.type] || "#888";
      ctx.beginPath(); ctx.arc(n.x, n.y, r, 0, Math.PI * 2); ctx.fill();
      if (n.type === "task" && n.status === "done") {
        ctx.strokeStyle = "#fff"; ctx.lineWidth = 1.5 / view.k;
        ctx.beginPath(); ctx.moveTo(n.x - r / 2, n.y); ctx.lineTo(n.x - r / 6, n.y + r / 2.5); ctx.lineTo(n.x + r / 2, n.y - r / 2.5); ctx.stroke();
      }
      if (highlight.has(n.id) || n === selected) {
        ctx.strokeStyle = n === selected ? text : (colors[n.type] || "#888");
        ctx.lineWidth = 2.5 / view.k;
        ctx.beginPath(); ctx.arc(n.x, n.y, r + 4 / view.k, 0, Math.PI * 2); ctx.stroke();
      }
      var showThis = n === hover || n === selected || highlight.has(n.id) || (showLabels && (view.k > 0.9 || (n.degree || 0) >= 4)) || (focusSet && focusSet.has(n.id));
      if (showThis) {
        ctx.fillStyle = text;
        var label = n.title.length > 42 ? n.title.slice(0, 40) + "…" : n.title;
        ctx.fillText(label, n.x, n.y + r + 13 / view.k);
      }
    });
    ctx.globalAlpha = 1;
  }

  function toWorld(ev) {
    var r = canvas.getBoundingClientRect();
    return { x: (ev.clientX - r.left - view.x) / view.k, y: (ev.clientY - r.top - view.y) / view.k };
  }

  function pick(ev) {
    var p = toWorld(ev), best = null, bestD = Infinity;
    nodes.forEach(function (n) {
      if (!visible(n)) return;
      var dx = n.x - p.x, dy = n.y - p.y, d = dx * dx + dy * dy, r = radius(n) + 4 / view.k;
      if (d < r * r && d < bestD) { best = n; bestD = d; }
    });
    return best;
  }

  function openNode(n) {
    selected = n;
    draw();
    if (window.htmx) window.htmx.ajax("GET", "/nodes/" + n.id, { target: "#detail", swap: "innerHTML" });
  }

  canvas.addEventListener("pointerdown", function (ev) {
    canvas.setPointerCapture(ev.pointerId);
    moved = false;
    var n = pick(ev);
    if (n) { drag = { node: n }; kick(0.3); }
    else pan = { x: ev.clientX - view.x, y: ev.clientY - view.y };
    canvas.classList.add("dragging");
  });

  canvas.addEventListener("pointermove", function (ev) {
    if (drag) {
      var p = toWorld(ev);
      drag.node.x = p.x; drag.node.y = p.y; moved = true;
      kick(0.3);
      return;
    }
    if (pan) {
      view.x = ev.clientX - pan.x; view.y = ev.clientY - pan.y; moved = true;
      draw();
      return;
    }
    var n = pick(ev);
    if (n !== hover) { hover = n; draw(); }
    if (n) {
      var r = canvas.getBoundingClientRect();
      tip.hidden = false;
      tip.style.left = (ev.clientX - r.left + 14) + "px";
      tip.style.top = (ev.clientY - r.top + 14) + "px";
      tip.textContent = n.title + " — " + n.type + (n.tags && n.tags.length ? " · #" + n.tags.slice(0, 4).join(" #") : "");
    } else tip.hidden = true;
  });

  function release(ev) {
    var n = drag && drag.node;
    if (n && !moved) openNode(n);
    drag = null; pan = null;
    canvas.classList.remove("dragging");
  }
  canvas.addEventListener("pointerup", release);
  canvas.addEventListener("pointercancel", release);
  canvas.addEventListener("pointerleave", function () { if (!drag) { hover = null; tip.hidden = true; draw(); } });

  canvas.addEventListener("dblclick", function (ev) {
    var n = pick(ev);
    if (n) load({ focus: n.id });
  });

  canvas.addEventListener("wheel", function (ev) {
    ev.preventDefault();
    var r = canvas.getBoundingClientRect();
    var mx = ev.clientX - r.left, my = ev.clientY - r.top;
    var k = Math.max(0.08, Math.min(6, view.k * Math.exp(-ev.deltaY * 0.0015)));
    view.x = mx - (mx - view.x) * (k / view.k);
    view.y = my - (my - view.y) * (k / view.k);
    view.k = k;
    draw();
  }, { passive: false });

  document.getElementById("legend").addEventListener("click", function (ev) {
    var b = ev.target.closest("[data-type]");
    if (!b) return;
    var t = b.getAttribute("data-type");
    if (hiddenTypes.has(t)) { hiddenTypes.delete(t); b.classList.add("on"); }
    else { hiddenTypes.add(t); b.classList.remove("on"); }
    kick(0.3);
  });
  document.getElementById("g-fit").addEventListener("click", fit);
  document.getElementById("g-labels").addEventListener("click", function () { showLabels = !showLabels; draw(); });
  document.getElementById("g-reset").addEventListener("click", function () {
    if (form) form.reset();
    load({});
    if (window.htmx) window.htmx.ajax("GET", "/search", { target: "#results" });
  });

  function paramsFromForm() {
    var p = {};
    if (!form) return p;
    var fd = new FormData(form);
    var q = (fd.get("q") || "").trim();
    if (q) p.q = q;
    var types = fd.getAll("type");
    if (types.length) p.types = types.join(",");
    ["from", "to", "tag"].forEach(function (k) { var v = (fd.get(k) || "").trim(); if (v) p[k] = v; });
    return p;
  }
  var debounce;
  if (form) {
    ["input", "change", "submit"].forEach(function (evName) {
      form.addEventListener(evName, function (ev) {
        if (evName === "submit") ev.preventDefault();
        clearTimeout(debounce);
        debounce = setTimeout(function () {
          var p = paramsFromForm();
          if (started && shown()) load(p); else pending = p;
        }, 450);
      });
    });
  }

  document.addEventListener("sb:focus", function (ev) {
    if (!started || !shown()) return;
    var id = ev.detail.id, n = byId.get(id);
    if (n) {
      selected = n;
      var w = canvas.width / dpr, h = canvas.height / dpr;
      view.x = w / 2 - n.x * view.k; view.y = h / 2 - n.y * view.k;
      draw();
    } else {
      load({ focus: id }).then(function () {
        selected = byId.get(id) || null;
        draw();
      });
    }
  });

  document.body.addEventListener("graph-refresh", function () { if (started) load(lastParams); });

  function start(params) {
    started = true;
    resize();
    return load(params).then(function () {
      if (params.focus) {
        var n = byId.get(Number(params.focus));
        if (n) openNode(n);
      }
    });
  }

  // The network loads lazily: only when its tab is shown (see orbit.js).
  document.addEventListener("sb:view", function (ev) {
    if (ev.detail !== "network") return;
    var p = pending;
    pending = null;
    if (!started) start(p || init);
    else { resize(); if (p) load(p); }
  });
  document.addEventListener("sb:graph-load", function (ev) {
    pending = ev.detail || {};
    if (form) form.reset();
  });

  window.addEventListener("resize", resize);
  var init = {};
  if (canvas.dataset.q) init.q = canvas.dataset.q;
  if (canvas.dataset.focus) init.focus = canvas.dataset.focus;
  if (shown()) start(init);
})();
