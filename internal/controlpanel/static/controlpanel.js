(function () {
	"use strict";

	document.addEventListener("submit", function (event) {
		var form = event.target;
		if (!(form instanceof HTMLFormElement)) {
			return;
		}
		var message = form.getAttribute("data-confirm");
		if (message === null || message === "") {
			return;
		}
		if (!window.confirm(message)) {
			event.preventDefault();
		}
	});

	document.addEventListener("click", function (event) {
		var target = event.target;
		if (!(target instanceof Element)) {
			return;
		}
		var opener = target.closest("[data-dialog-open]");
		if (opener instanceof HTMLElement) {
			var id = opener.getAttribute("data-dialog-open");
			var dialog = id === null ? null : document.getElementById(id);
			if (dialog instanceof HTMLDialogElement) {
				dialog.dataset.returnFocus = id;
				dialog.showModal();
				var first = dialog.querySelector("input:not([disabled]), button:not([disabled]), a[href]");
				if (first instanceof HTMLElement) {
					first.focus();
				}
			}
			return;
		}
		if (target.closest("[data-dialog-close]")) {
			var closeDialog = target.closest("dialog");
			if (closeDialog instanceof HTMLDialogElement) {
				closeDialog.close();
			}
		}
	});

	// Dropdown menus (<details class="nav-menu"> / "row-menu"): close on
	// outside click or Escape, and keep at most one open at a time.
	var menuSelector = "details.nav-menu, details.row-menu";

	// Row menus sit inside horizontally scrollable cards, where an absolutely
	// positioned panel is clipped to the card's scroll box. While one is open,
	// float its panel with position: fixed anchored to the summary, flipping
	// above it when there is no room below. The panel's right edge is pinned to
	// the summary's (never computed from a measured width) so the anchor cannot
	// shift if fonts or layout settle after the panel first paints.
	var rowMenuGap = 6;

	function positionRowMenu(menu) {
		var summary = menu.querySelector("summary");
		var panel = menu.querySelector(":scope > ul");
		if (!(summary instanceof HTMLElement) || !(panel instanceof HTMLElement)) {
			return;
		}
		var rect = summary.getBoundingClientRect();
		panel.style.position = "fixed";
		panel.style.margin = "0";
		panel.style.width = "auto";
		panel.style.left = "auto";
		panel.style.right = Math.max(8, window.innerWidth - rect.right) + "px";
		if (rect.bottom + rowMenuGap + panel.offsetHeight > window.innerHeight - 8 && rect.top - rowMenuGap - panel.offsetHeight > 8) {
			panel.style.top = "auto";
			panel.style.bottom = window.innerHeight - rect.top + rowMenuGap + "px";
		} else {
			panel.style.bottom = "auto";
			panel.style.top = rect.bottom + rowMenuGap + "px";
		}
	}

	function resetRowMenu(menu) {
		var panel = menu.querySelector(":scope > ul");
		if (panel instanceof HTMLElement) {
			panel.removeAttribute("style");
		}
	}

	function closeOpenRowMenus() {
		document.querySelectorAll("details.row-menu[open]").forEach(function (menu) {
			menu.open = false;
		});
	}

	// Scroll events are dispatched asynchronously, so a scroll the browser
	// started before the menu opened (e.g. scrolling the clicked summary into
	// view) can arrive just after: follow the summary through that early
	// scroll. A fixed panel cannot track later, deliberate scrolls (the summary
	// may slide under the card's clip edge), so those close the menu.
	var rowMenuOpenedAt = 0;

	window.addEventListener("resize", closeOpenRowMenus);
	document.addEventListener("scroll", function () {
		var open = document.querySelector("details.row-menu[open]");
		if (!(open instanceof HTMLDetailsElement)) {
			return;
		}
		if (performance.now() - rowMenuOpenedAt < 150) {
			positionRowMenu(open);
		} else {
			open.open = false;
		}
	}, true);

	// Open and position row menus synchronously on the summary click — the
	// panel is positioned exactly once, before the first paint. Relying on the
	// asynchronous toggle event instead lets the browser paint frames with the
	// panel at its fallback position and then re-pin it, visible as a jump.
	document.addEventListener("click", function (event) {
		var target = event.target;
		if (!(target instanceof Element)) {
			return;
		}
		var summary = target.closest("details.row-menu > summary");
		if (!(summary instanceof HTMLElement)) {
			return;
		}
		var menu = summary.parentElement;
		event.preventDefault();
		if (menu.open) {
			menu.open = false;
			return;
		}
		closeOpenRowMenus();
		rowMenuOpenedAt = performance.now();
		menu.open = true;
		positionRowMenu(menu);
	});

	document.addEventListener("toggle", function (event) {
		var menu = event.target;
		if (!(menu instanceof HTMLDetailsElement) || !menu.matches(menuSelector)) {
			return;
		}
		if (!menu.open) {
			resetRowMenu(menu);
			return;
		}
		document.querySelectorAll(menuSelector + "[open]").forEach(function (other) {
			if (other !== menu) {
				other.open = false;
			}
		});
	}, true);

	document.addEventListener("click", function (event) {
		var target = event.target;
		if (!(target instanceof Element)) {
			return;
		}
		document.querySelectorAll(menuSelector + "[open]").forEach(function (menu) {
			if (!menu.contains(target)) {
				menu.open = false;
			}
		});
	});

	document.addEventListener("keydown", function (event) {
		if (event.key !== "Escape") {
			return;
		}
		var open = document.querySelector(menuSelector + "[open]");
		if (!(open instanceof HTMLDetailsElement)) {
			return;
		}
		open.open = false;
		var summary = open.querySelector("summary");
		if (summary instanceof HTMLElement) {
			summary.focus();
		}
	});

	document.body.addEventListener("htmx:afterSwap", function (event) {
		var target = event.target;
		if (!(target instanceof HTMLElement) || target.id !== "modal-root") {
			return;
		}
		var dialog = target.querySelector("dialog");
		if (!(dialog instanceof HTMLDialogElement)) {
			return;
		}
		dialog.showModal();
		var first = dialog.querySelector("input:not([disabled]), button:not([disabled]), a[href]");
		if (first instanceof HTMLElement) {
			first.focus();
		}
	});

	document.addEventListener("close", function (event) {
		var dialog = event.target;
		if (!(dialog instanceof HTMLDialogElement)) {
			return;
		}
		var modalRoot = document.getElementById("modal-root");
		if (modalRoot instanceof HTMLElement && modalRoot.contains(dialog)) {
			modalRoot.innerHTML = "";
			return;
		}
		var id = dialog.dataset.returnFocus;
		if (!id) {
			return;
		}
		var opener = document.querySelector("[data-dialog-open='" + id + "']");
		if (opener instanceof HTMLElement) {
			opener.focus();
		}
	}, true);
})();
