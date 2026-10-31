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
// The sheet opens at the click, not when the answer comes: an outline of the
// panel (the skeleton in the page's <template>) slides in at once, and the
// panel takes its place inside the same sheet when it arrives. A sheet that
// is already open stays open; a card or a tab clicked inside it changes what
// it holds, in outline only if the answer is slow.
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
  var SKELETON_ID = "canvas-sheet-skeleton";
  var PANEL_PATH = /^\/apps\/[^/]+$/;
  // The tabs that are swapped in place. Metrics and Console are not: their
  // scripts start on page load (charts, and a terminal with a socket open),
  // so those tabs, and any tab clicked while one of them is showing, are
  // still page loads.
  var TAB_PATH = /^\/apps\/[^/]+\/(variables|settings|domains|storage|database|backups)$/;
  var SCRIPTED_TAB = /^\/apps\/[^/]+\/(metrics|console)$/;
  // An answer quicker than this replaces what is there without an outline in
  // between, which would only be a flash.
  var SLOW_MS = 150;

  // Whether the sheet holds a tab whose scripts a swap would strand.
  var scripted = SCRIPTED_TAB.test(location.pathname);
  // Where closing the panel goes: the canvas this page was loaded at, or the
  // project's canvas in the environment it shows.
  var atPanel = /^\/apps\//.test(location.pathname);
  var canvasHref = atPanel
    ? "/projects/" + encodeURIComponent(canvas.getAttribute("data-project") || "default") +
      "?environment=" + encodeURIComponent(canvas.getAttribute("data-environment") || "production")
    : location.pathname + location.search;
  var canvasTitle = atPanel ? null : document.title;

  function swappable(path) {
    return PANEL_PATH.test(path) || TAB_PATH.test(path);
  }

  // The panel a path belongs to: /apps/web for /apps/web/variables.
  function panelOf(path) {
    return path.split("/").slice(0, 3).join("/");
  }

  // Every entry this page makes, and the one it was loaded at, says what it
  // shows, so back and forward know whether to open a panel or close it.
  window.history.replaceState(stateFor(location.pathname, document.title), "", location.href);

  function stateFor(path, title) {
    return { yachtCanvas: true, panel: /^\/apps\/[^/]+/.test(path) ? path : null, title: title };
  }

  canvas.addEventListener("click", function (event) {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    var card = event.target.closest && event.target.closest("a.canvas-card");
    if (!card || !canvas.contains(card)) return;
    var url = new URL(card.href, location.href);
    if (scripted || url.origin !== location.origin || !PANEL_PATH.test(url.pathname) || url.search || url.hash) {
      keepView();
      return;
    }
    event.preventDefault();
    show(url.pathname, true, "card");
  });

  // A tab in the open panel. The sheet is a modal dialog, outside the canvas,
  // so this listens on the document.
  document.addEventListener("click", function (event) {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    var tab = event.target.closest && event.target.closest("a.tab");
    var sheet = document.getElementById(SHEET_ID);
    if (!tab || !sheet || !sheet.contains(tab) || !tab.closest(".canvas-sheet-tabs")) return;
    var url = new URL(tab.href, location.href);
    if (scripted || url.origin !== location.origin || !swappable(url.pathname) || url.search || url.hash) {
      keepView();
      return;
    }
    event.preventDefault();
    if (url.pathname === location.pathname) return;
    show(url.pathname, true, "tab", tab);
  });

  // A full page load after all: say so to canvas.js, so it keeps the view
  // this canvas had rather than sliding to the card.
  function keepView() {
    try {
      sessionStorage.setItem("yacht.keepView", "1");
    } catch (e) {
      // The slide is the worst of it.
    }
  }

  window.addEventListener("popstate", function (event) {
    var state = event.state;
    if (!state || !state.yachtCanvas) return;
    if (!state.panel) {
      hide();
      if (state.title) document.title = state.title;
    } else if (swappable(state.panel) && !scripted) {
      show(state.panel, false, panelOf(state.panel) === panelOf(openPath() || "") ? "tab" : "card");
    } else {
      location.reload();
    }
  });

  // The panel the open sheet shows, from the marked card.
  function openPath() {
    var card = canvas.querySelector("a.canvas-card.canvas-card-open");
    return card ? new URL(card.href, location.href).pathname : null;
  }

  function contentOf(sheet) {
    return sheet && sheet.querySelector("[data-tui-dialog-content]");
  }

  function skeleton() {
    var tpl = document.getElementById(SKELETON_ID);
    return tpl && tpl.content ? tpl.content : null;
  }

  // The sheet to open: the page's, or, on a canvas with no panel open yet,
  // the skeleton's, put where the server renders the panel: after the deploy
  // logs sheet, which follows the canvas.
  function shell() {
    var sheet = document.getElementById(SHEET_ID);
    if (sheet) return sheet;
    var outline = skeleton();
    var root = outline && outline.querySelector("[data-tui-dialog]");
    if (!root) return null;
    sheet = document.importNode(root, true);
    sheet.setAttribute("id", SHEET_ID);
    var anchor = document.getElementById("deploy-logs-sheet") || canvas;
    anchor.after(sheet);
    return sheet;
  }

  // outline puts the skeleton in the sheet: all of it, or for a tab only the
  // body, under the panel's own header and tabs.
  function outline(dialog, part) {
    var from = skeleton();
    if (!from || !dialog) return;
    if (part === "body") {
      var body = dialog.querySelector(".canvas-sheet-body");
      var rows = from.querySelector(".canvas-sheet-body");
      if (body && rows) {
        body.replaceChildren.apply(body, cloneChildren(rows));
        body.setAttribute("aria-busy", "true");
        return;
      }
    }
    var whole = from.querySelector("[data-tui-dialog-content]");
    if (!whole) return;
    var keep = focusKey(dialog);
    dialog.replaceChildren.apply(dialog, cloneChildren(whole));
    dialog.setAttribute("aria-labelledby", whole.getAttribute("aria-labelledby"));
    dialog.setAttribute("aria-busy", "true");
    refocus(dialog, keep);
  }

  function cloneChildren(el) {
    return Array.prototype.map.call(el.childNodes, function (n) {
      return document.importNode(n, true);
    });
  }

  // The tab clicked is the current one from the moment it is clicked.
  function markTab(dialog, tab) {
    if (!tab) return;
    dialog.querySelectorAll(".canvas-sheet-tabs a.tab").forEach(function (t) {
      var on = t === tab;
      t.classList.toggle("tab-active", on);
      if (on) t.setAttribute("aria-current", "page");
      else t.removeAttribute("aria-current");
    });
  }

  // show puts the panel at path in the sheet, opening it if it is closed.
  var pending = null;
  var slow = null;
  function show(path, push, kind, tab) {
    var request = (pending = {});
    window.clearTimeout(slow);
    var sheet = shell();
    var dialog = contentOf(sheet);
    var tui = window.tui && window.tui.dialog;
    if (dialog && tui) {
      watch(dialog);
      var open = dialog.open && dialog.getAttribute("data-tui-dialog-closing") !== "true";
      if (!open) {
        outline(dialog, "panel");
        tui.open(sheet);
      } else {
        markTab(dialog, tab);
        slow = window.setTimeout(function () {
          if (request === pending) outline(dialog, kind === "tab" ? "body" : "panel");
        }, SLOW_MS);
      }
    }
    markOpen(panelOf(path));

    fetch(path, { credentials: "same-origin", headers: { Accept: "text/html" } })
      .then(function (response) {
        if (!response.ok || new URL(response.url, location.href).pathname !== path) throw new Error("not the panel");
        return response.text();
      })
      .then(function (text) {
        if (request !== pending) return; // a later click won
        window.clearTimeout(slow);
        var doc = new DOMParser().parseFromString(text, "text/html");
        var next = doc.getElementById(SHEET_ID);
        if (!next) throw new Error("no panel");
        var current = document.getElementById(SHEET_ID);
        var into = contentOf(current);
        var from = contentOf(next);
        if (into && from && tui) {
          swap(into, from);
        } else {
          // No skeleton to have opened: the panel's own sheet goes in, and
          // templUI's dialog script, seeing it arrive, opens it as it does
          // one rendered open on page load.
          var sheet = document.adoptNode(next);
          if (current) {
            current.replaceWith(sheet);
          } else {
            var anchor = document.getElementById("deploy-logs-sheet") || canvas;
            anchor.after(sheet);
          }
          if (window.htmx) window.htmx.process(sheet);
        }
        scripted = false;
        if (doc.title) document.title = doc.title;
        // Reopening a panel closed at its own address is not a new place.
        if (push && location.pathname !== path) window.history.pushState(stateFor(path, document.title), "", path);
      })
      .catch(function () {
        if (request === pending) location.assign(path);
      });
  }

  // swap moves the panel's content into the open sheet. The <dialog> stays:
  // it is what is open, what holds focus, and what gives it back to the card
  // on close.
  function swap(into, from) {
    var keep = focusKey(into);
    var label = from.getAttribute("aria-labelledby");
    into.replaceChildren.apply(into, Array.prototype.slice.call(from.childNodes).map(function (n) {
      return document.adoptNode(n);
    }));
    if (label) into.setAttribute("aria-labelledby", label);
    else into.removeAttribute("aria-labelledby");
    into.removeAttribute("aria-busy");
    if (window.htmx) window.htmx.process(into);
    refocus(into, keep);
  }

  // What had focus in the sheet, to find again in what replaces it: the tab
  // that was clicked, say. Undefined when focus was elsewhere.
  function focusKey(dialog) {
    var el = document.activeElement;
    if (!el || el === document.body || !dialog.contains(el)) return undefined;
    return el.getAttribute("href") || "";
  }

  // Focus stays in the sheet when what held it is replaced: on the same link
  // if there is one, otherwise where showModal would put it.
  function refocus(dialog, key) {
    if (key === undefined) return;
    var el = key ? dialog.querySelector('a[href="' + key.replace(/["\\]/g, "\\$&") + '"]') : null;
    el = el || dialog.querySelector(
      'a[href], button:not([disabled]), input:not([type="hidden"]):not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])'
    );
    if (el && el.focus) el.focus({ preventScroll: true });
  }

  // Closed by its button, Esc or a click beside it, the panel is gone from
  // the address too, and no card is marked: the status poll in canvas.js
  // redraws only a canvas with no panel open, and a reload at the panel's
  // address would open it again.
  function watch(dialog) {
    if (dialog.yachtWatched) return;
    dialog.yachtWatched = true;
    dialog.addEventListener("close", function () {
      if (document.getElementById(SHEET_ID) !== dialog.closest("[data-tui-dialog]")) return;
      pending = null;
      window.clearTimeout(slow);
      markOpen(null);
      if (/^\/apps\//.test(location.pathname)) {
        if (canvasTitle) document.title = canvasTitle;
        window.history.pushState(stateFor(canvasHref, document.title), "", canvasHref);
      }
    });
  }
  var rendered = contentOf(document.getElementById(SHEET_ID));
  if (rendered) watch(rendered);

  function hide() {
    pending = null;
    window.clearTimeout(slow);
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
