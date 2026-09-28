// Conteúdo page: selection (shift-click ranges, whole filter), bulk actions, detail panel.
(function () {
  "use strict";

  // Trash tab: "select all on this page".
  var checkAll = document.querySelector("[data-check-all]");
  if (checkAll) {
    checkAll.addEventListener("change", function () {
      document.querySelectorAll("#trash-form input[name=id]").forEach(function (c) { c.checked = checkAll.checked; });
    });
  }

  var bulk = document.getElementById("bulk");
  var filters = document.getElementById("filters");
  if (!bulk || !filters) return;
  var bar = document.getElementById("bulkbar");
  var sel = new Set(); // selected ids, kept across pages and list refreshes
  var allMode = false; // every result of the current filter
  var last = null; // last clicked row checkbox, for shift-click ranges
  var clearAfterSwap = false;

  function rowBoxes() { return Array.prototype.slice.call(document.querySelectorAll("#content-rows input[data-row]")); }
  function total() {
    var r = document.getElementById("content-rows");
    return r ? Number(r.getAttribute("data-total")) || 0 : 0;
  }

  function render() {
    var n = allMode ? total() : sel.size;
    bar.hidden = n === 0;
    document.getElementById("sel-count").textContent = n.toLocaleString("pt-BR") + (allMode ? " (todo o filtro)" : "");
    document.getElementById("sel-all").value = allMode ? "1" : "";
    var box = document.getElementById("sel-ids");
    box.textContent = "";
    if (!allMode) {
      sel.forEach(function (id) {
        var i = document.createElement("input");
        i.type = "hidden"; i.name = "id"; i.value = id;
        box.appendChild(i);
      });
    }
    var boxes = rowBoxes(), checked = 0;
    boxes.forEach(function (c) {
      c.checked = allMode || sel.has(c.value);
      c.closest("tr").classList.toggle("sel", c.checked);
      if (c.checked) checked++;
    });
    var page = document.querySelector("[data-check-page]");
    if (page) {
      page.checked = boxes.length > 0 && checked === boxes.length;
      page.indeterminate = checked > 0 && checked < boxes.length;
    }
    var banner = document.getElementById("select-all");
    if (banner) {
      banner.hidden = !(boxes.length > 0 && checked === boxes.length);
      banner.querySelector("[data-when=page]").hidden = allMode;
      banner.querySelector("[data-when=all]").hidden = !allMode;
    }
  }

  function clear() { sel.clear(); allMode = false; last = null; render(); }

  function leaveAllMode() { // keep the visible page selected when narrowing an "all" selection
    if (!allMode) return;
    allMode = false;
    rowBoxes().forEach(function (c) { sel.add(c.value); });
  }

  function setBox(c, on) {
    if (on) sel.add(c.value); else sel.delete(c.value);
  }

  bulk.addEventListener("click", function (e) {
    var t = e.target;
    if (t.matches("input[data-row]")) {
      leaveAllMode();
      var boxes = rowBoxes();
      if (e.shiftKey && last && last !== t) {
        var a = boxes.indexOf(last), b = boxes.indexOf(t);
        if (a >= 0 && b >= 0) {
          boxes.slice(Math.min(a, b), Math.max(a, b) + 1).forEach(function (c) { setBox(c, t.checked); });
        }
      }
      setBox(t, t.checked);
      last = t;
      render();
      return;
    }
    if (t.matches("[data-check-page]")) {
      leaveAllMode();
      rowBoxes().forEach(function (c) { setBox(c, t.checked); });
      render();
      return;
    }
    if (t.closest("[data-select-all]")) { allMode = true; render(); return; }
    if (t.closest("[data-clear-sel]")) { clear(); return; }
    if (t.closest("[data-export]")) { exportSelection(); return; }
    var more = t.closest("[data-more]");
    if (more) {
      var open = bar.classList.toggle("expanded");
      more.setAttribute("aria-expanded", String(open));
      return;
    }
    var btn = t.closest("button[name=action]");
    if (btn) {
      if (btn.value === "trash" && allMode && !window.confirm("Mover os " + total().toLocaleString("pt-BR") + " itens do filtro para a lixeira? Dá para desfazer por 30 dias.")) {
        e.preventDefault();
        return;
      }
      clearAfterSwap = btn.value === "trash";
      if (allMode) clearAfterSwap = true; // the result set may change
      return;
    }
    // Clicking elsewhere on a row toggles it (links and inputs keep their behaviour).
    var tr = t.closest("#content-rows tr[data-id]");
    if (tr && !t.closest("a, input, button, select, label")) {
      var c = tr.querySelector("input[data-row]");
      if (c) { c.click(); }
    }
  });

  // Enter in the tag field adds the tag (the form's first submit button is "Apagar").
  bulk.addEventListener("keydown", function (e) {
    if (e.key !== "Enter" || !e.target.matches("input")) return;
    e.preventDefault();
    if (e.target.name === "set_tag") bulk.querySelector("button[value=tag_add]").click();
  });

  function exportSelection() {
    var f = document.createElement("form");
    f.method = "post";
    f.action = "/content/export";
    f.hidden = true;
    function add(k, v) {
      var i = document.createElement("input");
      i.type = "hidden"; i.name = k; i.value = v;
      f.appendChild(i);
    }
    new FormData(filters).forEach(function (v, k) { add(k, v); });
    if (allMode) add("all", "1");
    else sel.forEach(function (id) { add("id", id); });
    document.body.appendChild(f);
    f.submit(); // native submit: the browser downloads the zip
    f.remove();
  }

  // A new filter means a new result set: start the selection over.
  filters.addEventListener("change", clear);
  filters.addEventListener("input", clear);

  function refilter() {
    if (filters.requestSubmit) filters.requestSubmit();
    else htmx.trigger(filters, "submit");
  }

  document.addEventListener("click", function (e) {
    var chip = e.target.closest("[data-special]");
    if (chip) {
      var input = filters.querySelector("input[name=special]");
      var on = input.value !== chip.getAttribute("data-special");
      input.value = on ? chip.getAttribute("data-special") : "";
      document.querySelectorAll("[data-special]").forEach(function (c) {
        var active = on && c === chip;
        c.classList.toggle("on", active);
        c.setAttribute("aria-pressed", String(active));
      });
      clear();
      refilter();
      return;
    }
    var batch = e.target.closest("[data-clear-batch]");
    if (batch) {
      filters.querySelector("input[name=batch]").value = "";
      batch.remove();
      clear();
      refilter();
      return;
    }
    // Tags in the detail panel filter this list instead of opening the graph search.
    var tag = e.target.closest("#detail a.tag");
    if (tag) {
      e.preventDefault();
      e.stopPropagation();
      filters.querySelector("input[name=tag]").value = tag.textContent.replace(/^#/, "").trim();
      clear();
      refilter();
    }
  }, true);

  // "Focar no grafo" from the detail panel opens the network view.
  document.addEventListener("sb:focus", function (ev) {
    if (!document.getElementById("graph") && ev.detail && ev.detail.id) window.location.href = "/?focus=" + ev.detail.id;
  });

  document.body.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (!t) return;
    if (t.id === "detail") {
      var art = t.querySelector("[data-node-id]");
      var open = art ? art.getAttribute("data-node-id") : "";
      document.querySelectorAll("#content-rows tr[data-id]").forEach(function (tr) { tr.classList.toggle("open", tr.getAttribute("data-id") === open); });
      if (window.innerWidth < 1100) t.scrollIntoView({ behavior: "smooth", block: "start" });
    }
  });

  // The list is swapped with outerHTML: re-apply the selection to the new rows.
  document.body.addEventListener("htmx:load", function (e) {
    if (!e.detail || !e.detail.elt || e.detail.elt.id !== "content-rows") return;
    if (clearAfterSwap) { clearAfterSwap = false; sel.clear(); allMode = false; last = null; }
    render();
  });

  render();
})();
