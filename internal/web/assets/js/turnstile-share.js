// Puts the sign-in page's one Turnstile token in whichever form is sent.
//
// The page has two forms, the password and the emailed link, and one widget
// drawn outside both: a widget in each was two checks for one visitor, one
// saying "Success!" while the other still asked. The widget keeps its token
// in a field of its own; at submit this copies it into the form that is
// going, so the server reads it from that form as it would from a widget
// inside it.
//
// Without this script, or without Cloudflare's, either form goes with no
// token, and the server refuses it and asks for the check: nothing is let
// past it that the widget would have stopped.
(function () {
	"use strict";

	var FIELD = '[name="cf-turnstile-response"]';

	document.addEventListener("submit", function (event) {
		var form = event.target;
		if (!form || !form.hasAttribute || !form.hasAttribute("data-turnstile-share")) {
			return;
		}
		var into = form.querySelector("[data-turnstile-token]");
		var widget = document.querySelector("[data-turnstile-widget]");
		var from = widget && widget.querySelector(FIELD);
		if (into) {
			into.value = from ? from.value : "";
		}
	}, true);
})();
