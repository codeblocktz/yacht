// The project canvas, drawn with JointJS: cards you can move, and arrows that
// route around them from each app to what it depends on.
//
// The page renders and navigates without this file. The server lays the
// cards out and draws straight edges between them, every card is a link, and
// with the script blocked that picture is what you get. This file hands those
// same cards to JointJS — moved into the drawing rather than drawn again, so
// a card's status and links have one source, the template — and replaces the
// server's edges with routed, labelled ones.
//
// The view (where you have panned to, and how far you are zoomed) is this
// person's and stays in the browser. Where a card sits is the team's, and is
// sent to the server when it is dropped.
(function () {
  "use strict";

  var canvas = document.querySelector("[data-canvas]");
  if (!canvas || !window.joint) return;

  var inner = canvas.querySelector(".canvas-inner");
  var data = document.getElementById("canvas-graph");
  if (!inner || !data) return;

  var graphData;
  try {
    graphData = JSON.parse(data.textContent);
  } catch (e) {
    return; // The server's picture stays.
  }

  var CARD_W = parseInt(canvas.dataset.cardW, 10);
  if (!CARD_W) return;

  var MIN_ZOOM = 0.25;
  var MAX_ZOOM = 2;
  var GRID_SIZE = 22; // matches the dot grid in the stylesheet
  var FIT_MARGIN = 48;
  var HTML_NS = "http://www.w3.org/1999/xhtml";

  var joint = window.joint;

  // ---- shapes

  // A card is a foreignObject holding the server's own card markup. The rect
  // under it is what JointJS measures and routes around; it is drawn
  // invisible, because the card draws itself.
  var Card = joint.dia.Element.define(
    "yacht.Card",
    {
      size: { width: CARD_W, height: 96 },
      attrs: {
        body: { width: "calc(w)", height: "calc(h)", fill: "transparent", stroke: "none" },
        fo: { width: "calc(w)", height: "calc(h)" },
      },
    },
    {
      markup: [
        { tagName: "rect", selector: "body" },
        {
          tagName: "foreignObject",
          selector: "fo",
          children: [
            {
              tagName: "div",
              namespaceURI: HTML_NS,
              selector: "html",
              className: "canvas-card-host",
            },
          ],
        },
      ],
    }
  );

  // A dependency: from the app that needs something to the app that has it,
  // so the arrowhead points at what is depended on — the way it reads aloud.
  // The wide transparent path under the line is what the pointer finds; a
  // 1.5px line is too thin to hover.
  var Dependency = joint.dia.Link.define(
    "yacht.Dependency",
    {
      attrs: {
        hit: { connection: true, fill: "none", stroke: "transparent", strokeWidth: 14 },
        line: {
          connection: true,
          fill: "none",
          targetMarker: { type: "path", d: "M 8 -4 0 0 8 4 z", class: "canvas-link-arrow" },
        },
      },
    },
    {
      markup: [
        { tagName: "path", selector: "hit", className: "canvas-link-hit" },
        { tagName: "path", selector: "line", className: "canvas-link" },
      ],
      defaultLabel: {
        markup: [
          { tagName: "rect", selector: "labelBody", className: "canvas-link-label-bg" },
          { tagName: "text", selector: "labelText", className: "canvas-link-label" },
        ],
        attrs: {
          labelText: { textAnchor: "middle", textVerticalAnchor: "middle", fontSize: 11 },
          labelBody: {
            ref: "labelText",
            x: "calc(x - 6)",
            y: "calc(y - 3)",
            width: "calc(w + 12)",
            height: "calc(h + 6)",
            rx: 4,
          },
        },
        position: { distance: 0.5 },
      },
    }
  );

  var namespace = { yacht: { Card: Card, Dependency: Dependency }, standard: joint.shapes.standard };
  var graph = new joint.dia.Graph({}, { cellNamespace: namespace });

  var host = document.createElement("div");
  host.className = "canvas-paper";
  canvas.insertBefore(host, inner);

  var paper = new joint.dia.Paper({
    el: host,
    model: graph,
    cellViewNamespace: namespace,
    width: "100%",
    height: "100%",
    gridSize: GRID_SIZE / 2,
    async: false,
    frozen: true,
    background: { color: "transparent" },
    interactive: { linkMove: false, labelMove: false, addLinkFromMagnet: false, vertexAdd: false },
    defaultRouter: { name: "manhattan", args: { padding: 18, step: GRID_SIZE / 2, maximumLoops: 2000 } },
    defaultConnector: { name: "rounded", args: { radius: 10 } },
    defaultAnchor: { name: "center" },
    defaultConnectionPoint: { name: "boundary", args: { offset: 2 } },
    // A card can be dropped anywhere; the origin is the one limit, for the
    // reason the server clamps it — a card dragged past the corner is an
    // overshoot, not a place.
    restrictTranslate: function () {
      return { x: 0, y: 0, width: Infinity, height: Infinity };
    },
  });

  // ---- cells

  var byName = {};
  graphData.nodes.forEach(function (n) {
    var card = new Card({ id: "app:" + n.name, position: { x: n.x, y: n.y }, size: { width: CARD_W, height: n.h } });
    card.set("app", n.name);
    byName[n.name] = card;
  });
  graph.resetCells(
    Object.keys(byName)
      .map(function (k) {
        return byName[k];
      })
      .concat(
        graphData.edges
          .filter(function (e) {
            return byName[e.from] && byName[e.to];
          })
          .map(function (e) {
            var link = new Dependency({ source: { id: byName[e.from].id }, target: { id: byName[e.to].id } });
            var via = e.via || [];
            if (via.length) {
              link.labels([
                { attrs: { labelText: { text: via.length > 2 ? via[0] + " +" + (via.length - 1) : via.join(", ") } } },
              ]);
              link.attr("root/title", e.from + " uses " + via.join(", ") + " from " + e.to);
            }
            return link;
          })
      )
  );

  // Rendered first — a frozen paper has no views to put the cards in — and
  // then the server's cards go into the drawing. Every card has to make it
  // across before the server's picture is hidden; otherwise the drawing is
  // taken down again and the picture stays.
  paper.unfreeze();
  var moves = graph.getElements().map(function (card) {
    var view = card.findView(paper);
    return {
      node: inner.querySelector('[data-node="' + CSS.escape(card.get("app")) + '"]'),
      slot: view && view.findNode("html"),
    };
  });
  if (!moves.every(function (m) { return m.node && m.slot; })) {
    host.remove();
    return;
  }
  moves.forEach(function (m) {
    m.node.style.left = "";
    m.node.style.top = "";
    m.slot.appendChild(m.node);
  });
  inner.hidden = true;
  canvas.classList.add("canvas-drawn");

  // ---- the view

  var view = { x: 0, y: 0, zoom: 1 };
  var viewKey = "yacht.view." + (canvas.dataset.project || "");

  function clamp(n, lo, hi) {
    return Math.min(hi, Math.max(lo, n));
  }

  // persist is false for a move the person did not make — sliding an open
  // card into view — so it is not what the canvas opens at next time.
  function apply(persist) {
    paper.matrix({ a: view.zoom, b: 0, c: 0, d: view.zoom, e: view.x, f: view.y });

    // The grid moves and scales with the drawing. A grid that stayed put
    // would be the one thing on screen insisting the canvas had not moved.
    var step = GRID_SIZE * view.zoom;
    canvas.style.backgroundSize = step + "px " + step + "px";
    canvas.style.backgroundPosition = view.x + "px " + view.y + "px";

    var label = document.querySelector("[data-zoom-level]");
    if (label) label.textContent = Math.round(view.zoom * 100) + "%";

    if (persist === false) return;
    try {
      localStorage.setItem(viewKey, JSON.stringify(view));
    } catch (e) {
      // Private browsing, or storage full. The view still works for this visit.
    }
  }

  // zoomTo keeps the point under the cursor where it is, so getting closer
  // to something does not mean hunting for it again afterwards.
  function zoomTo(next, clientX, clientY) {
    next = clamp(next, MIN_ZOOM, MAX_ZOOM);
    if (next === view.zoom) return;
    var rect = canvas.getBoundingClientRect();
    var ax = clientX === undefined ? rect.width / 2 : clientX - rect.left;
    var ay = clientY === undefined ? rect.height / 2 : clientY - rect.top;
    var gx = (ax - view.x) / view.zoom;
    var gy = (ay - view.y) / view.zoom;
    view.zoom = next;
    view.x = ax - gx * view.zoom;
    view.y = ay - gy * view.zoom;
    apply();
  }

  function fit() {
    var rect = canvas.getBoundingClientRect();
    var box = graph.getBBox();
    if (!box) return;
    view.zoom = clamp(
      Math.min((rect.width - FIT_MARGIN * 2) / box.width, (rect.height - FIT_MARGIN * 2) / box.height, 1),
      MIN_ZOOM,
      MAX_ZOOM
    );
    view.x = (rect.width - box.width * view.zoom) / 2 - box.x * view.zoom;
    view.y = (rect.height - box.height * view.zoom) / 2 - box.y * view.zoom;
    apply();
  }

  var saved = null;
  try {
    saved = JSON.parse(localStorage.getItem(viewKey));
  } catch (e) {
    // Nothing stored, or it was written by an older version.
  }
  if (saved && isFinite(saved.x) && isFinite(saved.y) && saved.zoom) {
    view = { x: saved.x, y: saved.y, zoom: clamp(saved.zoom, MIN_ZOOM, MAX_ZOOM) };
    apply();
  } else {
    fit();
  }
  revealOpenCard();

  // revealOpenCard slides the view so the card whose panel is open sits in
  // the part of the canvas the panel leaves showing, as Railway does. A
  // panel that hid the card it was about would leave nothing on screen
  // saying which app this is, or what it is connected to.
  function revealOpenCard() {
    var open = canvas.querySelector(".canvas-card-open");
    var sheet = document.querySelector("#canvas-sheet .canvas-sheet");
    if (!open || !sheet) return;
    var cardBox = open.getBoundingClientRect();
    var canvasBox = canvas.getBoundingClientRect();
    // Its width rather than where it is: the panel may still be sliding in.
    var visibleRight = canvasBox.right - sheet.offsetWidth - FIT_MARGIN;
    var visibleLeft = canvasBox.left + FIT_MARGIN;
    if (visibleRight - visibleLeft < cardBox.width) return; // a narrow window: nothing fits beside it
    if (cardBox.left >= visibleLeft && cardBox.right <= visibleRight) return;
    // Centred in what is left, keeping the zoom somebody chose.
    view.x += (visibleLeft + visibleRight) / 2 - (cardBox.left + cardBox.right) / 2;
    apply(false);
  }

  var controls = document.querySelector("[data-canvas-controls]");
  if (controls) {
    controls.hidden = false;
    controls.addEventListener("click", function (event) {
      var button = event.target.closest("[data-zoom]");
      if (!button) return;
      switch (button.dataset.zoom) {
        case "in":
          zoomTo(view.zoom + 0.1);
          break;
        case "out":
          zoomTo(view.zoom - 0.1);
          break;
        case "reset":
          zoomTo(1);
          break;
        case "fit":
          fit();
          break;
      }
    });
  }

  // Ctrl or Command with the wheel zooms; the wheel alone moves the view.
  // Browsers report a trackpad pinch as a ctrl-wheel, so pinching works too.
  canvas.addEventListener(
    "wheel",
    function (event) {
      if (event.target.closest("[data-canvas-controls]")) return;
      event.preventDefault();
      if (event.ctrlKey || event.metaKey) {
        zoomTo(view.zoom * (event.deltaY < 0 ? 1.12 : 1 / 1.12), event.clientX, event.clientY);
        return;
      }
      view.x -= event.shiftKey ? event.deltaY : event.deltaX;
      view.y -= event.shiftKey ? 0 : event.deltaY;
      apply();
    },
    { passive: false }
  );

  // Dragging the background moves the view. Unbounded in every direction:
  // that is what makes the canvas infinite rather than a window onto a sheet.
  var pan = null;
  paper.on("blank:pointerdown", function (evt) {
    pan = { clientX: evt.clientX, clientY: evt.clientY, x: view.x, y: view.y };
    canvas.classList.add("canvas-panning");
  });
  window.addEventListener("pointermove", function (event) {
    if (!pan) return;
    view.x = pan.x + (event.clientX - pan.clientX);
    view.y = pan.y + (event.clientY - pan.clientY);
    apply();
  });
  window.addEventListener("pointerup", function () {
    if (!pan) return;
    pan = null;
    canvas.classList.remove("canvas-panning");
  });

  // ---- moving a card

  // A drag that ends on a card still fires a click, and the card is a link,
  // so every drag would finish by opening the panel. It is swallowed on the
  // way past instead.
  var moved = null;
  var swallowClick = false;
  canvas.addEventListener(
    "click",
    function (event) {
      if (!swallowClick) return;
      swallowClick = false;
      event.preventDefault();
      event.stopPropagation();
    },
    true
  );

  paper.on("element:pointerdown", function (view) {
    moved = { card: view.model, from: view.model.position() };
    view.el.classList.add("canvas-card-dragging");
  });
  graph.on("change:position", function (card) {
    if (moved && moved.card === card) moved.dirty = true;
  });
  paper.on("element:pointerup", function (cardView) {
    cardView.el.classList.remove("canvas-card-dragging");
    var drag = moved;
    moved = null;
    if (!drag || !drag.dirty) return;
    swallowClick = true;
    save(cardView);
  });

  // Failure is shown rather than swallowed. The card is already where it was
  // dropped, so a silent failure looks exactly like a save — until the next
  // reload puts it back with nothing to explain why.
  function save(cardView) {
    var card = cardView.model;
    var p = card.position();
    var body = new URLSearchParams();
    body.set("x", String(Math.max(0, Math.round(p.x))));
    body.set("y", String(Math.max(0, Math.round(p.y))));
    fetch("/apps/" + encodeURIComponent(card.get("app")) + "/position", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
      credentials: "same-origin",
    })
      .then(function (response) {
        if (!response.ok) throw new Error("HTTP " + response.status);
        cardView.el.classList.remove("canvas-card-unsaved");
      })
      .catch(function () {
        cardView.el.classList.add("canvas-card-unsaved");
      });
  }

  // ---- the Add menu's GitHub step

  // GitHub Repository turns the menu into a search over the team's
  // repositories, as Railway's does, instead of leaving the canvas for a
  // page. Back returns to the list of things to add.
  var addMenu = document.querySelector(".canvas-add");
  if (addMenu) {
    addMenu.addEventListener("click", function (event) {
      var open = event.target.closest("[data-add-open]");
      var back = event.target.closest("[data-add-back]");
      if (!open && !back) return;
      event.preventDefault();
      var root = addMenu.querySelector('[data-add-step="root"]');
      var step = addMenu.querySelector('[data-add-step="github"]');
      if (!root || !step) return;
      root.hidden = !!open;
      step.hidden = !open;
      if (open) {
        var search = step.querySelector('input[type="search"]');
        search.focus();
        if (window.htmx) window.htmx.trigger(search, "search");
      }
    });
    // An install without a GitHub App has no list to search. Said in the
    // menu, with the way to the page that explains it, rather than an empty
    // box that looks like a team with no repositories.
    addMenu.addEventListener("htmx:responseError", function (event) {
      var rows = addMenu.querySelector("[data-add-rows]");
      var link = addMenu.querySelector("[data-add-open]");
      if (!rows || !link) return;
      rows.innerHTML = "";
      var note = document.createElement("div");
      note.className = "px-3 py-4 text-[12.5px] text-muted-foreground";
      note.textContent = "Repositories could not be listed. ";
      var a = document.createElement("a");
      a.href = link.getAttribute("href");
      a.className = "underline";
      a.textContent = "Open GitHub deploy";
      note.appendChild(a);
      rows.appendChild(note);
    });
  }

  // ---- keeping the cards current

  // Each card's state, asked for every few seconds while the page is in
  // view: a push deploys an app without anybody pressing anything here, and
  // Railway's cards go from Building to Online while you watch. A service
  // added or removed elsewhere changes the picture, which is the server's to
  // lay out — so the page is drawn again, unless a panel is open, where a
  // reload would throw away whatever was being typed into it.
  var statusURL = "/projects/" + encodeURIComponent(canvas.dataset.project || "") +
    "/status?environment=" + encodeURIComponent(canvas.dataset.environment || "");
  function refreshCards() {
    if (document.visibilityState !== "visible") return;
    fetch(statusURL, { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (response) {
        return response.ok ? response.json() : null;
      })
      .then(function (data) {
        if (!data || !data.apps) return;
        var drawn = graph.getElements().map(function (card) {
          return card.get("app");
        });
        var names = Object.keys(data.apps);
        var changed =
          names.length !== drawn.length ||
          names.some(function (name) {
            return drawn.indexOf(name) < 0;
          });
        if (changed) {
          if (!document.querySelector(".canvas-card-open")) location.reload();
          return;
        }
        names.forEach(function (name) {
          var node = canvas.querySelector('[data-node="' + CSS.escape(name) + '"] .canvas-card-status');
          if (!node) return;
          var state = data.apps[name];
          node.className = "canvas-card-status " + state.class;
          var label = node.lastChild;
          if (label && label.nodeType === 3) {
            label.textContent = state.label;
          } else {
            node.appendChild(document.createTextNode(state.label));
          }
        });
      })
      .catch(function () {
        // A missed poll is the next one's to make up.
      });
  }
  setInterval(refreshCards, 5000);
  document.addEventListener("visibilitychange", refreshCards);

  // ---- what a card is connected to

  // Hovering a card lights its arrows and dims the rest, which is the
  // question a canvas of more than three apps is opened to answer.
  function focus(card, on) {
    var related = {};
    graph.getConnectedLinks(card).forEach(function (link) {
      related[link.id] = true;
      var other = link.source().id === card.id ? link.target().id : link.source().id;
      related[other] = true;
    });
    related[card.id] = true;
    canvas.classList.toggle("canvas-focusing", on);
    graph.getCells().forEach(function (cell) {
      var v = cell.findView(paper);
      if (!v) return;
      v.el.classList.toggle("canvas-related", on && !!related[cell.id]);
    });
  }
  paper.on("element:mouseenter", function (v) {
    if (!pan && !moved) focus(v.model, true);
  });
  paper.on("element:mouseleave", function (v) {
    focus(v.model, false);
  });
})();
