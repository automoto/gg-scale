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
	// above it when there is no room below.
	var rowMenuGap = 6;

	function positionRowMenu(menu) {
		var summary = menu.querySelector("summary");
		var panel = menu.querySelector(":scope > div");
		if (!(summary instanceof HTMLElement) || !(panel instanceof HTMLElement)) {
			return;
		}
		var rect = summary.getBoundingClientRect();
		panel.style.position = "fixed";
		panel.style.right = "auto";
		panel.style.margin = "0";
		var left = Math.max(8, rect.right - panel.offsetWidth);
		var top = rect.bottom + rowMenuGap;
		if (top + panel.offsetHeight > window.innerHeight - 8 && rect.top - rowMenuGap - panel.offsetHeight > 8) {
			top = rect.top - rowMenuGap - panel.offsetHeight;
		}
		panel.style.left = left + "px";
		panel.style.top = top + "px";
	}

	function resetRowMenu(menu) {
		var panel = menu.querySelector(":scope > div");
		if (panel instanceof HTMLElement) {
			panel.removeAttribute("style");
		}
	}

	function closeOpenRowMenus() {
		document.querySelectorAll("details.row-menu[open]").forEach(function (menu) {
			menu.open = false;
		});
	}

	// A fixed panel cannot track the summary through a scroll (the summary may
	// slide under the card's clip edge), so close instead of repositioning.
	window.addEventListener("resize", closeOpenRowMenus);
	document.addEventListener("scroll", closeOpenRowMenus, true);

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
		if (menu.matches("details.row-menu")) {
			positionRowMenu(menu);
		}
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
