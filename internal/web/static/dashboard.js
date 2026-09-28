// Painel: chart tooltips (hover and keyboard focus) and "Ver tudo" toggles.
(function () {
  "use strict";

  var tip = document.createElement("div");
  tip.className = "chart-tip";
  tip.hidden = true;
  tip.setAttribute("role", "status");
  var value = document.createElement("b");
  var label = document.createElement("span");
  tip.appendChild(value);
  tip.appendChild(label);
  document.body.appendChild(tip);

  function show(col) {
    value.textContent = col.getAttribute("data-tip-value"); // textContent: labels are data
    label.textContent = col.getAttribute("data-tip-label");
    tip.hidden = false;
    var r = col.getBoundingClientRect();
    var w = tip.offsetWidth, h = tip.offsetHeight;
    var x = Math.min(Math.max(8, r.left + r.width / 2 - w / 2), window.innerWidth - w - 8);
    var y = r.top + r.height - (col.querySelector("i").offsetHeight || 0) - h - 8;
    tip.style.left = x + "px";
    tip.style.top = Math.max(8, y) + "px";
  }
  function hide() { tip.hidden = true; }

  document.querySelectorAll("[data-chart] .col").forEach(function (col) {
    col.addEventListener("pointerenter", function () { show(col); });
    col.addEventListener("pointerleave", hide);
    col.addEventListener("focus", function () { show(col); });
    col.addEventListener("blur", hide);
  });
  window.addEventListener("scroll", hide, { passive: true });

  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-expand]");
    if (!b) return;
    var el = document.getElementById(b.getAttribute("data-expand"));
    if (!el) return;
    var open = el.classList.toggle("open");
    b.setAttribute("aria-expanded", String(open));
    b.textContent = open ? "Ver menos" : "Ver tudo";
  });
})();
