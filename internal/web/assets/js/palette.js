/* The command palette: ⌘K (Ctrl K) or "/" from anywhere.

   What it searches comes from three places, none of them a list kept here:

   - pages: every link in the sidebar, read from the sidebar itself, and the
     pages under them that the sidebar leaves out, declared in the dialog. A
     wrapping application's navigation is in it the moment it is drawn.
   - the team's projects and apps, and in the Admin console the install's
     teams: from the JSON index the dialog names, fetched once, the first
     time the palette opens on a page.
   - actions: declared by the server inside the dialog.

   The dialog is a native modal, so the page behind it is inert and Escape
   closes it with no help from here. */
(function () {
  "use strict";

  var dialog, input, list, scope;
  var index = null;
  var fetching = null;
  var results = [];
  var active = 0;
  var pick = null; // the action whose target is being chosen
  var returnTo = null;

  var KIND_ORDER = ["action", "page", "team", "project", "app"];
  var KIND_LABEL = { action: "Actions", page: "Pages", team: "Customers", project: "Projects", app: "Apps" };
  var STATE_CLASS = {
    serving: "status-ok",
    "deploy-failed": "status-warn",
    failed: "status-err",
    deploying: "status-info status-live",
  };

  /* ---- sources ---------------------------------------------------------- */

  function icon(name) {
    var t = dialog.querySelector('template[data-palette-icon="' + name + '"]');
    return t ? t.content.firstElementChild.cloneNode(true) : null;
  }

  function pages() {
    var out = [];
    var seen = {};
    var links = document.querySelectorAll("#sidebar [data-nav-item]");
    for (var i = 0; i < links.length; i++) {
      var a = links[i];
      var href = a.getAttribute("href");
      if (seen[href]) continue;
      seen[href] = true;
      var svg = a.querySelector("svg");
      out.push({
        kind: "page",
        name: a.getAttribute("data-nav-label") || a.textContent.trim(),
        detail: a.getAttribute("data-nav-group") || "",
        href: href,
        node: svg ? svg.cloneNode(true) : null,
        // A page's group is where it sits, not what it is called: it counts
        // only typed whole, or "cust" would list every page under Cluster.
        wholeDetail: true,
      });
    }
    var subs = dialog.querySelectorAll("[data-palette-page]");
    for (var j = 0; j < subs.length; j++) {
      var d = subs[j].dataset;
      if (seen[d.url]) continue;
      seen[d.url] = true;
      var parent = document.querySelector('#sidebar [data-nav-item][href="' + d.parent + '"] svg');
      out.push({
        kind: "page",
        name: d.label,
        detail: d.group || "",
        href: d.url,
        node: parent ? parent.cloneNode(true) : null,
        wholeDetail: true,
        sub: true,
      });
    }
    return out;
  }

  function actions() {
    var out = [];
    var decl = dialog.querySelectorAll("[data-palette-action]");
    for (var i = 0; i < decl.length; i++) {
      var d = decl[i].dataset;
      if (d.needs && !(index && index[d.needs])) continue;
      out.push({
        kind: "action",
        name: d.label,
        href: d.url || "",
        run: d.run || "",
        pick: d.pick || "",
        keywords: d.keywords || "",
        icon: d.icon,
      });
    }
    return out;
  }

  function teamEntries() {
    var out = [];
    var items = (index && index.items) || [];
    for (var i = 0; i < items.length; i++) {
      var it = items[i];
      var kind = it.kind === "project" || it.kind === "team" ? it.kind : "app";
      out.push({
        kind: kind,
        name: it.name,
        href: it.href,
        detail: it.detail || "",
        status: it.status || "",
        icon: { project: "boxes", team: "building" }[kind] || "box",
      });
    }
    return out;
  }

  function load() {
    var url = dialog.getAttribute("data-index");
    if (!url || index || fetching) return;
    fetching = fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (r) {
        return r.ok ? r.json() : { items: [] };
      })
      .catch(function () {
        return { items: [] };
      })
      .then(function (data) {
        index = data || { items: [] };
        if (dialog.open) render();
      });
  }

  /* ---- matching ---------------------------------------------------------

     Fuzzy enough to forgive, strict enough to rank: a name that starts with
     what was typed beats one with a word that does, which beats the letters
     appearing anywhere in order. Detail and keywords count, for less. */
  function score(text, q) {
    if (!q) return 1;
    text = text.toLowerCase();
    if (text === q) return 100;
    if (text.indexOf(q) === 0) return 80;
    var at = text.indexOf(q);
    if (at > 0 && /[\s\-_/.]/.test(text.charAt(at - 1))) return 60;
    if (at > 0) return 40;
    var j = 0;
    for (var i = 0; i < text.length && j < q.length; i++) {
      if (text.charAt(i) === q.charAt(j)) j++;
    }
    // Letters in order anywhere is generous; with one or two typed it matches
    // nearly everything, so it waits for a third.
    return j === q.length && q.length >= 3 ? 10 : 0;
  }

  // Detail and keywords count only from the start of one of their words: a
  // team's id from its beginning, an app by its project's name. Letters in
  // order anywhere is forgiving enough for a name, but beside it, it matched
  // "cust" to "Cluster". A page's group counts only when typed whole.
  function wordStart(text, q) {
    text = text.toLowerCase();
    for (var at = text.indexOf(q); at >= 0; at = text.indexOf(q, at + 1)) {
      if (at === 0 || /[\s\-_/.]/.test(text.charAt(at - 1))) return true;
    }
    return false;
  }

  function wholeWord(text, q) {
    return text.toLowerCase().split(/[\s\-_/.]+/).indexOf(q) >= 0;
  }

  function rank(entry, q) {
    var s = score(entry.name, q);
    if (s) return s + 5;
    if (!q) return 0;
    var detail = entry.detail || "";
    if (entry.wholeDetail ? wholeWord(detail, q) : wordStart(detail, q)) return 15;
    return wordStart(entry.keywords || "", q) ? 15 : 0;
  }

  function search(q) {
    q = q.trim().toLowerCase();
    var pool;
    if (pick) {
      pool = teamEntries().filter(function (e) {
        return e.kind === pick.pick;
      });
    } else {
      pool = actions().concat(pages(), teamEntries());
    }
    var scored = [];
    for (var i = 0; i < pool.length; i++) {
      var r = rank(pool[i], q);
      if (r > 0) scored.push({ e: pool[i], r: r, i: i });
    }
    // With nothing typed, everything in its natural order, capped per kind so
    // a team with ninety apps still sees the actions and the pages.
    if (!q) {
      var counts = {};
      return scored
        .filter(function (s) {
          // The pages the sidebar leaves out wait to be asked for, or the
          // list opens on every tab of every section.
          if (s.e.sub) return false;
          counts[s.e.kind] = (counts[s.e.kind] || 0) + 1;
          return pick || s.e.kind === "action" || s.e.kind === "page" || counts[s.e.kind] <= 6;
        })
        .map(function (s) {
          return s.e;
        });
    }
    scored.sort(function (a, b) {
      return b.r - a.r || a.i - b.i;
    });
    return scored.slice(0, 40).map(function (s) {
      return s.e;
    });
  }

  /* ---- drawing ---------------------------------------------------------- */

  function highlight(el, name, q) {
    var at = q ? name.toLowerCase().indexOf(q) : -1;
    if (at < 0) {
      el.textContent = name;
      return;
    }
    el.appendChild(document.createTextNode(name.slice(0, at)));
    var mark = document.createElement("mark");
    mark.textContent = name.slice(at, at + q.length);
    el.appendChild(mark);
    el.appendChild(document.createTextNode(name.slice(at + q.length)));
  }

  function render() {
    var q = input.value.trim().toLowerCase();
    results = search(q);
    // Grouped by kind, groups in the order their best result ranked.
    var groups = {};
    var order = [];
    for (var i = 0; i < results.length; i++) {
      var k = results[i].kind;
      if (!groups[k]) {
        groups[k] = [];
        order.push(k);
      }
      groups[k].push(results[i]);
    }
    if (!q) {
      order.sort(function (a, b) {
        return KIND_ORDER.indexOf(a) - KIND_ORDER.indexOf(b);
      });
    }
    results = [];
    list.textContent = "";

    for (var g = 0; g < order.length; g++) {
      var kind = order[g];
      var head = document.createElement("div");
      head.className = "palette-group";
      head.setAttribute("role", "presentation");
      head.textContent = pick ? pick.name.replace(/…$/, "") : KIND_LABEL[kind];
      list.appendChild(head);
      for (var j = 0; j < groups[kind].length; j++) {
        var e = groups[kind][j];
        var n = results.length;
        results.push(e);
        var opt = document.createElement("div");
        opt.className = "palette-opt";
        opt.id = "palette-opt-" + n;
        opt.setAttribute("role", "option");
        opt.setAttribute("aria-selected", "false");
        opt.setAttribute("data-index", String(n));
        // An app is drawn by its state, as it is in the sidebar: the dot
        // takes the icon's place rather than sitting beside a generic box.
        if (e.kind === "app") {
          var dot = document.createElement("span");
          dot.className = "tree-dot " + (STATE_CLASS[e.status] || "status-neutral");
          dot.setAttribute("aria-hidden", "true");
          opt.appendChild(dot);
        } else {
          var glyph = e.node ? e.node.cloneNode(true) : icon(e.icon);
          if (glyph) opt.appendChild(glyph);
        }
        var name = document.createElement("span");
        name.className = "palette-name";
        highlight(name, e.name, q);
        opt.appendChild(name);
        if (e.detail) {
          var detail = document.createElement("span");
          detail.className = "palette-detail";
          detail.textContent = e.detail;
          opt.appendChild(detail);
        } else {
          var spacer = document.createElement("span");
          spacer.className = "ml-auto";
          opt.appendChild(spacer);
        }
        var enter = icon("corner-down-left");
        if (enter) {
          enter.classList.add("palette-enter");
          opt.appendChild(enter);
        }
        list.appendChild(opt);
      }
    }

    if (!results.length) {
      var empty = document.createElement("div");
      empty.className = "palette-empty";
      empty.setAttribute("role", "presentation");
      empty.textContent = fetching && !index ? "Loading…" : 'Nothing matches "' + input.value.trim() + '"';
      list.appendChild(empty);
    }
    select(0);
  }

  function select(n) {
    var opts = list.querySelectorAll('[role="option"]');
    if (!opts.length) {
      input.removeAttribute("aria-activedescendant");
      return;
    }
    active = (n + opts.length) % opts.length;
    for (var i = 0; i < opts.length; i++) {
      opts[i].setAttribute("aria-selected", i === active ? "true" : "false");
    }
    input.setAttribute("aria-activedescendant", opts[active].id);
    opts[active].scrollIntoView({ block: "nearest" });
  }

  /* ---- doing ------------------------------------------------------------ */

  function choose(e, newTab) {
    if (!e) return;
    if (e.pick) {
      pick = e;
      scope.textContent = e.name.replace(/…$/, "");
      scope.hidden = false;
      input.value = "";
      input.setAttribute("placeholder", "Choose an app…");
      render();
      return;
    }
    var href = e.href;
    if (pick) href = pick.href.replace("{name}", encodeURIComponent(e.name));
    if (e.run === "theme") {
      close();
      if (window.yachtToggleTheme) window.yachtToggleTheme();
      return;
    }
    if (!href) return;
    if (newTab) {
      window.open(href, "_blank", "noopener");
      return;
    }
    close(true);
    window.location.href = href;
  }

  function leavePick() {
    pick = null;
    scope.hidden = true;
    input.setAttribute("placeholder", input.getAttribute("data-placeholder"));
    render();
  }

  function open() {
    if (!dialog || dialog.open) return;
    // A confirmation is waiting on an answer; the palette does not jump it.
    var confirm = document.querySelector("dialog[data-confirm-dialog][open]");
    if (confirm) return;
    returnTo = document.activeElement;
    pick = null;
    scope.hidden = true;
    input.value = "";
    input.setAttribute("placeholder", input.getAttribute("data-placeholder"));
    dialog.showModal();
    input.focus();
    render();
    load();
  }

  function close(navigating) {
    if (!dialog.open) return;
    dialog.close();
    if (!navigating && returnTo && returnTo.focus) returnTo.focus();
  }

  function typing(target) {
    if (!target) return false;
    var tag = target.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || target.isContentEditable;
  }

  function start() {
    dialog = document.querySelector("[data-palette]");
    if (!dialog) return;
    input = dialog.querySelector("[data-palette-input]");
    list = dialog.querySelector("[data-palette-list]");
    scope = dialog.querySelector("[data-palette-scope]");
    input.setAttribute("data-placeholder", input.getAttribute("placeholder"));

    document.addEventListener("keydown", function (event) {
      var k = event.key && event.key.toLowerCase();
      if (k === "k" && (event.metaKey || event.ctrlKey) && !event.altKey) {
        event.preventDefault();
        if (dialog.open) close();
        else open();
        return;
      }
      if (event.key === "/" && !dialog.open && !typing(event.target) &&
          !event.metaKey && !event.ctrlKey && !event.altKey) {
        event.preventDefault();
        open();
      }
    });

    document.addEventListener("click", function (event) {
      var opener = event.target.closest && event.target.closest("[data-palette-open]");
      if (opener) {
        event.preventDefault();
        // From the phone drawer, the drawer goes: the palette is what is
        // being looked at now.
        if (document.documentElement.getAttribute("data-nav") === "open" && window.yachtSidebarToggle) {
          window.yachtSidebarToggle();
        }
        open();
      }
    });

    input.addEventListener("input", render);
    input.addEventListener("keydown", function (event) {
      switch (event.key) {
        case "ArrowDown":
          select(active + 1);
          break;
        case "ArrowUp":
          select(active - 1);
          break;
        case "Home":
          if (input.value) return;
          select(0);
          break;
        case "End":
          if (input.value) return;
          select(-1);
          break;
        case "Enter":
          choose(results[active], event.metaKey || event.ctrlKey);
          break;
        case "Backspace":
          if (input.value || !pick) return;
          leavePick();
          break;
        case "Tab":
          // The field is the only thing in here to be on; Tab stays in it
          // rather than wandering off to the browser's own controls.
          break;
        default:
          return;
      }
      event.preventDefault();
    });

    // Escape inside a pick goes back a step rather than all the way out.
    dialog.addEventListener("cancel", function (event) {
      if (pick) {
        event.preventDefault();
        leavePick();
      }
    });
    dialog.addEventListener("close", function () {
      pick = null;
    });

    list.addEventListener("mousemove", function (event) {
      var opt = event.target.closest('[role="option"]');
      if (opt) {
        var n = Number(opt.getAttribute("data-index"));
        if (n !== active) select(n);
      }
    });
    list.addEventListener("click", function (event) {
      var opt = event.target.closest('[role="option"]');
      if (opt) choose(results[Number(opt.getAttribute("data-index"))], event.metaKey || event.ctrlKey);
    });
    // A click on the backdrop lands on the dialog itself, outside its box.
    dialog.addEventListener("click", function (event) {
      if (event.target !== dialog) return;
      var r = dialog.getBoundingClientRect();
      var inside = event.clientX >= r.left && event.clientX <= r.right &&
        event.clientY >= r.top && event.clientY <= r.bottom;
      if (!inside) close();
    });

    window.yachtPalette = { open: open, close: close };
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
