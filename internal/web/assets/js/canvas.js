// Dragging cards around the canvas.
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

  var CARD_W = parseInt(canvas.dataset.cardW, 10);
  var CARD_H = parseInt(canvas.dataset.cardH, 10);
  var VOLUME_H = parseInt(canvas.dataset.volumeH, 10);
  if (!CARD_W || !CARD_H) return;

  var inner = canvas.querySelector(".canvas-inner");

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

  var drag = null;

  canvas.addEventListener("pointerdown", function (event) {
    var handle = event.target.closest("[data-drag-handle]");
    if (!handle || event.button !== 0) return;

    var node = handle.closest("[data-node]");
    if (!node) return;

    // Stops the browser from starting its own link-drag, and stops the click
    // that ends the gesture from following the card's href.
    event.preventDefault();
    handle.setPointerCapture(event.pointerId);

    var start = box(node);
    drag = {
      node: node,
      handle: handle,
      pointerId: event.pointerId,
      offsetX: event.clientX - start.x,
      offsetY: event.clientY - start.y,
      moved: false,
    };
    node.classList.add("canvas-node-dragging");
  });

  canvas.addEventListener("pointermove", function (event) {
    if (!drag || event.pointerId !== drag.pointerId) return;

    // Clamped at the origin for the reason the server clamps it: a card
    // dragged past the top-left corner is an overshoot, not a request to be
    // put somewhere the canvas does not extend to.
    var x = Math.max(0, Math.round(event.clientX - drag.offsetX));
    var y = Math.max(0, Math.round(event.clientY - drag.offsetY));

    drag.node.style.left = x + "px";
    drag.node.style.top = y + "px";
    drag.moved = true;
    redrawEdges(drag.node.dataset.node);
  });

  function endDrag(event) {
    if (!drag || event.pointerId !== drag.pointerId) return;

    var node = drag.node;
    var moved = drag.moved;
    node.classList.remove("canvas-node-dragging");
    if (drag.handle.hasPointerCapture(event.pointerId)) {
      drag.handle.releasePointerCapture(event.pointerId);
    }
    drag = null;
    if (!moved) return;

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
