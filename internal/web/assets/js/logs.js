// Follows an app's log over server-sent events, appending lines as they arrive.
//
// The server renders the tail first, so this only ever adds to what is already
// on the page. If the stream cannot be established it hands the pane back to
// the htmx poll that rendered it, which is why that endpoint still exists: a
// network that eats SSE is otherwise a log pane that never fills and never says
// why.
(function () {
	"use strict";

	// Two failures before giving up. EventSource retries by itself and the
	// first failure is usually a redeploy or a laptop waking, so falling back
	// on one would trade a working stream for a poll at the first hiccup.
	var MAX_FAILURES = 2;

	function init(root) {
		var url = root.getAttribute("data-log-stream");
		if (!url || typeof EventSource === "undefined") return;

		var pane = root.querySelector(".logs");
		if (!pane) return; // nothing rendered yet, the poll will fill it

		// Polling and streaming at once would double every line.
		root.removeAttribute("hx-trigger");

		var failures = 0;
		var source = new EventSource(url);

		source.addEventListener("message", function (e) {
			// Read before appending: appending is what moves it.
			var pinned = atBottom(pane);
			pane.appendChild(lineNode(e.data));
			if (pinned) pane.scrollTop = pane.scrollHeight;
		});

		source.addEventListener("open", function () {
			failures = 0;
		});

		source.addEventListener("error", function () {
			// readyState stays CONNECTING while EventSource retries on its own.
			// Only a closed stream is one it has given up on.
			if (source.readyState !== EventSource.CLOSED) return;
			if (++failures < MAX_FAILURES) return;
			fallBackToPolling(root);
		});

		window.addEventListener("pagehide", function () {
			source.close();
		});
	}

	// atBottom reports whether the reader is following the tail.
	//
	// Someone scrolled up is reading something, and yanking them back to the
	// bottom on every arriving line makes that impossible.
	function atBottom(pane) {
		return pane.scrollHeight - pane.scrollTop - pane.clientHeight < 24;
	}

	function lineNode(text) {
		var line = document.createElement("div");
		line.className = "log-line";
		var body = document.createElement("span");
		body.className = "log-text";
		// textContent, never innerHTML: this is untrusted output from somebody
		// else's container, and it lands on an authenticated page.
		body.textContent = text;
		line.appendChild(body);
		return line;
	}

	function fallBackToPolling(root) {
		root.setAttribute("hx-trigger", "load, every 5s");
		if (window.htmx) window.htmx.process(root);
	}

	function start() {
		document.querySelectorAll("[data-log-stream]").forEach(init);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", start);
	} else {
		start();
	}
})();
