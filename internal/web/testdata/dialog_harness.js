// Runs templUI's dialog script, as the dashboard serves it, against the
// dialogs a page rendered, and says what became of each.
//
//   node dialog_harness.js <dialog.min.js> <dialogs.json> load|swap [transition]
//
// dialogs.json is the page cut down to the elements the script looks at (any
// with a data-tui-dialog* attribute), nested as they were. The DOM here is the
// least that script needs: attribute selectors, closest, dataset, and a
// <dialog> whose showModal and close set `open` the way a browser's do. What it
// prints is one line of JSON:
//
//   {"opened": {...}, "closing": {...}, "closed": {...}, "closeDelays": [ms...]}
//
// each mapping a dialog root's id to whether its <dialog> is open, the
// data-tui-dialog-open it carries, and the one it carried when it was shown:
// once the script has run, as each open dialog's Close button is clicked, and
// once the close animation's timers have run out. transition is what the
// element's computed transition-duration says (default "0.2s").
//
// load puts the dialogs in the page before the script runs, as a page load or
// a back/forward that reloads does. swap adds them after, as an htmx swap
// does, so they reach the script through its MutationObserver.
"use strict";

const fs = require("fs");
const vm = require("vm");

const [scriptFile, treeFile, mode, transition = "0.2s"] = process.argv.slice(2);
const script = fs.readFileSync(scriptFile, "utf8");
const tree = fs.readFileSync(treeFile, "utf8");
if (mode !== "load" && mode !== "swap") throw new Error("harness: mode is load or swap, not " + mode);

// [a][b="c"] and the like: the only selectors the script uses.
function parseSelector(sel) {
  const parts = [];
  const re = /\[([\w-]+)(?:="([^"]*)")?\]/g;
  let m;
  while ((m = re.exec(sel))) parts.push([m[1], m[2]]);
  if (parts.length === 0) {
    throw new Error("harness: unsupported selector " + sel);
  }
  return parts;
}

function camel(attr) {
  return attr.replace(/^data-/, "").replace(/-([a-z])/g, (_, c) => c.toUpperCase());
}

class Element {
  constructor(tag, attrs) {
    this.tagName = tag.toUpperCase();
    this.attrs = new Map(Object.entries(attrs || {}));
    this.children = [];
    this.parentElement = null;
    this.listeners = {};
    this.open = false;
    const el = this;
    this.dataset = new Proxy({}, {
      get(_, key) {
        for (const [name, value] of el.attrs) {
          if (name.startsWith("data-") && camel(name) === key) return value;
        }
        return undefined;
      },
      set(_, key, value) {
        el.setAttribute("data-" + key.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase()), String(value));
        return true;
      },
    });
  }
  get id() { return this.attrs.get("id") || ""; }
  get offsetWidth() { return 0; }
  getAttribute(n) { return this.attrs.has(n) ? this.attrs.get(n) : null; }
  setAttribute(n, v) { this.attrs.set(n, String(v)); }
  hasAttribute(n) { return this.attrs.has(n); }
  removeAttribute(n) { this.attrs.delete(n); }
  appendChild(c) { c.parentElement = this; this.children.push(c); return c; }
  matches(sel) {
    return parseSelector(sel).every(([n, v]) => this.attrs.has(n) && (v === undefined || this.attrs.get(n) === v));
  }
  closest(sel) {
    for (let el = this; el; el = el.parentElement) if (el.matches && el.matches(sel)) return el;
    return null;
  }
  querySelectorAll(sel) {
    const out = [];
    const walk = (el) => el.children.forEach((c) => { if (c.matches(sel)) out.push(c); walk(c); });
    walk(this);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  dispatch(type) {
    const event = { type, target: this, preventDefault() {} };
    for (let el = this; el; el = el.parentElement) (el.listeners[type] || []).forEach((fn) => fn(event));
    (document.listeners[type] || []).forEach((fn) => fn(event));
  }
  // A browser's <dialog>: showModal throws on one already open.
  showModal() {
    if (this.open) throw new Error("InvalidStateError");
    this.open = true;
    this.dataOpenAtShow = this.getAttribute("data-tui-dialog-open");
  }
  show() { this.open = true; }
  close() {
    if (!this.open) return;
    this.open = false;
    (this.listeners.close || []).forEach((fn) => fn({ type: "close", target: this }));
  }
}

function build(node) {
  const el = new Element(node.tag, node.attrs);
  (node.children || []).forEach((c) => el.appendChild(build(c)));
  return el;
}

const body = new Element("body");
const document = {
  readyState: "complete",
  body,
  listeners: {},
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
  getElementById(id) { return body.querySelectorAll("[id]").find((el) => el.id === id) || null; },
  querySelector(sel) { return body.querySelector(sel); },
  querySelectorAll(sel) { return body.querySelectorAll(sel); },
};

const observers = [];
const timers = [];
const delays = [];
const window = {
  setTimeout(fn, ms) { timers.push(fn); delays.push(ms || 0); return timers.length; },
  clearTimeout() {},
  getComputedStyle: () => ({ transitionDuration: transition, transitionDelay: "0s" }),
};
class MutationObserver {
  constructor(fn) { this.fn = fn; observers.push(this); }
  observe() {}
}

function state() {
  const out = {};
  body.querySelectorAll("[data-tui-dialog]").forEach((root) => {
    const dialog = root.querySelectorAll("[data-tui-dialog-content]").find((d) => d.closest("[data-tui-dialog]") === root);
    out[root.id] = {
      open: !!(dialog && dialog.open),
      dataOpen: dialog ? dialog.getAttribute("data-tui-dialog-open") : null,
      dataOpenAtShow: dialog ? dialog.dataOpenAtShow || null : null,
      closing: dialog ? dialog.getAttribute("data-tui-dialog-closing") : null,
    };
  });
  return out;
}

const roots = JSON.parse(tree);
if (mode === "load") roots.forEach((n) => body.appendChild(build(n)));
vm.runInNewContext(script, { window, document, MutationObserver, console });
if (mode === "swap") {
  roots.forEach((n) => body.appendChild(build(n)));
  observers.forEach((o) => o.fn([]));
}
const result = { opened: state() };

// Each open dialog's own Close button, clicked; then the close animation's
// timer runs out.
body.querySelectorAll("[data-tui-dialog]").forEach((root) => {
  const button = root.querySelectorAll("[data-tui-dialog-close]").find((b) => b.closest("[data-tui-dialog]") === root);
  const dialog = root.querySelectorAll("[data-tui-dialog-content]").find((d) => d.closest("[data-tui-dialog]") === root);
  if (button && dialog && dialog.open) button.dispatch("click");
});
result.closing = state();
result.closeDelays = delays.slice();
timers.splice(0).forEach((fn) => fn());
result.closed = state();

process.stdout.write(JSON.stringify(result) + "\n");
