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
  var view = { x: 0, y: 0, k: 1 }, goal = null; // goal: camera target, eased toward each frame
  var inertia = { x: 0, y: 0 }, fade = 0, fadeNode = null, lastMove = null;
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
    wake();
  }
  function wake() {
    if (!running) { running = true; requestAnimationFrame(loop); }
  }
  function camTo(x, y, k) {
    goal = { x: x, y: y, k: k };
    inertia.x = inertia.y = 0;
    wake();
  }
  // Steps camera easing, pan inertia and hover fade; returns true while still moving.
  function animate() {
    var busy = false;
    if (goal) {
      var t = 0.16;
      view.x += (goal.x - view.x) * t; view.y += (goal.y - view.y) * t; view.k += (goal.k - view.k) * t;
      if (Math.abs(goal.x - view.x) < 0.3 && Math.abs(goal.y - view.y) < 0.3 && Math.abs(goal.k - view.k) < 0.001) {
        view.x = goal.x; view.y = goal.y; view.k = goal.k; goal = null;
      } else busy = true;
    }
    if (!pan && (Math.abs(inertia.x) > 0.05 || Math.abs(inertia.y) > 0.05)) {
      view.x += inertia.x; view.y += inertia.y;
      inertia.x *= 0.92; inertia.y *= 0.92;
      busy = true;
    }
    var f = hover || selected, target = f ? 1 : 0;
    if (f) fadeNode = f;
    if (Math.abs(fade - target) > 0.01) { fade += (target - fade) * 0.2; busy = true; }
    else { fade = target; if (!f) fadeNode = null; }
    return busy;
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
      p.vx *= 0.6; p.vy *= 0.6;
      p.x += Math.max(-40, Math.min(40, p.vx));
      p.y += Math.max(-40, Math.min(40, p.vy));
    });
    alpha *= 0.988;
  }

  function loop() {
    if (alpha > 0.02) tick();
    if (needFit && alpha < 0.35) { fit(); needFit = false; }
    var busy = animate();
    draw();
    if (alpha > 0.02 || drag || busy) requestAnimationFrame(loop);
    else running = false;
  }

  function fit() {
    var vis = nodes.filter(visible);
    if (!vis.length) return;
    var minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    vis.forEach(function (n) { minX = Math.min(minX, n.x); minY = Math.min(minY, n.y); maxX = Math.max(maxX, n.x); maxY = Math.max(maxY, n.y); });
    var w = canvas.width / dpr, h = canvas.height / dpr;
    var k = Math.min(w / (maxX - minX + 120), h / (maxY - minY + 160), 2.2);
    k = Math.max(0.1, k);
    camTo(w / 2 - ((minX + maxX) / 2) * k, h / 2 - ((minY + maxY) / 2) * k + 20, k);
  }

  function draw() {
    var w = canvas.width, h = canvas.height;
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, w, h);
    var bg = css("--bg") || "#fff";
    var g = ctx.createRadialGradient(w / 2, h / 2, 0, w / 2, h / 2, Math.max(w, h) * 0.7);
    g.addColorStop(0, css("--o-bg1") || bg); g.addColorStop(1, css("--o-bg2") || bg);
    ctx.fillStyle = g; ctx.fillRect(0, 0, w, h);
    ctx.setTransform(dpr * view.k, 0, 0, dpr * view.k, dpr * view.x, dpr * view.y);
    var f = fadeNode, focusSet = null;
    if (f) { focusSet = new Set(adj.get(f.id) || []); focusSet.add(f.id); }
    var dim = function (on) { return on ? 1 : 1 - 0.85 * fade; };
    var line = css("--muted") || "#888";
    ctx.lineCap = "round";
    edges.forEach(function (e) {
      if (!visible(e.s) || !visible(e.t)) return;
      var lit = f && (e.s === f || e.t === f);
      var base = 0.14 + 0.26 * Math.min(1, e.w || 0.5);
      ctx.globalAlpha = lit ? base + (0.85 - base) * fade : base * dim(false);
      ctx.strokeStyle = lit ? (colors[f.type] || line) : line;
      ctx.lineWidth = (lit ? 1 + fade : 1) / view.k;
      var mx = (e.s.x + e.t.x) / 2, my = (e.s.y + e.t.y) / 2;
      var cx = mx - (e.t.y - e.s.y) * 0.12, cy = my + (e.t.x - e.s.x) * 0.12;
      ctx.beginPath(); ctx.moveTo(e.s.x, e.s.y); ctx.quadraticCurveTo(cx, cy, e.t.x, e.t.y); ctx.stroke();
    });
    var text = css("--text") || "#111";
    ctx.font = "500 " + (12 / view.k) + "px system-ui, sans-serif";
    ctx.textAlign = "center";
    ctx.lineJoin = "round";
    nodes.forEach(function (n) {
      if (!visible(n)) return;
      var r = radius(n), c = colors[n.type] || "#888";
      var on = !focusSet || focusSet.has(n.id);
      ctx.globalAlpha = dim(on);
      if (n === hover || n === selected || highlight.has(n.id)) {
        ctx.shadowColor = c; ctx.shadowBlur = 16 * dpr;
      }
      var grad = ctx.createRadialGradient(n.x - r * 0.35, n.y - r * 0.35, r * 0.1, n.x, n.y, r);
      grad.addColorStop(0, "rgba(255,255,255,.55)"); grad.addColorStop(0.35, c); grad.addColorStop(1, c);
      ctx.fillStyle = grad;
      ctx.beginPath(); ctx.arc(n.x, n.y, r, 0, Math.PI * 2); ctx.fill();
      ctx.shadowBlur = 0;
      ctx.strokeStyle = bg; ctx.lineWidth = 1.5 / view.k; ctx.stroke();
      if (n.type === "task" && n.status === "done") {
        ctx.strokeStyle = "#fff"; ctx.lineWidth = 1.5 / view.k;
        ctx.beginPath(); ctx.moveTo(n.x - r / 2, n.y); ctx.lineTo(n.x - r / 6, n.y + r / 2.5); ctx.lineTo(n.x + r / 2, n.y - r / 2.5); ctx.stroke();
      }
      if (highlight.has(n.id) || n === selected) {
        ctx.strokeStyle = n === selected ? text : c;
        ctx.lineWidth = 2 / view.k;
        ctx.beginPath(); ctx.arc(n.x, n.y, r + 4 / view.k, 0, Math.PI * 2); ctx.stroke();
      }
      var showThis = n === hover || n === selected || highlight.has(n.id) || (showLabels && (view.k > 0.9 || (n.degree || 0) >= 4)) || (focusSet && focusSet.has(n.id) && fade > 0.5);
      if (showThis) {
        var label = n.title.length > 42 ? n.title.slice(0, 40) + "…" : n.title;
        var ly = n.y + r + 14 / view.k;
        ctx.strokeStyle = bg; ctx.lineWidth = 3.5 / view.k; ctx.globalAlpha *= 0.9;
        ctx.strokeText(label, n.x, ly);
        ctx.globalAlpha = dim(on);
        ctx.fillStyle = text;
        ctx.fillText(label, n.x, ly);
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
    else {
      if (goal) { view.x = goal.x; view.y = goal.y; view.k = goal.k; goal = null; }
      pan = { x: ev.clientX - view.x, y: ev.clientY - view.y };
      inertia.x = inertia.y = 0; lastMove = { x: ev.clientX, y: ev.clientY, t: performance.now() };
    }
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
      var now = performance.now(), dt = Math.max(1, now - lastMove.t);
      inertia.x = 0.6 * inertia.x + 0.4 * (ev.clientX - lastMove.x) * 16 / dt;
      inertia.y = 0.6 * inertia.y + 0.4 * (ev.clientY - lastMove.y) * 16 / dt;
      lastMove = { x: ev.clientX, y: ev.clientY, t: now };
      draw();
      return;
    }
    var n = pick(ev);
    if (n !== hover) { hover = n; wake(); }
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
    if (pan && lastMove && performance.now() - lastMove.t > 80) inertia.x = inertia.y = 0;
    drag = null; pan = null;
    wake();
    canvas.classList.remove("dragging");
  }
  canvas.addEventListener("pointerup", release);
  canvas.addEventListener("pointercancel", release);
  canvas.addEventListener("pointerleave", function () { if (!drag) { hover = null; tip.hidden = true; wake(); } });

  canvas.addEventListener("dblclick", function (ev) {
    var n = pick(ev);
    if (n) load({ focus: n.id });
  });

  canvas.addEventListener("wheel", function (ev) {
    ev.preventDefault();
    var r = canvas.getBoundingClientRect();
    var mx = ev.clientX - r.left, my = ev.clientY - r.top;
    var from = goal || view;
    var k = Math.max(0.08, Math.min(6, from.k * Math.exp(-ev.deltaY * 0.0015)));
    camTo(mx - (mx - from.x) * (k / from.k), my - (my - from.y) * (k / from.k), k);
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
  document.getElementById("g-labels").addEventListener("click", function () { showLabels = !showLabels; wake(); });
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
      var k = Math.max(view.k, 1.2);
      camTo(w / 2 - n.x * k, h / 2 - n.y * k, k);
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
