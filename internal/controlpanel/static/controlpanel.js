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
