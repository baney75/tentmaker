(function () {
  "use strict";
  // Fetch this page again every data-every seconds and swap in the new
  // <main>. One fetch at a time, none while the tab is hidden, a 10 s
  // timeout, and on failure the wait doubles up to 5 minutes, with jitter,
  // so many open tabs never stampede a device that is down.
  var every = (parseInt(document.body.getAttribute("data-every"), 10) || 15) * 1000;
  var maxWait = 300000;
  var wait = every;
  var timer = 0;
  var busy = false;
  var last = Date.now();
  var fresh = document.getElementById("fresh");
  var updated = document.getElementById("updated");

  function schedule(ms) {
    clearTimeout(timer);
    timer = setTimeout(tick, ms + Math.floor(Math.random() * 1000));
  }

  function stale(on) {
    if (!fresh) { return; }
    fresh.classList.toggle("stale", on);
    if (on && updated) {
      updated.textContent = updated.textContent.replace(/ \(.*\)$/, "") + " (can't reach the device, retrying)";
    }
  }

  function tick() {
    if (document.hidden || busy) { return; }
    busy = true;
    last = Date.now();
    var ctl = typeof AbortController === "function" ? new AbortController() : null;
    var cut = setTimeout(function () { if (ctl) { ctl.abort(); } }, 10000);
    fetch(location.pathname, { cache: "no-store", credentials: "same-origin", signal: ctl ? ctl.signal : undefined })
      .then(function (r) {
        if (!r.ok) { throw new Error("HTTP " + r.status); }
        return r.text();
      })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        var next = doc.getElementById("live");
        var cur = document.getElementById("live");
        if (!next || !cur) { throw new Error("no content"); }
        cur.replaceWith(document.importNode(next, true));
        var u = doc.getElementById("updated");
        if (u && updated) {
          updated.textContent = u.textContent;
          updated.setAttribute("datetime", u.getAttribute("datetime") || "");
        }
        wait = every;
        stale(false);
      })
      .catch(function () {
        wait = Math.min(wait * 2, maxWait);
        stale(true);
      })
      .then(function () {
        clearTimeout(cut);
        busy = false;
        schedule(wait);
      });
  }

  // A hidden tab lets its timer lapse; showing it again fetches at once
  // when the last try is old enough, else when the next one is due.
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden && !busy) { schedule(Math.max(0, wait - (Date.now() - last))); }
  });
  schedule(wait);
})();
