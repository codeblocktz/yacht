// Shows and hides a variable's value, the way Railway's eye button does.
//
// Every value on the Variables tab is drawn masked. A plain one is already in
// the page, hidden by the stylesheet, so showing it is only this toggle. A
// secret is not in the page at all: its button also carries an hx-get that
// fetches the value the first time it is pressed, which is the request the
// server logs. Hiding it again keeps what was fetched rather than asking
// twice — but it was never in the page before somebody asked for it, which is
// the part that matters to whoever is looking over their shoulder.
//
// With this script absent the values stay masked. Nothing breaks; nothing is
// shown that was not asked for.
(function () {
	"use strict";

	function onClick(event) {
		var button = event.target.closest("[data-reveal]");
		if (!button) return;
		var row = button.closest("[data-var]");
		if (!row) return;

		var shown = row.hasAttribute("data-revealed");
		if (shown) {
			row.removeAttribute("data-revealed");
		} else {
			row.setAttribute("data-revealed", "");
		}
		button.setAttribute("aria-pressed", shown ? "false" : "true");
		button.setAttribute(
			"aria-label",
			(shown ? "Show " : "Hide ") + (button.getAttribute("data-reveal") || "value")
		);
	}

	function start() {
		// Delegated, so rows that arrive in a swapped fragment work too.
		document.addEventListener("click", onClick);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", start);
	} else {
		start();
	}
})();
