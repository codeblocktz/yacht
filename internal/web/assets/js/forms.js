// Marks a submitted form's button as busy until the page goes away.
//
// Every mutation in this dashboard is a form POST, and several of them block on
// the cluster: creating an app applies a namespace, a Deployment, a Service and
// an Ingress before it answers. Without this the only sign anything happened is
// the browser's own tab spinner, and a button that still looks pressable is one
// people press again — which submits the form twice.
//
// Nothing here is required to submit anything. With this script absent the form
// posts exactly as it always did; what is lost is the feedback, not the action.
(function () {
	"use strict";

	function onSubmit(event) {
		var form = event.target;
		if (!form || form.tagName !== "FORM") return;

		// The confirmation dialog cancels the first submit and re-fires it after
		// the person confirms. Marking the button busy on that first pass would
		// leave it spinning behind an open dialog they may well cancel.
		if (form.dataset.confirmed === "false") return;

		var button = event.submitter;
		if (!button || !button.matches("button, input[type=submit]")) {
			button = form.querySelector("button[type=submit], button:not([type])");
		}
		if (!button) return;

		// aria-busy rather than disabled. A disabled submit button is not
		// submitted with the form, so a handler reading which button was pressed
		// — the cordon forms send their intent that way — would stop receiving
		// it. The pointer-events rule in the stylesheet is what actually blocks
		// the second click.
		button.setAttribute("aria-busy", "true");

		// A form that navigates away never needs clearing. One that does not —
		// because the browser restored the page from cache when somebody came
		// back — would otherwise stay spinning forever.
		window.addEventListener("pageshow", function () {
			button.removeAttribute("aria-busy");
		});
	}

	function start() {
		// One listener on the document rather than one per form: htmx swaps
		// fragments in constantly, and a per-form listener would miss every form
		// that arrived after load.
		document.addEventListener("submit", onSubmit);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", start);
	} else {
		start();
	}
})();
