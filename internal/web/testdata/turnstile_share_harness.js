// Runs turnstile-share.js, as the dashboard serves it, against the forms the
// sign-in page rendered, standing in for Cloudflare's widget, and says what
// each form would send.
//
//   node turnstile_share_harness.js <turnstile-share.js> <page.json> [token]
//
// page.json is the page cut down to its forms, their inputs and anything with
// a data-turnstile* attribute, nested as they were. The widget's part is
// played as Cloudflare's script plays it: once the check passes it puts an
// input named cf-turnstile-response, holding the token, inside each widget.
// With no token the check has not passed, and nothing is put there.
//
// Each form is then submitted, the event going to the document's listeners
// as a real submit bubbles to them, and what it prints is one line of JSON:
//
//   [{"action": "/sign-in", "fields": {"email": "", "cf-turnstile-response": "…"}}, …]
//
// the fields being every named input inside that form, as the browser would
// post them.
"use strict";

const fs = require("fs");
const vm = require("vm");

const [scriptFile, treeFile, token = ""] = process.argv.slice(2);
const script = fs.readFileSync(scriptFile, "utf8");
const tree = JSON.parse(fs.readFileSync(treeFile, "utf8"));

// [a], [a="b"], and compounds of them: the only selectors the script uses.
function parseSelector(sel) {
  const parts = [];
  const re = /\[([\w-]+)(?:="([^"]*)")?\]/g;
  let m;
  let seen = "";
  while ((m = re.exec(sel))) {
    parts.push([m[1], m[2]]);
    seen += m[0];
  }
  if (parts.length === 0 || seen !== sel) {
    throw new Error("harness: unsupported selector " + sel);
  }
  return parts;
}

class Element {
  constructor(tag, attrs) {
    this.tagName = tag.toUpperCase();
    this.attrs = new Map(Object.entries(attrs || {}));
    this.children = [];
    this.parentElement = null;
    this.value = this.attrs.get("value") || "";
  }
  hasAttribute(name) { return this.attrs.has(name); }
  getAttribute(name) { return this.attrs.has(name) ? this.attrs.get(name) : null; }
  appendChild(c) { c.parentElement = this; this.children.push(c); return c; }
  matches(sel) {
    return parseSelector(sel).every(([k, v]) => this.attrs.has(k) && (v === undefined || this.attrs.get(k) === v));
  }
  *descendants() {
    for (const c of this.children) {
      yield c;
      yield* c.descendants();
    }
  }
  querySelector(sel) {
    for (const d of this.descendants()) if (d.matches(sel)) return d;
    return null;
  }
}

const root = new Element("body");
function build(nodes, parent) {
  for (const n of nodes) build(n.children || [], parent.appendChild(new Element(n.tag, n.attrs)));
}
build(tree, root);

const listeners = [];
const document = {
  querySelector: (sel) => root.querySelector(sel),
  addEventListener: (type, fn) => { if (type === "submit") listeners.push(fn); },
};

// Cloudflare's part: a passed check leaves its token inside the widget.
if (token) {
  for (const d of root.descendants()) {
    if (d.hasAttribute("data-turnstile-widget")) {
      d.appendChild(new Element("input", { type: "hidden", name: "cf-turnstile-response", value: token }));
    }
  }
}

vm.runInNewContext(script, { document, window: {} });

const out = [];
for (const form of [...root.descendants()].filter((d) => d.tagName === "FORM")) {
  for (const fn of listeners) fn({ type: "submit", target: form });
  const fields = {};
  for (const d of form.descendants()) {
    if (d.tagName === "INPUT" && d.hasAttribute("name")) fields[d.getAttribute("name")] = d.value;
  }
  out.push({ action: form.getAttribute("action"), fields });
}
process.stdout.write(JSON.stringify(out));
