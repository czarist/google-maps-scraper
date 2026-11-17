(function () {
    var key = "gmaps-theme";

    function preferred() {
        var saved = "";

        try {
            saved = localStorage.getItem(key) || "";
        } catch (err) {
            saved = "";
        }

        if (saved === "light" || saved === "dark") {
            return saved;
        }

        return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    }

    function apply(theme) {
        document.documentElement.setAttribute("data-theme", theme);
        document.documentElement.style.colorScheme = theme;
    }

    function sync() {
        var button = document.getElementById("theme-toggle");
        var dark = document.documentElement.getAttribute("data-theme") === "dark";

        if (button) {
            button.setAttribute("aria-pressed", dark ? "true" : "false");
        }
    }

    apply(preferred());

    // Pages are swapped in place (hx-boost), so the button is found on each click
    // instead of being bound once.
    document.addEventListener("click", function (e) {
        if (!e.target.closest || !e.target.closest("#theme-toggle")) {
            return;
        }

        var next = document.documentElement.getAttribute("data-theme") === "dark" ? "light" : "dark";

        try {
            localStorage.setItem(key, next);
        } catch (err) {
            /* The choice still applies for this page. */
        }

        apply(next);
        sync();
    });

    document.addEventListener("DOMContentLoaded", sync);
    document.addEventListener("htmx:afterSettle", sync);
}());

// A thin bar at the top while a page is loading in place.
(function () {
    function bar() {
        var el = document.getElementById("page-progress");

        if (!el) {
            el = document.createElement("div");
            el.id = "page-progress";
            el.setAttribute("aria-hidden", "true");
            document.body.appendChild(el);
        }

        return el;
    }

    document.addEventListener("htmx:beforeRequest", function (e) {
        if (e.detail && e.detail.boosted) {
            bar().className = "is-loading";
        }
    });

    function done(e) {
        if (e.detail && e.detail.boosted) {
            bar().className = "is-done";
        }
    }

    document.addEventListener("htmx:afterSettle", done);
    document.addEventListener("htmx:responseError", done);
    document.addEventListener("htmx:sendError", done);

    // Keep <html lang> in step with the page after a language switch.
    document.addEventListener("htmx:beforeSwap", function (e) {
        var xhr = e.detail && e.detail.xhr;
        var m = xhr && e.detail.boosted && /<html lang="([^"]+)"/.exec(xhr.responseText || "");

        if (m) {
            document.documentElement.lang = m[1];
        }
    });
}());
