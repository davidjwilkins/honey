// Refreshes the stats every few seconds while the page is visible, and
// asks before purging everything.
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

  var all = document.getElementById("purge-all");
  all.addEventListener("submit", function (e) {
    if (!confirm("Purge everything from the cache? Every page will be fetched from the backend again.")) {
      e.preventDefault();
    }
  });
})();
