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

	document.addEventListener("toggle", function (event) {
		var menu = event.target;
		if (!(menu instanceof HTMLDetailsElement) || !menu.open || !menu.matches(menuSelector)) {
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
