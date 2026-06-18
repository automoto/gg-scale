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
})();
