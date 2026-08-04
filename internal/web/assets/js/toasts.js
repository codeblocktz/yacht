// Announces and dismisses the message a redirect carried.
//
// The server renders the toast into the live region already, so it is on screen
// and readable before this runs. Two things are left that markup cannot do.
//
// First, announcement. A live region reports what changes inside it, and content
// present in the initial HTML never changed — so a server-rendered toast is
// silent to a screen reader. Re-inserting it after load is the change the region
// needs to see.
//
// Second, dismissal. Without this script the toast simply stays, which is a
// worse default than disappearing only for the messages that can afford to.
(function () {
	"use strict";

	// How long a message stays before it leaves on its own.
	//
	// Failures never leave. A toast is not a place to put something the person
	// has to act on, and a message that says the thing they asked for did not
	// happen is exactly that — it waits to be dismissed deliberately.
	var DISMISS_MS = 6000;

	function announce(host, toast) {
		// Removed and re-added on the next frame. Doing both synchronously is a
		// change the browser may never paint separately, and a region that never
		// saw two states has nothing to announce.
		var assertive = toast.hasAttribute("data-toast-assertive");
		toast.remove();
		window.requestAnimationFrame(function () {
			// Failures interrupt; everything else waits for a pause in whatever
			// the person is doing. Set before insertion so the region is already
			// in the right mode when the content lands.
			host.setAttribute("aria-live", assertive ? "assertive" : "polite");
			host.appendChild(toast);
		});
	}

	function dismiss(toast) {
		toast.remove();
	}

	function init(host) {
		var toast = host.querySelector("[data-toast]");
		if (!toast) return;

		var close = toast.querySelector("[data-toast-close]");
		if (close) {
			close.addEventListener("click", function () {
				dismiss(toast);
			});
		}

		announce(host, toast);

		if (toast.hasAttribute("data-toast-assertive")) return;

		var timer = window.setTimeout(function () {
			dismiss(toast);
		}, DISMISS_MS);

		// Reading it, or tabbed into it, means it is still wanted. Cancelled
		// rather than restarted on leaving: something read once does not need a
		// second full countdown, and re-arming makes a toast the pointer merely
		// crossed outlive one it did not.
		toast.addEventListener("mouseenter", function () {
			window.clearTimeout(timer);
		});
		toast.addEventListener("focusin", function () {
			window.clearTimeout(timer);
		});
	}

	function start() {
		document.querySelectorAll("[data-toast-host]").forEach(init);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", start);
	} else {
		start();
	}
})();
