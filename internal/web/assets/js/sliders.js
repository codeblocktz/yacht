// Keeps a slider's label and its filled track in step with the handle.
//
// The server renders both correctly for the value it knows about, so the page
// is right before this runs and right without it. What this adds is the part
// that only matters while somebody is dragging: a number that changes under
// their thumb, which is the whole reason a slider beats a text field.
(function () {
	"use strict";

	function paint(input) {
		var max = Number(input.max) || 1;
		var pct = Math.round((Number(input.value) / max) * 100);
		input.style.background =
			"linear-gradient(to right,var(--foreground) 0%,var(--foreground) " +
			pct +
			"%,var(--muted) " +
			pct +
			"%,var(--muted) 100%)";
	}

	// The label is rendered by Go, and these two have to agree with it. Kept
	// deliberately close to cpuDisplay and memDisplay in resources.go: if they
	// drift, the number jumps the moment somebody touches the handle.
	function cpuLabel(millis) {
		if (millis <= 0) return "No limit";
		if (millis < 1000) return Number((millis / 1000).toPrecision(2)) + " vCPU";
		if (millis % 1000 === 0) return millis / 1000 + " vCPU";
		return (millis / 1000).toFixed(1) + " vCPU";
	}

	function memLabel(mib) {
		if (mib <= 0) return "No limit";
		if (mib < 1024) return mib + " MB";
		if (mib % 1024 === 0) return mib / 1024 + " GB";
		return (mib / 1024).toFixed(1) + " GB";
	}

	function update(input) {
		paint(input);

		var wrap = input.closest("[data-slider]");
		if (!wrap) return;
		var out = wrap.querySelector("[data-slider-value]");
		if (!out) return;

		var value = Number(input.value);
		out.textContent =
			input.getAttribute("data-unit") === "cpu" ? cpuLabel(value) : memLabel(value);

		// "No limit" is a different kind of answer from a small number, and
		// reads differently. The class is what says so.
		wrap.classList.toggle("slider-none", value <= 0);
	}

	function start() {
		document.addEventListener("input", function (event) {
			if (event.target.matches(".slider")) update(event.target);
		});
		// htmx swaps panels in, so this runs on whatever is present now and
		// the delegated listener above covers whatever arrives later.
		document.querySelectorAll(".slider").forEach(paint);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", start);
	} else {
		start();
	}
})();
