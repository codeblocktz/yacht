// The log explorer's keyboard, its live tail, and the range pickers that
// submit as soon as they change.
//
// Everything here adds to a page that already works without it: the filter is
// a form, the range a select with a submit behind Enter, and live tail without
// a log store is an htmx poll. Only following the store's tail needs script,
// because a stream of lines arriving is not something a form can show.
(function () {
	"use strict";

	// The most rows a followed log keeps. A tab left open on a busy project
	// would otherwise grow the page without end.
	var MAX_ROWS = 2000;

	// "/" focuses the filter, as it does on Railway and in most log viewers —
	// unless somebody is already typing, when "/" is a character.
	document.addEventListener("keydown", function (e) {
		if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
		var t = e.target;
		if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
		var box = document.querySelector("[data-log-filter]");
		if (!box) return;
		e.preventDefault();
		box.focus();
		box.select();
	});

	// A choice in a form marked data-autosubmit submits it: the range picker
	// is a selectbox, whose hidden input is told of the change.
	document.addEventListener("change", function (e) {
		var el = e.target;
		var form = el && el.closest ? el.closest("form[data-autosubmit]") : null;
		if (!form || el.type === "search") return;
		form.requestSubmit ? form.requestSubmit() : form.submit();
	});

	// A log reads downwards, newest last, so the list opens on its end.
	function pinBottom(root) {
		(root || document).querySelectorAll("[data-log-scroll]").forEach(function (el) {
			el.scrollTop = el.scrollHeight;
		});
	}

	function atBottom(el) {
		return el.scrollHeight - el.scrollTop - el.clientHeight < 24;
	}

	function pad(n) {
		return (n < 10 ? "0" : "") + n;
	}

	function timeOf(iso) {
		var d = new Date(iso);
		if (isNaN(d.getTime())) return "";
		return pad(d.getUTCHours()) + ":" + pad(d.getUTCMinutes()) + ":" + pad(d.getUTCSeconds());
	}

	// rowFor builds one row with textContent only: the message is somebody's
	// container output, and none of it may become markup.
	function rowFor(entry, services) {
		var tr = document.createElement("tr");
		if (entry.error) tr.className = "logx-err";
		var time = document.createElement("td");
		time.className = "logx-time";
		time.textContent = timeOf(entry.at);
		time.title = entry.at;
		var svc = document.createElement("td");
		svc.className = "logx-svc";
		var dot = document.createElement("span");
		dot.className = "logx-dot";
		var slot = Math.max(services.indexOf(entry.service), 0) % 8;
		dot.style.background = "var(--series-" + (slot + 1) + ")";
		svc.appendChild(dot);
		svc.appendChild(document.createTextNode(entry.service || ""));
		var msg = document.createElement("td");
		msg.className = "logx-msg";
		msg.textContent = entry.message || "";
		tr.appendChild(time);
		tr.appendChild(svc);
		tr.appendChild(msg);
		return tr;
	}

	// ensureTable finds the rows to append to, turning the empty state into a
	// table when the first line arrives.
	function ensureTable(root) {
		var body = root.querySelector("[data-log-rows]");
		if (body) return body;
		var empty = root.querySelector(".empty");
		if (!empty) return null;
		var wrap = document.createElement("div");
		wrap.className = "logx";
		wrap.setAttribute("data-log-scroll", "");
		var table = document.createElement("table");
		var head = document.createElement("thead");
		var hr = document.createElement("tr");
		["Time", "Service", "Data"].forEach(function (label) {
			var th = document.createElement("th");
			th.scope = "col";
			th.textContent = label;
			hr.appendChild(th);
		});
		head.appendChild(hr);
		body = document.createElement("tbody");
		body.setAttribute("data-log-rows", "");
		table.appendChild(head);
		table.appendChild(body);
		wrap.appendChild(table);
		empty.replaceWith(wrap);
		return body;
	}

	function follow(root) {
		var url = root.getAttribute("data-log-tail");
		if (!url || typeof EventSource === "undefined" || root.dataset.following) return;
		root.dataset.following = "1";
		var services = (root.getAttribute("data-log-services") || "").split(", ");
		var source = new EventSource(url);
		source.addEventListener("message", function (e) {
			var entry;
			try {
				entry = JSON.parse(e.data);
			} catch (err) {
				return;
			}
			var body = ensureTable(root);
			if (!body) return;
			var scroller = body.closest("[data-log-scroll]");
			var pinned = scroller ? atBottom(scroller) : false;
			body.appendChild(rowFor(entry, services));
			while (body.rows.length > MAX_ROWS) body.deleteRow(0);
			if (scroller && pinned) scroller.scrollTop = scroller.scrollHeight;
		});
		window.addEventListener("pagehide", function () {
			source.close();
		});
	}

	function init(root) {
		pinBottom(root);
		(root || document).querySelectorAll("[data-log-tail]").forEach(follow);
	}

	document.addEventListener("DOMContentLoaded", function () {
		init(document);
	});
	// The results are swapped in by the poll on a live page without a store;
	// keep them on their newest line as each arrives.
	// An outerHTML swap's target is the element it replaced, so the page is
	// searched again rather than the target.
	document.addEventListener("htmx:afterSettle", function () {
		init(document);
	});
})();
