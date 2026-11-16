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

    apply(preferred());

    document.addEventListener("DOMContentLoaded", function () {
        var button = document.getElementById("theme-toggle");

        if (!button) {
            return;
        }

        function sync() {
            var dark = document.documentElement.getAttribute("data-theme") === "dark";

            button.setAttribute("aria-pressed", dark ? "true" : "false");
        }

        sync();

        button.addEventListener("click", function () {
            var next = document.documentElement.getAttribute("data-theme") === "dark" ? "light" : "dark";

            try {
                localStorage.setItem(key, next);
            } catch (err) {
                /* The choice still applies for this page. */
            }

            apply(next);
            sync();
        });
    });
}());
