(function () {
	const INGEST_URL = "http://localhost:8080/api/ingest";
	const SITE_TOKEN = "cb69e2f6273b1e20362152cd784f5a07"; // from POST /api/sites response

	// Stable session ID for this browser tab (cleared on tab close)
	function getSessionID() {
		let id = sessionStorage.getItem("_sid");
		if (!id) {
			id = crypto.randomUUID();
			sessionStorage.setItem("_sid", id);
		}
		return id;
	}

	// Deduplicate pageviews: only one per session per URL
	function hasTrackedPageview() {
		const key = "_pv_" + location.pathname;
		if (sessionStorage.getItem(key)) return true;
		sessionStorage.setItem(key, "1");
		return false;
	}

	function send(eventType, properties) {
		navigator.sendBeacon(
			INGEST_URL,
			JSON.stringify({
				site_token: SITE_TOKEN,
				event_type: eventType,
				page_url: location.href,
				session_id: getSessionID(),
				referrer_url: document.referrer || "",
				properties: properties ? JSON.stringify(properties) : "",
			}),
		);
	}

	// Track unique pageview on load
	if (!hasTrackedPageview()) {
		send("pageview");
	}

	window.tracker = {
		// DOM usage: tracker.trackClick('#my-button', { label: 'hero-cta' })
		trackClick: function (selector, meta) {
			const el = document.querySelector(selector);
			if (!el) return;
			el.addEventListener("click", function () {
				send("click", { selector: selector, ...meta });
			});
		},

		send,

		// React usage: <Button onClick={window.tracker.handleClick("Sign-in button")}>
		// Returns a handler function; call it with a label, optionally extra meta.
		handleClick: function (label, meta) {
			return function () {
				send("click", { label: label, ...meta });
			};
		},
	};
})();
