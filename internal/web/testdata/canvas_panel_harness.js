// Runs canvas-panel.js and templUI's dialog script, as the dashboard serves
// them, on a canvas with the cards and sheets a page rendered, clicks a card,
// and says what became of the page at each step.
//
//   node canvas_panel_harness.js <dialog.min.js> <canvas-panel.js> <page.json>
//
// page.json carries what the server rendered: the card links on the canvas,
// the elements that follow it (the deploy logs sheet and the panel's skeleton
// template), and for each panel address its title and its #canvas-sheet. All
// as element trees: tag, attributes and children, no text. The DOM here is
// the least the two scripts need, with a <dialog> whose showModal and close
// behave as a browser's do, timers that run only when told, and requests that
// answer only when told, so what the page shows while a request is out can be
// looked at. What it prints is one line of JSON, a report per flow.
"use strict";

const fs = require("fs");
const vm = require("vm");

const [dialogFile, panelFile, pageFile] = process.argv.slice(2);
const dialogScript = fs.readFileSync(dialogFile, "utf8");
const panelScript = fs.readFileSync(panelFile, "utf8");
const page = JSON.parse(fs.readFileSync(pageFile, "utf8"));
const ORIGIN = "http://yacht.test";

// ---- selectors: compound selectors (tag, #id, .class, [a], [a="v"],
// :not(...)), the descendant combinator and comma lists.

function parseCompound(src) {
  const parts = { tag: null, id: null, classes: [], attrs: [], not: [] };
  const re = /^(?:([a-zA-Z][\w-]*)|#([\w-]+)|\.([\w-]+)|\[([\w-]+)(?:=(?:"((?:[^"\\]|\\.)*)"|([^\]]*)))?\]|:not\(([^()]*)\))/;
  let s = src;
  while (s.length) {
    const m = re.exec(s);
    if (!m) throw new Error("harness: unsupported selector " + src);
    if (m[1]) parts.tag = m[1].toUpperCase();
    else if (m[2]) parts.id = m[2];
    else if (m[3]) parts.classes.push(m[3]);
    else if (m[4]) parts.attrs.push([m[4], m[5] !== undefined ? m[5].replace(/\\(.)/g, "$1") : m[6]]);
    else if (m[7] !== undefined) parts.not.push(parseCompound(m[7]));
    s = s.slice(m[0].length);
  }
  return parts;
}

function matchCompound(el, p) {
  if (!el || !el.attrs) return false;
  if (p.tag && el.tagName !== p.tag) return false;
  if (p.id && el.id !== p.id) return false;
  if (p.classes.some((c) => !el.classList.contains(c))) return false;
  if (p.attrs.some(([n, v]) => !el.attrs.has(n) || (v !== undefined && el.attrs.get(n) !== v))) return false;
  if (p.not.some((q) => matchCompound(el, q))) return false;
  return true;
}

function splitList(sel) {
  const out = [];
  let depth = 0, quote = false, cur = "";
  for (const ch of sel) {
    if (ch === '"') quote = !quote;
    if (!quote && (ch === "(" || ch === "[")) depth++;
    if (!quote && (ch === ")" || ch === "]")) depth--;
    if (ch === "," && !depth && !quote) { out.push(cur.trim()); cur = ""; } else cur += ch;
  }
  out.push(cur.trim());
  return out;
}

function splitDescendants(sel) {
  const out = [];
  let depth = 0, quote = false, cur = "";
  for (const ch of sel) {
    if (ch === '"') quote = !quote;
    if (!quote && (ch === "(" || ch === "[")) depth++;
    if (!quote && (ch === ")" || ch === "]")) depth--;
    if (/\s/.test(ch) && !depth && !quote) { if (cur) out.push(cur); cur = ""; } else cur += ch;
  }
  if (cur) out.push(cur);
  return out.map(parseCompound);
}

function matches(el, sel) {
  return splitList(sel).some((one) => {
    const chain = splitDescendants(one);
    if (!matchCompound(el, chain[chain.length - 1])) return false;
    let at = el.parentElement;
    for (let i = chain.length - 2; i >= 0; i--) {
      while (at && !matchCompound(at, chain[i])) at = at.parentElement;
      if (!at) return false;
      at = at.parentElement;
    }
    return true;
  });
}

function run(flow) {
  // ---- the document

  const timers = [];
  const observers = [];
  let observing = false;
  const mutated = () => {
    if (observing) return;
    observing = true;
    queueMicrotask(() => { observing = false; observers.forEach((o) => o.fn([])); });
  };

  function camel(attr) {
    return attr.replace(/^data-/, "").replace(/-([a-z])/g, (_, c) => c.toUpperCase());
  }

  class ClassList {
    constructor(el) { this.el = el; }
    get list() { return (this.el.attrs.get("class") || "").split(/\s+/).filter(Boolean); }
    contains(c) { return this.list.includes(c); }
    toggle(c, on) {
      const has = this.contains(c);
      const want = on === undefined ? !has : !!on;
      if (want && !has) this.el.attrs.set("class", this.list.concat(c).join(" "));
      if (!want && has) this.el.attrs.set("class", this.list.filter((x) => x !== c).join(" "));
      return want;
    }
  }

  class Element {
    constructor(tag, attrs) {
      this.tagName = tag.toUpperCase();
      this.attrs = new Map(Object.entries(attrs || {}));
      this.children = [];
      this.parentElement = null;
      this.listeners = {};
      this.open = false;
      this.touched = 0;
      this.classList = new ClassList(this);
      const el = this;
      this.dataset = new Proxy({}, {
        get(_, key) {
          for (const [name, value] of el.attrs) if (name.startsWith("data-") && camel(name) === key) return value;
          return undefined;
        },
        set(_, key, value) {
          el.setAttribute("data-" + key.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase()), String(value));
          return true;
        },
      });
      if (this.tagName === "TEMPLATE") this.content = new Element("#fragment");
    }
    get id() { return this.attrs.get("id") || ""; }
    get href() { return new URL(this.attrs.get("href"), ORIGIN + "/").href; }
    get childNodes() { return this.children.slice(); }
    get offsetWidth() { return 0; }
    getAttribute(n) { return this.attrs.has(n) ? this.attrs.get(n) : null; }
    setAttribute(n, v) { this.attrs.set(n, String(v)); }
    hasAttribute(n) { return this.attrs.has(n); }
    removeAttribute(n) { this.attrs.delete(n); }
    detach() {
      if (this.parentElement) {
        const kids = this.parentElement.children;
        kids.splice(kids.indexOf(this), 1);
        this.parentElement = null;
      }
      return this;
    }
    appendChild(c) { c.detach(); c.parentElement = this; this.children.push(c); mutated(); return c; }
    replaceChildren(...nodes) {
      this.children.forEach((c) => { c.parentElement = null; });
      this.children = [];
      nodes.forEach((n) => this.appendChild(n));
      mutated();
    }
    replaceWith(next) {
      next.detach();
      const kids = this.parentElement.children;
      kids[kids.indexOf(this)] = next;
      next.parentElement = this.parentElement;
      this.parentElement = null;
      mutated();
    }
    after(next) {
      next.detach();
      const kids = this.parentElement.children;
      kids.splice(kids.indexOf(this) + 1, 0, next);
      next.parentElement = this.parentElement;
      mutated();
    }
    cloneNode(deep) {
      const el = new Element(this.tagName, Object.fromEntries(this.attrs));
      if (deep) this.children.forEach((c) => el.appendChild(c.cloneNode(true)));
      if (deep && this.content) this.content.children.forEach((c) => el.content.appendChild(c.cloneNode(true)));
      return el;
    }
    matches(sel) { return matches(this, sel); }
    closest(sel) {
      for (let el = this; el && el.attrs; el = el.parentElement) if (el.matches(sel)) return el;
      return null;
    }
    contains(el) {
      for (; el; el = el.parentElement) if (el === this) return true;
      return false;
    }
    all() { return this.children.flatMap((c) => [c, ...c.all()]); }
    querySelectorAll(sel) {
      const list = this.all().filter((el) => el.matches(sel));
      list.forEach = Array.prototype.forEach;
      return list;
    }
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
    focus() { document.activeElement = this; }
    // A browser's <dialog>: showModal throws on one already open, and puts
    // focus on the first thing in it that takes focus.
    showModal() {
      if (this.open) throw new Error("InvalidStateError");
      this.open = true;
      this.dataOpenAtShow = this.getAttribute("data-tui-dialog-open");
      const first = this.querySelector(FOCUSABLE);
      if (first) first.focus();
    }
    show() { this.open = true; }
    close() {
      if (!this.open) return;
      this.open = false;
      (this.listeners.close || []).forEach((fn) => fn({ type: "close", target: this }));
    }
  }
  const FOCUSABLE = 'a[href], button:not([disabled]), input:not([type="hidden"]):not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])';

  function build(node) {
    const el = new Element(node.tag, node.attrs);
    const into = el.content || el;
    (node.children || []).forEach((c) => into.appendChild(build(c)));
    return el;
  }

  const report = { fetched: [], pushed: [], assigned: [], processed: 0, keepView: null, steps: {} };

  const body = new Element("body");
  const main = body.appendChild(new Element("main"));
  const canvas = main.appendChild(new Element("div", { "data-canvas": "", "data-project": "default", "data-environment": "production" }));
  for (const href of flow.cards || page.cards) {
    const a = canvas.appendChild(new Element("div")).appendChild(new Element("a", { href, class: "canvas-card" }));
    a.appendChild(new Element("span")); // what the pointer lands on
  }
  page.after.forEach((n) => main.appendChild(build(n)));
  // Anything done to the canvas element itself, as opposed to the classes on
  // its cards, is counted: a jump starts with the canvas being redrawn.
  ["replaceWith", "after", "appendChild", "replaceChildren"].forEach((m) => {
    const orig = canvas[m];
    canvas[m] = function () { canvas.touched++; return orig.apply(this, arguments); };
  });

  const location = {
    href: ORIGIN + page.at,
    origin: ORIGIN,
    get pathname() { return new URL(this.href).pathname; },
    get search() { return new URL(this.href).search; },
    assign(url) { report.assigned.push(url); },
    reload() { report.assigned.push("reload"); },
  };
  const popstate = [];
  const document = {
    title: page.title,
    readyState: "complete",
    body,
    activeElement: body,
    listeners: {},
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    querySelector: (sel) => body.querySelector(sel),
    querySelectorAll: (sel) => body.querySelectorAll(sel),
    getElementById: (id) => body.all().find((el) => el.id === id) || null,
    adoptNode: (n) => n.detach(),
    importNode: (n, deep) => n.cloneNode(deep),
  };
  const history = {
    state: null,
    replaceState(state, _, url) { this.state = state; location.href = new URL(url, location.href).href; },
    pushState(state, _, url) { this.state = state; report.pushed.push(url); location.href = new URL(url, location.href).href; },
  };

  // Requests wait to be answered: answer() settles the oldest.
  const requests = [];
  const fetch = (path) => {
    report.fetched.push(path);
    return new Promise((resolve) => requests.push({ path, resolve }));
  };
  function answer() {
    const r = requests.shift();
    if (!r) return settle(); // nothing asked: a click left to the browser
    const panel = page.panels[r.path];
    if (flow.fail || !panel) r.resolve({ ok: false, url: ORIGIN + r.path, text: () => Promise.resolve("") });
    else r.resolve({ ok: true, url: ORIGIN + (flow.redirect || r.path), text: () => Promise.resolve(r.path) });
    return settle();
  }
  // The text a request answers with is the panel's address; parsing it
  // builds that panel's page.
  class DOMParser {
    parseFromString(path) {
      const panel = page.panels[path];
      const sheet = panel ? build(panel.sheet) : null;
      // Which panel is which, for the report: the trees carry no text.
      const content = sheet && sheet.querySelector("[data-tui-dialog-content]");
      if (content && content.children[0]) content.children[0].setAttribute("data-panel", path);
      return { title: panel ? panel.title : "", getElementById: (id) => (sheet && id === sheet.id ? sheet : null) };
    }
  }
  const window = {
    fetch, DOMParser, history, location,
    setTimeout(fn, ms) { timers.push({ fn, ms: ms || 0 }); return timers.length; },
    clearTimeout(id) { if (timers[id - 1]) timers[id - 1].fn = null; },
    getComputedStyle: () => ({ transitionDuration: flow.transition || "0.2s, 0.2s", transitionDelay: "0s" }),
    addEventListener: (type, fn) => type === "popstate" && popstate.push(fn),
    htmx: { process: () => report.processed++ },
  };
  // Timers run when told, and say how long they were set for.
  function runTimers() {
    const due = timers.filter((t) => t.fn);
    const delays = due.map((t) => t.ms);
    due.forEach((t) => { const fn = t.fn; t.fn = null; fn(); });
    return delays;
  }
  class MutationObserver {
    constructor(fn) { this.fn = fn; observers.push(this); }
    observe() {}
  }
  const sessionStorage = { setItem: (k, v) => { if (k === "yacht.keepView") report.keepView = v; } };
  const ctx = { window, document, location, history, fetch, DOMParser, URL, sessionStorage, MutationObserver, console, queueMicrotask };
  vm.createContext(ctx);
  vm.runInContext(dialogScript, ctx);
  vm.runInContext(panelScript, ctx);

  function dispatch(el, type, init) {
    const event = Object.assign({ type, target: el, button: 0, defaultPrevented: false }, init);
    event.preventDefault = () => { event.defaultPrevented = true; };
    for (let at = el; at; at = at.parentElement) (at.listeners[type] || []).forEach((fn) => fn(event));
    (document.listeners[type] || []).forEach((fn) => fn(event));
    return event;
  }
  function clickCard(href, init) {
    const card = canvas.querySelectorAll("a.canvas-card").find((a) => a.getAttribute("href") === href);
    if (!card) throw new Error("harness: no card links to " + href);
    return !dispatch(card.children[0], "click", init || {}).defaultPrevented;
  }
  function settle() { return new Promise((r) => setTimeout(r, 0)); }

  // What the page shows: the sheet, what is in it, and the canvas.
  function look() {
    const sheet = document.getElementById("canvas-sheet");
    const dialog = sheet && sheet.querySelector("[data-tui-dialog-content]");
    const tab = dialog && dialog.querySelector(".canvas-sheet-tabs a.tab-active");
    return {
      sheet: !!sheet,
      sheetAfterLogs: !!sheet && sheet.parentElement === main &&
        main.children.indexOf(sheet) === main.children.indexOf(document.getElementById("deploy-logs-sheet")) + 1,
      dialog: dialog || null,
      open: !!(dialog && dialog.open),
      dataOpen: dialog ? dialog.getAttribute("data-tui-dialog-open") : null,
      dataOpenAtShow: dialog ? dialog.dataOpenAtShow || null : null,
      closing: dialog ? dialog.getAttribute("data-tui-dialog-closing") : null,
      skeletons: dialog ? dialog.querySelectorAll('[data-slot="skeleton"]').length : 0,
      bodySkeletons: dialog ? dialog.querySelectorAll('.canvas-sheet-body [data-slot="skeleton"]').length : 0,
      headSkeletons: dialog ? dialog.querySelectorAll('.canvas-sheet-head [data-slot="skeleton"]').length : 0,
      busy: dialog ? dialog.getAttribute("aria-busy") : null,
      label: dialog ? dialog.getAttribute("aria-labelledby") : null,
      panel: dialog && dialog.querySelector("[data-panel]") ? dialog.querySelector("[data-panel]").getAttribute("data-panel") : null,
      activeTab: tab ? tab.getAttribute("href") : null,
      focusInSheet: !!dialog && dialog.contains(document.activeElement),
      cardsOpen: canvas.querySelectorAll("a.canvas-card").filter((a) => a.classList.contains("canvas-card-open")).map((a) => a.getAttribute("href")),
      title: document.title,
      canvasTouched: canvas.touched,
      path: location.pathname,
    };
  }
  function snap(name, extra) {
    const s = look();
    delete s.dialog;
    report.steps[name] = Object.assign(s, extra || {});
  }

  // Back or forward to url, as a browser goes: the address first, then
  // popstate with the entry's state.
  function travel(url, state) {
    location.href = ORIGIN + url;
    history.state = state;
    popstate.forEach((fn) => fn({ state }));
  }

  return flow.steps({ clickCard, answer, look, snap, runTimers, dispatch, travel, document, report, settle });
}

// ---- the flows

const flows = {
  // A card on a canvas with nothing open: the sheet opens on the click with
  // the panel in outline, the panel replaces the outline when it arrives;
  // back and forward to another service, a tab, then Close.
  open: {
    steps: async (h) => {
      h.report.followed = h.clickCard("/apps/web");
      h.snap("clicked");
      const dialog = h.look().dialog;
      await h.answer();
      h.snap("answered", { sameDialog: h.look().dialog === dialog });

      // Forward to another service's panel, as history does: the sheet stays
      // open, and only a slow answer shows the outline.
      h.travel("/apps/api", { yachtCanvas: true, panel: "/apps/api", title: "" });
      h.snap("switching");
      const delays = h.runTimers();
      h.snap("switchSlow", { delays });
      await h.answer();
      h.snap("switched", { sameDialog: h.look().dialog === dialog });

      // A tab in the panel: swapped in place, the body in outline.
      const tab = h.look().dialog.querySelectorAll(".canvas-sheet-tabs a.tab").find((a) => a.getAttribute("href") === "/apps/api/variables");
      tab.focus();
      h.report.tabFollowed = !h.dispatch(tab, "click", {}).defaultPrevented;
      h.snap("tabClicked");
      h.runTimers();
      h.snap("tabSlow");
      await h.answer();
      h.snap("tabbed", { sameDialog: h.look().dialog === dialog, focusOnTab: h.document.activeElement.getAttribute && h.document.activeElement.getAttribute("href") });

      // Metrics starts its charts on page load, so it is left to the browser.
      const metrics = h.look().dialog.querySelectorAll(".canvas-sheet-tabs a.tab").find((a) => a.getAttribute("href") === "/apps/api/metrics");
      h.report.metricsFollowed = !h.dispatch(metrics, "click", {}).defaultPrevented;
      h.report.metricsKeepView = h.report.keepView;

      // Close: the exit runs, then the sheet is closed.
      const close = h.look().dialog.querySelectorAll("[data-tui-dialog-close]").find((b) => b.closest("[data-tui-dialog]").id === "canvas-sheet");
      h.dispatch(close, "click", {});
      h.snap("closing");
      const closeDelays = h.runTimers();
      h.snap("closed", { delays: closeDelays });
      return h.report;
    },
  },
  // Opened, then back to the canvas.
  back: {
    steps: async (h) => {
      h.clickCard("/apps/web");
      await h.answer();
      h.travel("/projects/default", { yachtCanvas: true, panel: null, title: "Default · Yacht" });
      h.snap("closing");
      h.runTimers();
      h.snap("closed");
      return h.report;
    },
  },
  // A sheet with no transition to wait for, as under reduced motion with
  // the fade gone too: closing does not wait.
  instant: {
    transition: "0s",
    steps: async (h) => {
      h.clickCard("/apps/web");
      await h.answer();
      const close = h.look().dialog.querySelectorAll("[data-tui-dialog-close]").find((b) => b.closest("[data-tui-dialog]").id === "canvas-sheet");
      h.dispatch(close, "click", {});
      h.snap("closed", { delays: h.runTimers() });
      return h.report;
    },
  },
  modified: {
    steps: async (h) => {
      h.report.followed = h.clickCard("/apps/web", { metaKey: true });
      return h.report;
    },
  },
  logs: {
    cards: ["/apps/web", "/apps/api/deployments/00000000-0000-0000-0000-000000000001/logs"],
    steps: async (h) => {
      h.report.followed = h.clickCard("/apps/api/deployments/00000000-0000-0000-0000-000000000001/logs");
      return h.report;
    },
  },
  failed: {
    fail: true,
    steps: async (h) => {
      h.clickCard("/apps/web");
      await h.answer();
      return h.report;
    },
  },
  redirected: {
    redirect: "/login",
    steps: async (h) => {
      h.clickCard("/apps/web");
      await h.answer();
      return h.report;
    },
  },
};

const out = {};
(async () => {
  for (const [name, flow] of Object.entries(flows)) {
    try {
      out[name] = await run(flow);
    } catch (e) {
      out[name] = { error: String(e && e.stack || e) };
    }
  }
  process.stdout.write(JSON.stringify(out) + "\n");
})();
