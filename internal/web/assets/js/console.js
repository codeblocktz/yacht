// The Console tab's terminal: xterm.js on one side, a shell in the app's
// container on the other, and a WebSocket between them.
//
// The protocol is small on purpose. Binary frames are terminal bytes, in both
// directions, and nothing else; text frames are JSON the console itself says —
// the terminal's size going up, "connected", an error or why it closed coming
// down. Keeping the two apart means a program printing JSON is only printing.
(function () {
	"use strict";

	// Input is sent in pieces well under the server's frame limit, so a large
	// paste arrives as several frames rather than closing the connection.
	var CHUNK = 16 * 1024;

	// Network drops are retried this many times, backing off. Anything the
	// server ended on purpose — the shell exited, a limit, a refusal — is not:
	// it said why, and reconnecting on its own would hide that.
	var RETRIES = 5;

	// The server's close codes, from console.go.
	var CLOSE_NO_POD = 4004;

	function init(root) {
		if (root.dataset.consoleReady) return;
		root.dataset.consoleReady = "true";

		var screen = root.querySelector("[data-console-screen]");
		var status = root.querySelector("[data-console-status]");
		var note = root.querySelector("[data-console-note]");
		var reconnect = root.querySelector("[data-console-reconnect]");
		var full = root.querySelector("[data-console-fullscreen]");
		var fullLabel = root.querySelector("[data-console-fullscreen-label]");
		var podLabel = root.querySelector("[data-console-pod-label]");

		if (typeof window.Terminal === "undefined" || !window.FitAddon) {
			setState("closed", "Unavailable");
			say("The terminal could not load. Reload the page to try again.");
			return;
		}

		var pod = root.getAttribute("data-console-pod") || "";
		var term = new window.Terminal({
			cursorBlink: true,
			fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
			fontSize: 13,
			scrollback: 5000,
			theme: { background: "#0c0c0e", foreground: "#e4e4e7", cursor: "#e4e4e7" },
		});
		var fit = new window.FitAddon.FitAddon();
		term.loadAddon(fit);
		term.open(screen);
		refit();

		var encoder = new TextEncoder();
		var ws = null;
		var failures = 0;
		var retry = null;
		var ended = false; // the server said why it closed
		var shells = 0;

		function socketURL() {
			var u = new URL(root.getAttribute("data-console-url"), window.location.href);
			// Same host and port as the page, so the CSP's connect-src covers
			// it and the session cookie goes with it.
			u.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
			if (pod) u.searchParams.set("pod", pod);
			return u.toString();
		}

		function connect() {
			window.clearTimeout(retry);
			ended = false;
			reconnect.hidden = true;
			say("");
			setState("connecting", failures ? "Reconnecting" : "Connecting");

			var sock = new WebSocket(socketURL());
			sock.binaryType = "arraybuffer";
			ws = sock;

			sock.addEventListener("open", sendSize);
			sock.addEventListener("message", function (e) {
				if (typeof e.data !== "string") {
					term.write(new Uint8Array(e.data));
					return;
				}
				var m;
				try {
					m = JSON.parse(e.data);
				} catch (_) {
					return;
				}
				if (m.type === "connected") {
					failures = 0;
					if (m.pod) {
						pod = m.pod;
						if (podLabel) podLabel.textContent = m.pod;
					}
					// A reconnect is a new shell, not the old one resumed. Said
					// in the scrollback so what is above it is not mistaken for
					// this shell's history.
					if (shells++ > 0) term.write("\r\n\x1b[2m— new shell in " + pod + " —\x1b[0m\r\n");
					setState("connected", "Connected");
					term.focus();
				} else if (m.type === "error" || m.type === "closed") {
					ended = true;
					say(m.message || "");
				}
			});
			sock.addEventListener("close", function (e) {
				if (ws !== sock) return;
				ws = null;
				setState("closed", "Disconnected");

				// The pod asked for is gone, most often replaced by a deploy.
				// Once, without naming one, the server picks what is running.
				if (e.code === CLOSE_NO_POD && pod) {
					pod = "";
					connect();
					return;
				}
				if (ended || e.code === 1000 || e.code >= 4000) {
					reconnect.hidden = false;
					return;
				}
				if (document.visibilityState === "hidden" || ++failures > RETRIES) {
					say("The connection dropped.");
					reconnect.hidden = false;
					return;
				}
				setState("connecting", "Reconnecting");
				retry = window.setTimeout(connect, Math.min(500 * Math.pow(2, failures), 10000));
			});
		}

		function send(bytes) {
			if (!ws || ws.readyState !== WebSocket.OPEN) return;
			for (var i = 0; i < bytes.length; i += CHUNK) {
				ws.send(bytes.subarray(i, i + CHUNK));
			}
		}

		function sendSize() {
			if (!ws || ws.readyState !== WebSocket.OPEN) return;
			ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
		}

		function refit() {
			// The fit addon measures the element, and a sheet still sliding in
			// or a hidden tab has no size worth measuring.
			if (!screen.clientWidth || !screen.clientHeight) return;
			try {
				fit.fit();
			} catch (_) {}
		}

		function setState(state, text) {
			if (!status) return;
			status.dataset.state = state;
			status.textContent = text;
		}

		function say(text) {
			if (!note) return;
			note.textContent = text;
			note.hidden = !text;
		}

		term.onData(function (data) {
			send(encoder.encode(data));
		});
		// Some mouse reports are raw bytes rather than text, and encoding them
		// as UTF-8 would turn every byte above 127 into two.
		term.onBinary(function (data) {
			var bytes = new Uint8Array(data.length);
			for (var i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) & 0xff;
			send(bytes);
		});
		term.onResize(sendSize);

		// Escape belongs to the shell — vim, less, a readline prompt. Left to
		// bubble it is also the sheet's close request, and the panel would
		// shut under somebody leaving insert mode.
		screen.addEventListener("keydown", function (e) {
			if (e.key === "Escape") e.preventDefault();
		});

		var pending = 0;
		new ResizeObserver(function () {
			window.cancelAnimationFrame(pending);
			pending = window.requestAnimationFrame(refit);
		}).observe(screen);

		if (!root.requestFullscreen) {
			full.hidden = true;
		} else {
			full.addEventListener("click", function () {
				if (document.fullscreenElement === root) {
					document.exitFullscreen();
				} else {
					root.requestFullscreen().catch(function () {});
				}
			});
			document.addEventListener("fullscreenchange", function () {
				if (fullLabel) {
					fullLabel.textContent = document.fullscreenElement === root ? "Exit full screen" : "Full screen";
				}
				term.focus();
			});
		}

		reconnect.addEventListener("click", function () {
			failures = 0;
			connect();
		});

		// A tab put away while the network dropped is reconnected when it comes
		// back, rather than spending its retries while nobody is looking.
		document.addEventListener("visibilitychange", function () {
			if (document.visibilityState === "visible" && !ws && !ended && !reconnect.hidden) {
				failures = 0;
				connect();
			}
		});

		window.addEventListener("pagehide", function () {
			window.clearTimeout(retry);
			if (ws) ws.close(1000);
		});

		connect();
	}

	function start() {
		document.querySelectorAll("[data-console]").forEach(init);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", start);
	} else {
		start();
	}
})();
