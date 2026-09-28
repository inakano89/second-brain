// Global UI helpers (CSP-safe: no inline handlers).
(function () {
  "use strict";

  document.addEventListener("click", function (e) {
    var c = e.target.closest("[data-confirm]");
    if (c && !window.confirm(c.getAttribute("data-confirm"))) {
      e.preventDefault();
      e.stopPropagation();
      return;
    }
    var f = e.target.closest("[data-focus]");
    if (f) {
      document.dispatchEvent(new CustomEvent("sb:focus", { detail: { id: Number(f.getAttribute("data-focus")) } }));
      if (f.tagName === "A" && f.getAttribute("href") === "#") e.preventDefault();
    }
  }, true);

  document.addEventListener("htmx:afterRequest", function (e) {
    var el = e.detail && e.detail.elt;
    if (el && el.hasAttribute && el.hasAttribute("data-reset") && e.detail.successful) el.reset();
  });

  document.addEventListener("htmx:responseError", function (e) {
    var xhr = e.detail.xhr;
    var target = document.getElementById("detail");
    var msg = (xhr && xhr.responseText) || "Erro na requisição";
    if (target) {
      var div = document.createElement("div");
      div.className = "flash err small";
      div.textContent = msg.slice(0, 300);
      target.prepend(div);
    } else {
      window.alert(msg.slice(0, 300));
    }
  });

  // The top bar is sticky: expose its height for sticky toolbars and anchor offsets.
  var topbar = document.querySelector(".topbar");
  function measure() { if (topbar) document.documentElement.style.setProperty("--topbar-h", topbar.offsetHeight + "px"); }
  measure();
  window.addEventListener("resize", measure);

  // Copy-on-click for tokens.
  document.addEventListener("dblclick", function (e) {
    var c = e.target.closest(".copy");
    if (c && navigator.clipboard) navigator.clipboard.writeText(c.textContent.trim());
  });
})();
