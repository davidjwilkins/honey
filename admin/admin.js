// Refreshes the stats every few seconds while the page is visible, says
// how many cached responses a purge would remove, and asks before purging
// everything.
(function () {
  var stats = document.getElementById("stats");
  function refresh() {
    if (document.hidden) return;
    fetch("/stats", { cache: "no-store" })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); })
      .then(function (html) { stats.innerHTML = html; })
      .catch(function () { /* try again next time */ });
  }
  setInterval(refresh, 5000);
  document.addEventListener("visibilitychange", refresh);

  var form = document.getElementById("purge");
  var button = document.getElementById("purge-button");
  var timer, asked = 0;
  function count() {
    var url = form.elements.url.value.trim();
    var prefix = form.elements.prefix.value;
    if (!url) {
      button.textContent = "Purge";
      return;
    }
    var ask = ++asked;
    fetch("/count?" + new URLSearchParams({ url: url, prefix: prefix }), { cache: "no-store" })
      .then(function (r) { return r.json(); })
      .then(function (body) {
        if (ask !== asked) return; // a later answer is coming
        if (typeof body.count !== "number") {
          button.textContent = "Purge";
        } else if (body.count === 0) {
          button.textContent = "Purge (none cached)";
        } else {
          button.textContent = "Purge " + body.count.toLocaleString() + " cached";
        }
      })
      .catch(function () { button.textContent = "Purge"; });
  }
  function later() {
    clearTimeout(timer);
    timer = setTimeout(count, 250);
  }
  form.addEventListener("input", later);
  form.addEventListener("change", later);

  document.getElementById("purge-all").addEventListener("submit", function (e) {
    if (!confirm("Purge everything from the cache? Every page will be fetched from the backend again.")) {
      e.preventDefault();
    }
  });
})();
