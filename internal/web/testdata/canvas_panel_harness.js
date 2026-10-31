// Runs canvas-panel.js, as the dashboard serves it, on a canvas with the
// cards a page rendered, clicks one, and says what became of the page.
//
//   node canvas_panel_harness.js <canvas-panel.js> <page.json>
//
// page.json is {"cards": [href...], "panels": {path: html}}: the card links
// the canvas page rendered, and what the server answers for each panel. The
// DOM here is the least the script needs. What it prints is one line of
// JSON, a report per scenario: whether the click left the page, what was
// fetched and pushed, whether the canvas element itself was touched, and
// where the sheet ended up.
"use strict";

const fs = require("fs");
const vm = require("vm");

const [scriptFile, pageFile] = process.argv.slice(2);
const script = fs.readFileSync(scriptFile, "utf8");
const page = JSON.parse(fs.readFileSync(pageFile, "utf8"));
const ORIGIN = "http://yacht.test";

class ClassList {
  constructor() { this.set = new Set(); }
  toggle(c, on) { (on === undefined ? !this.set.has(c) : on) ? this.set.add(c) : this.set.delete(c); }
  contains(c) { return this.set.has(c); }
}

class Element {
  constructor(tag, attrs) {
    this.tagName = tag.toUpperCase();
    this.attrs = Object.assign({}, attrs);
    this.children = [];
    this.parentElement = null;
    this.classList = new ClassList();
    this.listeners = {};
    this.touched = 0; // anything done to the element itself, not its children
  }
  get id() { return this.attrs.id || ""; }
  get href() { return new URL(this.attrs.href, ORIGIN + "/").href; }
  getAttribute(n) { return n in this.attrs ? this.attrs[n] : null; }
  appendChild(c) { c.parentElement = this; this.children.push(c); return c; }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  closest(sel) {
    for (let el = this; el; el = el.parentElement) if (el.matches(sel)) return el;
    return null;
  }
  matches(sel) {
    if (sel === "a.canvas-card") return this.tagName === "A" && this.classList.contains("canvas-card");
    if (sel === "[data-canvas]") return "data-canvas" in this.attrs;
    throw new Error("harness: unsupported selector " + sel);
  }
  contains(el) {
    for (; el; el = el.parentElement) if (el === this) return true;
    return false;
  }
  all() { return this.children.flatMap((c) => [c, ...c.all()]); }
  querySelectorAll(sel) { return this.all().filter((el) => el.matches(sel)); }
  replaceWith(next) {
    const kids = this.parentElement.children;
    kids[kids.indexOf(this)] = next;
    next.parentElement = this.parentElement;
    this.parentElement = null;
  }
  after(next) {
    const kids = this.parentElement.children;
    kids.splice(kids.indexOf(this) + 1, 0, next);
    next.parentElement = this.parentElement;
  }
}

function dispatch(el, type, init) {
  const event = Object.assign({ target: el, button: 0, defaultPrevented: false }, init);
  event.preventDefault = () => { event.defaultPrevented = true; };
  for (let at = el; at; at = at.parentElement) (at.listeners[type] || []).forEach((fn) => fn(event));
  return event;
}

// One canvas page, as the server renders /projects/<slug>, with nothing open.
function run(scenario) {
  const body = new Element("body");
  const main = body.appendChild(new Element("main"));
  const canvas = main.appendChild(new Element("div", { "data-canvas": "" }));
  for (const href of scenario.cards) {
    const a = canvas.appendChild(new Element("div")).appendChild(new Element("a", { href }));
    a.classList.toggle("canvas-card", true);
    a.appendChild(new Element("span")); // what the pointer lands on
  }
  main.appendChild(new Element("div", { id: "deploy-logs-sheet" }));
  // Anything done to the canvas element itself, as opposed to the classes on
  // its cards, is counted: a jump starts with the canvas being redrawn.
  const touch = () => canvas.touched++;
  ["replaceWith", "after", "appendChild"].forEach((m) => {
    const orig = canvas[m];
    canvas[m] = function () { touch(); return orig.apply(this, arguments); };
  });

  const report = { fetched: [], pushed: [], assigned: [], processed: 0, closed: 0, keepView: null };
  const location = {
    href: ORIGIN + scenario.at,
    origin: ORIGIN,
    get pathname() { return new URL(this.href).pathname; },
    assign(url) { report.assigned.push(url); },
  };
  const popstate = [];
  const document = {
    title: "Default · Yacht",
    body,
    querySelector: (sel) => body.querySelectorAll(sel)[0] || null,
    getElementById: (id) => body.all().find((el) => el.id === id) || null,
    adoptNode: (n) => n,
  };
  const history = {
    state: null,
    replaceState(state, _, url) { this.state = state; location.href = new URL(url, location.href).href; },
    pushState(state, _, url) { this.state = state; report.pushed.push(url); location.href = new URL(url, location.href).href; },
  };
  // The panel the server sends: only whether the page carries the sheet, at
  // the id the script takes, and its title, are read from it.
  class DOMParser {
    parseFromString(text) {
      const sheet = /<div id="canvas-sheet"[^>]*data-tui-dialog/.test(text) ? new Element("div", { id: "canvas-sheet", "data-from": "response" }) : null;
      const title = (/<title>([^<]*)<\/title>/.exec(text) || [])[1] || "";
      return { title, getElementById: (id) => (id === "canvas-sheet" ? sheet : null) };
    }
  }
  const fetch = (path) => {
    report.fetched.push(path);
    const html = scenario.panels[path];
    if (scenario.fail || html === undefined) return Promise.resolve({ ok: false, url: ORIGIN + path, text: () => Promise.resolve("") });
    return Promise.resolve({ ok: true, url: ORIGIN + (scenario.redirect || path), text: () => Promise.resolve(html) });
  };
  const window = {
    fetch, DOMParser, history, location,
    addEventListener: (type, fn) => type === "popstate" && popstate.push(fn),
    htmx: { process: () => report.processed++ },
    tui: { dialog: { close: () => report.closed++ } },
  };
  const sessionStorage = { setItem: (k, v) => { if (k === "yacht.keepView") report.keepView = v; } };
  const ctx = { window, document, location, history, fetch, DOMParser, URL, sessionStorage };
  vm.runInNewContext(script, ctx);

  const card = canvas.querySelectorAll("a.canvas-card").find((a) => a.attrs.href === scenario.click);
  if (!card) throw new Error("harness: no card links to " + scenario.click);
  const click = dispatch(card.children[0], "click", scenario.modifier ? { [scenario.modifier]: true } : {});
  report.followed = !click.defaultPrevented;

  const settle = () => new Promise((r) => setTimeout(r, 0));
  return settle().then(() => {
    const sheet = document.getElementById("canvas-sheet");
    report.sheet = sheet ? sheet.getAttribute("data-from") : null;
    report.sheetAfterLogs = !!sheet && sheet.parentElement === main &&
      main.children.indexOf(sheet) === main.children.indexOf(document.getElementById("deploy-logs-sheet")) + 1;
    report.open = canvas.querySelectorAll("a.canvas-card").filter((a) => a.classList.contains("canvas-card-open")).map((a) => a.attrs.href);
    report.title = document.title;
    report.canvasTouched = canvas.touched;
    if (!report.pushed.length) return report;
    // Back, to the canvas the click was made on.
    popstate.forEach((fn) => fn({ state: { yachtCanvas: true, panel: null, title: "Default · Yacht" } }));
    report.back = {
      closed: report.closed,
      open: canvas.querySelectorAll("a.canvas-card").filter((a) => a.classList.contains("canvas-card-open")).length,
      title: document.title,
    };
    return report;
  });
}

const out = {};
Promise.all(Object.entries(page.scenarios).map(([name, s]) =>
  run(Object.assign({ cards: page.cards, panels: page.panels, at: page.at }, s)).then((r) => { out[name] = r; })
)).then(() => process.stdout.write(JSON.stringify(out) + "\n"));
