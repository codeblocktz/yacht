// Moving around the canvas: panning, zooming, and dragging cards.
//
// The canvas renders and navigates without this file. Every card is a link and
// every position comes from the server, so with the script blocked you get a
// static picture you can still click through — which is the reason the layout
// is computed in Go rather than here.
//
// The geometry is read off the canvas element rather than repeated. The server
// laid the cards out with those numbers and drew the initial edges from them;
// a second copy in this file would be correct until somebody changed one of
// them, and then it would be wrong in a way nothing tests.
(function () {
  "use strict";

  var canvas = document.querySelector("[data-canvas]");
  if (!canvas) return;

  var stage = canvas.querySelector("[data-canvas-stage]");
  var inner = canvas.querySelector(".canvas-inner");
  if (!stage || !inner) return;

  var CARD_W = parseInt(canvas.dataset.cardW, 10);
  var CARD_H = parseInt(canvas.dataset.cardH, 10);
  var VOLUME_H = parseInt(canvas.dataset.volumeH, 10);
  if (!CARD_W || !CARD_H) return;

  var MIN_ZOOM = 0.25;
  var MAX_ZOOM = 2;
  var GRID_SIZE = 22; // matches the dot grid in the stylesheet

  var GRAPH_W = inner.offsetWidth;
  var GRAPH_H = inner.offsetHeight;

  // Zoom is this person's view of the canvas, not the team's arrangement of
  // it, so it stays in the browser rather than going to the server the way a
  // dragged position does.
  var zoomKey = "yacht.zoom." + (canvas.dataset.project || "");
  var zoom = 1;

  // ---- zoom

  function clamp(n, lo, hi) {
    return Math.min(hi, Math.max(lo, n));
  }

  function applyZoom() {
    inner.style.transformOrigin = "0 0";
    inner.style.transform = "scale(" + zoom + ")";
    stage.style.width = Math.round(GRAPH_W * zoom) + "px";
    stage.style.height = Math.round(GRAPH_H * zoom) + "px";
    // The dot grid scales too. A fixed grid under a scaled graph reads as the
    // cards having changed size rather than as the view having moved.
    canvas.style.backgroundSize = GRID_SIZE * zoom + "px " + GRID_SIZE * zoom + "px";

    var label = document.querySelector("[data-zoom-level]");
    if (label) label.textContent = Math.round(zoom * 100) + "%";
    try {
      localStorage.setItem(zoomKey, String(zoom));
    } catch (e) {
      // Private browsing, or storage full. Zooming still works for this visit.
    }
  }

  // setZoom keeps the point under the cursor where it is.
  //
  // Without the anchor, zooming always pulls toward the top-left corner, so
  // getting closer to something on the right means zooming in and then hunting
  // for it again.
  function setZoom(next, clientX, clientY) {
    next = clamp(next, MIN_ZOOM, MAX_ZOOM);
    if (next === zoom) return;

    var rect = canvas.getBoundingClientRect();
    var ax = clientX === undefined ? rect.width / 2 : clientX - rect.left;
    var ay = clientY === undefined ? rect.height / 2 : clientY - rect.top;

    var gx = (canvas.scrollLeft + ax) / zoom;
    var gy = (canvas.scrollTop + ay) / zoom;

    zoom = next;
    applyZoom();

    canvas.scrollLeft = gx * zoom - ax;
    canvas.scrollTop = gy * zoom - ay;
  }

  function fit() {
    var rect = canvas.getBoundingClientRect();
    var next = Math.min(rect.width / GRAPH_W, rect.height / GRAPH_H, 1);
    zoom = clamp(next, MIN_ZOOM, MAX_ZOOM);
    applyZoom();
    canvas.scrollLeft = 0;
    canvas.scrollTop = 0;
  }

  try {
    var saved = parseFloat(localStorage.getItem(zoomKey));
    if (saved) zoom = clamp(saved, MIN_ZOOM, MAX_ZOOM);
  } catch (e) {
    // Nothing stored, or storage unavailable.
  }
  applyZoom();

  var controls = document.querySelector("[data-zoom-controls]");
  if (controls) {
    controls.hidden = false;
    controls.addEventListener("click", function (event) {
      var button = event.target.closest("[data-zoom]");
      if (!button) return;
      switch (button.dataset.zoom) {
        case "in":
          setZoom(zoom + 0.1);
          break;
        case "out":
          setZoom(zoom - 0.1);
          break;
        case "reset":
          setZoom(1);
          break;
        case "fit":
          fit();
          break;
      }
    });
  }

  // Ctrl or Command with the wheel zooms; the wheel alone scrolls, which is
  // what the scroll container already does. Browsers report a trackpad pinch
  // as a ctrl-wheel, so pinching works without a second code path.
  canvas.addEventListener(
    "wheel",
    function (event) {
      if (!event.ctrlKey && !event.metaKey) return;
      event.preventDefault();
      setZoom(zoom * (event.deltaY < 0 ? 1.12 : 1 / 1.12), event.clientX, event.clientY);
    },
    { passive: false }
  );

  // ---- geometry

  function nodeAt(name) {
    return inner.querySelector('[data-node="' + CSS.escape(name) + '"]');
  }

  function box(node) {
    var height = node.dataset.hasVolume === "true" ? CARD_H + VOLUME_H : CARD_H;
    return {
      x: parseInt(node.style.left, 10) || 0,
      y: parseInt(node.style.top, 10) || 0,
      h: height,
    };
  }

  // The same route the server draws: out of the top of the card that needs
  // something, into the bottom of the card that provides it. Kept identical on
  // purpose — an edge that changed shape the moment you touched a card would
  // read as the connection itself having changed.
  function route(from, to) {
    var x1 = from.x + CARD_W / 2;
    var y1 = from.y;
    var x2 = to.x + CARD_W / 2;
    var y2 = to.y + to.h;
    var mid = Math.round((y1 + y2) / 2);
    return "M" + x1 + " " + y1 + " V" + mid + " H" + x2 + " V" + y2;
  }

  function redrawEdges(name) {
    var paths = inner.querySelectorAll(
      '[data-from="' + CSS.escape(name) + '"], [data-to="' + CSS.escape(name) + '"]'
    );
    paths.forEach(function (path) {
      var from = nodeAt(path.dataset.from);
      var to = nodeAt(path.dataset.to);
      if (!from || !to) return;
      path.setAttribute("d", route(box(from), box(to)));
    });
  }

  // ---- dragging a card, and panning the canvas

  var drag = null;
  var pan = null;

  // A drag that ends on a link still fires a click, and the link is the card,
  // so every drag used to finish by opening the panel. preventDefault on
  // pointerdown does not stop it: the click is a separate event that arrives
  // after pointerup regardless. It has to be swallowed on the way past.
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

  canvas.addEventListener("pointerdown", function (event) {
    if (event.button !== 0) return;

    var handle = event.target.closest("[data-drag-handle]");
    if (handle) {
      var node = handle.closest("[data-node]");
      if (!node) return;
      event.preventDefault();
      canvas.setPointerCapture(event.pointerId);
      var start = box(node);
      drag = {
        node: node,
        pointerId: event.pointerId,
        clientX: event.clientX,
        clientY: event.clientY,
        startX: start.x,
        startY: start.y,
        moved: false,
      };
      node.classList.add("canvas-node-dragging");
      return;
    }

    // Anywhere that is not a card pans the view. Dragging the background is
    // how every canvas works, and it costs nothing here: the background has
    // nothing else to click.
    if (event.target.closest("[data-node]")) return;
    event.preventDefault();
    canvas.setPointerCapture(event.pointerId);
    pan = {
      pointerId: event.pointerId,
      clientX: event.clientX,
      clientY: event.clientY,
      left: canvas.scrollLeft,
      top: canvas.scrollTop,
    };
    canvas.classList.add("canvas-panning");
  });

  canvas.addEventListener("pointermove", function (event) {
    if (pan && event.pointerId === pan.pointerId) {
      canvas.scrollLeft = pan.left - (event.clientX - pan.clientX);
      canvas.scrollTop = pan.top - (event.clientY - pan.clientY);
      return;
    }
    if (!drag || event.pointerId !== drag.pointerId) return;

    // Divided by the zoom: at 50% the pointer covers twice the canvas for the
    // same distance on screen, and a card that ignored that would slide out
    // from under the cursor.
    var x = Math.round(drag.startX + (event.clientX - drag.clientX) / zoom);
    var y = Math.round(drag.startY + (event.clientY - drag.clientY) / zoom);

    // Clamped at the origin for the reason the server clamps it: a card
    // dragged past the top-left corner is an overshoot, not a request to be
    // put somewhere the canvas does not extend to.
    x = Math.max(0, x);
    y = Math.max(0, y);

    drag.node.style.left = x + "px";
    drag.node.style.top = y + "px";
    drag.moved = true;
    redrawEdges(drag.node.dataset.node);
  });

  function release(event) {
    if (canvas.hasPointerCapture(event.pointerId)) {
      canvas.releasePointerCapture(event.pointerId);
    }
  }

  function endDrag(event) {
    if (pan && event.pointerId === pan.pointerId) {
      pan = null;
      canvas.classList.remove("canvas-panning");
      release(event);
      return;
    }
    if (!drag || event.pointerId !== drag.pointerId) return;

    var node = drag.node;
    var moved = drag.moved;
    node.classList.remove("canvas-node-dragging");
    release(event);
    drag = null;
    if (!moved) return;

    swallowClick = true;

    var position = box(node);
    var body = new URLSearchParams();
    body.set("x", String(position.x));
    body.set("y", String(position.y));

    // Failure is reported rather than swallowed. The card is already where the
    // person dropped it, so a silent failure looks exactly like a save — until
    // the next reload puts it back and there is nothing to explain why.
    fetch("/apps/" + encodeURIComponent(node.dataset.node) + "/position", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
      credentials: "same-origin",
    })
      .then(function (response) {
        if (!response.ok) throw new Error("HTTP " + response.status);
        node.classList.remove("canvas-node-unsaved");
      })
      .catch(function () {
        node.classList.add("canvas-node-unsaved");
      });
  }

  canvas.addEventListener("pointerup", endDrag);
  canvas.addEventListener("pointercancel", endDrag);
})();
