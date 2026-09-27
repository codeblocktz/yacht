/* The sidebar's behaviour: its two menus, the project tree, and the theme.

   Everything here is an enhancement over markup that already works. The
   menus are <details>, which open without this file; the tree's rows are
   links; the theme is applied before paint by an inline script in the head.
   What this adds is what makes them feel like the controls they look like —
   arrow keys, Escape, a click away closing a menu, a tree that remembers
   which projects were left open. */
(function () {
  "use strict";

  var root = document.documentElement;

  /* ---- theme ----------------------------------------------------------

     Three states, stored as two: "light" and "dark" are choices, and no
     stored value at all means "follow the system" — which is also what a
     fresh browser gets, and what themeScript reads before paint. */
  var THEME = "yacht-theme";
  var media = window.matchMedia("(prefers-color-scheme: dark)");

  function storedTheme() {
    try {
      return localStorage.getItem(THEME) || "system";
    } catch (e) {
      return "system";
    }
  }

  function applyTheme(mode) {
    var dark = mode === "dark" || (mode === "system" && media.matches);
    root.classList.toggle("dark", dark);
    var choices = document.querySelectorAll("[data-theme-set]");
    for (var i = 0; i < choices.length; i++) {
      var on = choices[i].getAttribute("data-theme-set") === mode;
      choices[i].setAttribute("aria-checked", on ? "true" : "false");
    }
  }

  window.yachtSetTheme = function (mode) {
    try {
      if (mode === "system") localStorage.removeItem(THEME);
      else localStorage.setItem(THEME, mode);
    } catch (e) {
      /* Private browsing: the choice holds for this page and no longer. */
    }
    applyTheme(mode);
  };

  // Flips between light and dark from wherever it is now, for the palette,
  // where a three-way choice would be a submenu for one keystroke.
  window.yachtToggleTheme = function () {
    window.yachtSetTheme(root.classList.contains("dark") ? "light" : "dark");
  };

  var onSchemeChange = function () {
    if (storedTheme() === "system") applyTheme("system");
  };
  if (media.addEventListener) media.addEventListener("change", onSchemeChange);

  /* ---- menus ------------------------------------------------------------ */

  function menuItems(menu) {
    var all = menu.querySelectorAll('[role="menuitem"], [role="menuitemradio"]');
    var out = [];
    for (var i = 0; i < all.length; i++) {
      if (!all[i].disabled && all[i].offsetParent !== null) out.push(all[i]);
    }
    return out;
  }

  function panelOf(details) {
    return details.querySelector('[role="menu"]');
  }

  function openMenu(details, last) {
    details.open = true;
    // Opened by keyboard, focus goes into the menu — onto the item that is
    // already chosen, if there is one, so Enter on it changes nothing.
    window.requestAnimationFrame(function () {
      var items = menuItems(panelOf(details));
      if (!items.length) return;
      var target = last ? items[items.length - 1] : items[0];
      for (var i = 0; i < items.length; i++) {
        if (items[i].getAttribute("aria-checked") === "true" && !last) {
          target = items[i];
          break;
        }
      }
      target.focus();
    });
  }

  function closeMenu(details, refocus) {
    if (!details.open) return;
    details.open = false;
    if (refocus) details.querySelector("summary").focus();
  }

  // Capture, so this runs before the drawer's own Escape handler: Escape in a
  // menu inside the phone drawer closes the menu, not the menu and the drawer.
  document.addEventListener("keydown", function (event) {
    var details = event.target.closest && event.target.closest("details[data-menu]");
    if (!details) return;
    var summary = details.querySelector("summary");

    if (event.target === summary) {
      switch (event.key) {
        case "ArrowDown":
        case "ArrowUp":
          event.preventDefault();
          openMenu(details, event.key === "ArrowUp");
          return;
        // Enter opens straight into the menu. Space is left to the browser:
        // a summary acts on Space's keyup, which preventing the keydown
        // does not stop, so handling it here would open and shut it at once.
        case "Enter":
          if (!details.open) {
            event.preventDefault();
            openMenu(details, false);
          }
          return;
        case "Escape":
          if (details.open) {
            event.preventDefault();
            closeMenu(details, true);
          }
          return;
      }
      return;
    }

    var items = menuItems(panelOf(details));
    var i = items.indexOf(document.activeElement);
    switch (event.key) {
      case "ArrowDown":
        items[(i + 1) % items.length].focus();
        break;
      case "ArrowUp":
        items[(i - 1 + items.length) % items.length].focus();
        break;
      case "Home":
        items[0].focus();
        break;
      case "End":
        items[items.length - 1].focus();
        break;
      case "Escape":
        closeMenu(details, true);
        break;
      case "Tab":
        // Leaving the menu closes it; the browser moves focus on.
        closeMenu(details, false);
        return;
      default:
        return;
    }
    event.preventDefault();
  }, true);

  document.addEventListener("click", function (event) {
    // A click anywhere else closes an open menu, as it does everywhere else.
    var open = document.querySelectorAll("details[data-menu][open]");
    for (var i = 0; i < open.length; i++) {
      if (!open[i].contains(event.target)) open[i].open = false;
    }

    var closer = event.target.closest && event.target.closest("[data-menu-close]");
    if (closer) {
      var details = closer.closest("details[data-menu]");
      if (details) closeMenu(details, true);
    }

    var choice = event.target.closest && event.target.closest("[data-theme-set]");
    if (choice) {
      event.preventDefault();
      window.yachtSetTheme(choice.getAttribute("data-theme-set"));
    }
  });

  /* ---- project tree -----------------------------------------------------

     A tree in the ARIA sense: one tab stop, arrows to move, right and left to
     open and close. Which projects are open is remembered per project, in
     this browser; treeRestore() applies it before paint. */
  var TREE = "yacht-tree";

  function remembered() {
    try {
      return JSON.parse(localStorage.getItem(TREE) || "{}") || {};
    } catch (e) {
      return {};
    }
  }

  function setOpen(node, open) {
    if (open) node.setAttribute("data-open", "");
    else node.removeAttribute("data-open");
    var item = node.querySelector('[role="treeitem"][aria-expanded]');
    if (item) item.setAttribute("aria-expanded", open ? "true" : "false");
    var state = remembered();
    state[node.getAttribute("data-tree-node")] = open;
    try {
      localStorage.setItem(TREE, JSON.stringify(state));
    } catch (e) {
      /* Remembered for this page only. */
    }
  }

  function visible(tree) {
    var all = tree.querySelectorAll('[role="treeitem"]');
    var out = [];
    for (var i = 0; i < all.length; i++) {
      if (all[i].offsetParent !== null) out.push(all[i]);
    }
    return out;
  }

  function focusItem(tree, item) {
    if (!item) return;
    var all = tree.querySelectorAll('[role="treeitem"]');
    for (var i = 0; i < all.length; i++) all[i].tabIndex = -1;
    item.tabIndex = 0;
    item.focus();
  }

  document.addEventListener("click", function (event) {
    var toggle = event.target.closest && event.target.closest("[data-tree-toggle]");
    if (!toggle) return;
    event.preventDefault();
    var node = toggle.closest("[data-tree-node]");
    setOpen(node, !node.hasAttribute("data-open"));
  });

  document.addEventListener("keydown", function (event) {
    var item = event.target.closest && event.target.closest('[role="tree"] [role="treeitem"]');
    if (!item || event.altKey || event.ctrlKey || event.metaKey) return;
    var tree = item.closest('[role="tree"]');
    var list = visible(tree);
    var i = list.indexOf(item);
    var node = item.closest("[data-tree-node]");
    var parent = node.querySelector('[role="treeitem"]');
    var expandable = item.hasAttribute("aria-expanded");
    var expanded = item.getAttribute("aria-expanded") === "true";

    switch (event.key) {
      case "ArrowDown":
        focusItem(tree, list[i + 1]);
        break;
      case "ArrowUp":
        focusItem(tree, list[i - 1]);
        break;
      case "Home":
        focusItem(tree, list[0]);
        break;
      case "End":
        focusItem(tree, list[list.length - 1]);
        break;
      case "ArrowRight":
        if (!expandable) return;
        if (!expanded) setOpen(node, true);
        else focusItem(tree, visible(tree)[i + 1]);
        break;
      case "ArrowLeft":
        if (expandable && expanded) setOpen(node, false);
        else if (item !== parent) focusItem(tree, parent);
        else return;
        break;
      default:
        return;
    }
    event.preventDefault();
  });

  /* ---- start ------------------------------------------------------------- */

  function start() {
    applyTheme(storedTheme());
    // ⌘ means nothing on a keyboard that does not have it.
    if (!/Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent)) {
      var labels = document.querySelectorAll("[data-shortcut-label]");
      for (var i = 0; i < labels.length; i++) labels[i].textContent = "Ctrl K";
    }
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
