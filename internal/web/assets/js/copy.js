// Copies a value to the clipboard and says so on the button.
//
// Confirmation is a tick on the control rather than a toast. Copying a DNS
// record means pressing three of these in a row, and three toasts for three
// trivial successes would bury the one message that mattered.
//
// Everything this copies is beside text that is already selectable, so with this
// script absent nothing becomes impossible — only slower.
(function () {
	"use strict";

	var RESTORE_MS = 1200;

	function copy(text) {
		if (navigator.clipboard && window.isSecureContext) {
			return navigator.clipboard.writeText(text);
		}
		// A self-hosted install served over plain HTTP is not a secure context,
		// which is most of them before a certificate is configured — exactly the
		// installs reading DNS instructions off this page. The old path still
		// works there.
		return new Promise(function (resolve, reject) {
			var field = document.createElement("textarea");
			field.value = text;
			field.setAttribute("readonly", "");
			field.style.position = "fixed";
			field.style.opacity = "0";
			document.body.appendChild(field);
			field.select();
			var ok = false;
			try {
				ok = document.execCommand("copy");
			} catch (e) {
				ok = false;
			}
			field.remove();
			ok ? resolve() : reject(new Error("copy refused"));
		});
	}

	function onClick(event) {
		var button = event.target.closest("[data-copy]");
		if (!button) return;

		var value = button.getAttribute("data-copy");
		if (!value) return;

		copy(value).then(
			function () {
				button.setAttribute("data-copied", "true");
				window.setTimeout(function () {
					button.removeAttribute("data-copied");
				}, RESTORE_MS);
			},
			function () {
				// Deliberately silent. The text is on screen and selectable, and
				// an error about a convenience failing is noise in front of
				// somebody who can simply highlight it.
			}
		);
	}

	function start() {
		// One delegated listener: these buttons arrive inside htmx fragments
		// that swap every few seconds, and a per-button listener would be lost
		// on the first swap.
		document.addEventListener("click", onClick);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", start);
	} else {
		start();
	}
})();
