// Opening a service's panel from its card without leaving the canvas.
//
// A card is a link to /apps/<name>, and that page is the same canvas with
// the service's sheet rendered open. Followed as a link, it is a new page:
// the drawing is torn down and built again, and canvas.js, finding a card
// open, slides the view to show it beside the panel. Somebody who clicked a
// card on screen saw the whole canvas jump. Railway's stays still and the
// panel slides in over it, so this fetches the page and takes only its
// sheet, and the canvas, its view and the cards on it are never touched.
//
// Everything else falls back to the link: a modified click, a card leading
// somewhere other than its service's panel (a first deploy's logs, whose
// stream logs.js starts on page load), a response without a sheet, a failed
// request.
(function () {
  "use strict";

  var canvas = document.querySelector("[data-canvas]");
  if (!canvas || !window.fetch || !window.DOMParser || !window.history.pushState) return;

  var SHEET_ID = "canvas-sheet";
  var PANEL_PATH = /^\/apps\/[^/]+$/;

  // Every entry this page makes, and the one it was loaded at, says what it
  // shows, so back and forward know whether to open a panel or close it.
  window.history.replaceState(stateFor(location.pathname, document.title), "", location.href);

  function stateFor(path, title) {
    return { yachtCanvas: true, panel: PANEL_PATH.test(path) ? path : null, title: title };
  }

  canvas.addEventListener("click", function (event) {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    var card = event.target.closest && event.target.closest("a.canvas-card");
    if (!card || !canvas.contains(card)) return;
    var url = new URL(card.href, location.href);
    if (url.origin !== location.origin || !PANEL_PATH.test(url.pathname) || url.search || url.hash) {
      // A full page load after all: say so to canvas.js, so it keeps the view
      // this canvas had rather than sliding to the card.
      try {
        sessionStorage.setItem("yacht.keepView", "1");
      } catch (e) {
        // The slide is the worst of it.
      }
      return;
    }
    event.preventDefault();
    show(url.pathname, true);
  });

  window.addEventListener("popstate", function (event) {
    var state = event.state;
    if (!state || !state.yachtCanvas) return;
    if (state.panel) {
      show(state.panel, false);
    } else {
      hide();
      if (state.title) document.title = state.title;
    }
  });

  // show puts the panel at path in place of whatever sheet is there.
  // templUI's dialog script sees the new sheet arrive and opens it, as it
  // does a sheet rendered open on page load.
  var pending = null;
  function show(path, push) {
    var request = (pending = {});
    fetch(path, { credentials: "same-origin", headers: { Accept: "text/html" } })
      .then(function (response) {
        if (!response.ok || new URL(response.url, location.href).pathname !== path) throw new Error("not the panel");
        return response.text();
      })
      .then(function (text) {
        if (request !== pending) return; // a later click won
        var doc = new DOMParser().parseFromString(text, "text/html");
        var next = doc.getElementById(SHEET_ID);
        if (!next) throw new Error("no panel");
        var sheet = document.adoptNode(next);
        var current = document.getElementById(SHEET_ID);
        if (current) {
          current.replaceWith(sheet);
        } else {
          // Where the server renders it: after the deploy logs sheet, which
          // follows the canvas.
          var anchor = document.getElementById("deploy-logs-sheet") || canvas;
          anchor.after(sheet);
        }
        if (window.htmx) window.htmx.process(sheet);
        markOpen(path);
        if (doc.title) document.title = doc.title;
        // Reopening a panel closed at its own address is not a new place.
        if (push && location.pathname !== path) window.history.pushState(stateFor(path, document.title), "", path);
      })
      .catch(function () {
        if (request === pending) location.assign(path);
      });
  }

  function hide() {
    pending = null;
    var sheet = document.getElementById(SHEET_ID);
    if (sheet && window.tui && window.tui.dialog) window.tui.dialog.close(sheet);
    markOpen(null);
  }

  // The card whose panel is open is marked, as the server marks it: the
  // status poll in canvas.js leaves a canvas with an open panel alone.
  function markOpen(path) {
    canvas.querySelectorAll("a.canvas-card").forEach(function (card) {
      var href = new URL(card.href, location.href).pathname;
      card.classList.toggle("canvas-card-open", !!path && href === path);
    });
  }
})();
