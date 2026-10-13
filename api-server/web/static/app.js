(function () {
  var toggle = document.querySelector("[data-nav-toggle]");
  var nav = document.querySelector("[data-nav]");
  if (toggle && nav) {
    toggle.addEventListener("click", function () {
      var open = nav.classList.toggle("is-open");
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
    });
  }

  var sidebar = document.querySelector("[data-sidebar]");
  var backdrop = document.querySelector("[data-backdrop]");
  function setDrawer(open) {
    if (!sidebar) return;
    sidebar.classList.toggle("is-open", open);
    if (backdrop) backdrop.hidden = !open;
    document.body.classList.toggle("is-locked", open);
  }
  var opener = document.querySelector("[data-drawer]");
  if (opener) opener.addEventListener("click", function () { setDrawer(true); });
  if (backdrop) backdrop.addEventListener("click", function () { setDrawer(false); });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") setDrawer(false);
  });

  document.querySelectorAll("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (e) {
      if (!window.confirm(form.getAttribute("data-confirm"))) e.preventDefault();
    });
  });

  var card = document.querySelector("[data-ai-status]");
  if (card) {
    var loading = card.querySelector("[data-ai-loading]");
    fetch("/api/integrations/ai").then(function (r) { return r.json(); }).then(function (d) {
      loading.hidden = true;
      card.querySelector("[data-ai-body]").hidden = false;
      var ok = d.configured && d.connected && d.model_available;
      var pill = card.querySelector("[data-ai-pill]");
      pill.textContent = !d.configured ? "Belum diatur" : (ok ? "Terhubung" : "Perlu perhatian");
      pill.className = "pill " + (!d.configured ? "pill-neutral" : (ok ? "pill-ok" : "pill-bad"));
      var facts = [
        ["Penyedia", d.provider || "—"],
        ["Model", d.model || "—"],
        ["Latensi", d.latency_ms == null ? "—" : d.latency_ms + " ms"]
      ];
      var dl = card.querySelector("[data-ai-facts]");
      facts.forEach(function (pair) {
        var dt = document.createElement("dt");
        dt.textContent = pair[0];
        var dd = document.createElement("dd");
        dd.textContent = String(pair[1]);
        dl.appendChild(dt);
        dl.appendChild(dd);
      });
      if (d.error) {
        var p = card.querySelector("[data-ai-error]");
        p.hidden = false;
        p.textContent = d.error;
      }
    }).catch(function () {
      loading.textContent = "Status integrasi tidak terbaca.";
    });
  }

  var testBtn = document.querySelector("[data-ai-test]");
  if (testBtn) {
    testBtn.addEventListener("click", function () {
      var out = document.querySelector("[data-ai-test-out]");
      testBtn.disabled = true;
      out.hidden = false;
      out.textContent = "Menguji koneksi. Ini bisa butuh beberapa detik.";
      var body = "csrf=" + encodeURIComponent(testBtn.getAttribute("data-csrf") || "");
      fetch("/app/integrations/ai/test", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: body
      }).then(function (r) { return r.json(); }).then(function (d) {
        if (d.ok) out.textContent = "Berhasil dalam " + d.latency_ms + " ms. Balasan: " + (d.reply || "OK");
        else out.textContent = d.error || "Uji koneksi gagal.";
      }).catch(function () {
        out.textContent = "Uji koneksi gagal.";
      }).finally(function () { testBtn.disabled = false; });
    });
  }
})();
