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

// Modals: a button with data-open-dialog="<dialog id>" opens it; a click on the
// backdrop or on [data-close-dialog] closes it. Bound once on document, so it
// keeps working after hx-boost swaps the page body.
(function () {
    if (window.__gmsDialogs) {
        return;
    }
    window.__gmsDialogs = true;

    document.addEventListener('click', function (e) {
        var t = e.target;
        if (!t || !t.closest) {
            return;
        }

        var opener = t.closest('[data-open-dialog]');
        if (opener) {
            var dlg = document.getElementById(opener.getAttribute('data-open-dialog'));
            if (dlg && typeof dlg.showModal === 'function' && !dlg.open) {
                dlg.showModal();
                var first = dlg.querySelector('input:not([type=hidden]), select, textarea');
                if (first) {
                    first.focus();
                }
            }
            return;
        }

        var open = t.closest('dialog.modal[open]');
        if (open && open.id !== 'gms-dialog' && (t === open || t.closest('[data-close-dialog]'))) {
            open.close();
        }
    });
}());

// Confirmation and status modals that replace window.confirm/alert.
//   gmsConfirm(message, {title, okLabel, danger}) -> Promise<boolean>
//   gmsAlert(message, {title, kind: 'success'|'error'|'info'}) -> Promise<void>
// Every hx-confirm goes through gmsConfirm, and failed actions show a status
// modal instead of failing silently.
(function () {
    if (window.gmsConfirm) {
        return;
    }

    function pt() {
        return /^pt/i.test(document.documentElement.lang || '');
    }

    function text(key) {
        var t = {
            confirm: pt() ? 'Confirmar' : 'Confirm',
            cancel: pt() ? 'Cancelar' : 'Cancel',
            ok: 'OK',
            sure: pt() ? 'Tem certeza?' : 'Are you sure?',
            done: pt() ? 'Pronto' : 'Done',
            error: pt() ? 'Algo deu errado' : 'Something went wrong',
            info: pt() ? 'Aviso' : 'Notice',
            failed: pt() ? 'A ação não pôde ser concluída. Tente de novo em instantes.' : 'The action could not be completed. Try again in a moment.',
            offline: pt() ? 'Não foi possível falar com o servidor. Confira a conexão e tente de novo.' : 'Could not reach the server. Check the connection and try again.'
        };
        return t[key];
    }

    var icons = {
        confirm: 'fa-circle-question',
        danger: 'fa-triangle-exclamation',
        success: 'fa-circle-check',
        error: 'fa-circle-xmark',
        info: 'fa-circle-info'
    };

    // One dialog, filled in for each question.
    function dialog() {
        var dlg = document.getElementById('gms-dialog');
        if (dlg) {
            return dlg;
        }
        dlg = document.createElement('dialog');
        dlg.id = 'gms-dialog';
        dlg.className = 'modal modal-sm gms-dialog';
        dlg.setAttribute('aria-labelledby', 'gms-dialog-title');
        dlg.setAttribute('aria-describedby', 'gms-dialog-message');
        dlg.innerHTML =
            '<section class="panel">' +
            '<div class="gms-dialog-body">' +
            '<span class="gms-dialog-icon" aria-hidden="true"><i class="fas"></i></span>' +
            '<div><h2 id="gms-dialog-title"></h2><p id="gms-dialog-message"></p></div>' +
            '</div>' +
            '<div class="gms-dialog-actions">' +
            '<button type="button" class="btn-quiet" data-gms="cancel"></button>' +
            '<button type="button" class="btn-primary" data-gms="ok"></button>' +
            '</div>' +
            '</section>';
        document.body.appendChild(dlg);
        return dlg;
    }

    function open(opts) {
        return new Promise(function (resolve) {
            var dlg = dialog();
            if (dlg.open) {
                dlg.close();
            }
            var kind = opts.kind;
            dlg.setAttribute('data-kind', kind);
            dlg.querySelector('.gms-dialog-icon i').className = 'fas ' + (icons[kind] || icons.info);
            dlg.querySelector('#gms-dialog-title').textContent = opts.title;
            dlg.querySelector('#gms-dialog-message').textContent = opts.message || '';

            var ok = dlg.querySelector('[data-gms="ok"]');
            var cancel = dlg.querySelector('[data-gms="cancel"]');
            ok.textContent = opts.okLabel;
            ok.className = kind === 'danger' ? 'btn-danger btn-danger-solid' : 'btn-primary';
            cancel.textContent = text('cancel');
            cancel.hidden = !opts.ask;

            var done = false;

            function finish(result) {
                if (done) {
                    return;
                }
                done = true;
                if (dlg.open) {
                    dlg.close();
                }
                resolve(result);
            }

            ok.onclick = function () { finish(true); };
            cancel.onclick = function () { finish(false); };
            // Escape and a click on the backdrop answer "no".
            dlg.onclose = function () { finish(false); };
            dlg.onclick = function (e) { if (e.target === dlg) finish(false); };

            dlg.showModal();
            (opts.ask ? cancel : ok).focus();
        });
    }

    window.gmsConfirm = function (message, opts) {
        opts = opts || {};
        return open({
            ask: true,
            kind: opts.danger ? 'danger' : 'confirm',
            title: opts.title || text('sure'),
            message: message,
            okLabel: opts.okLabel || text('confirm')
        });
    };

    window.gmsAlert = function (message, opts) {
        opts = opts || {};
        var kind = opts.kind || 'info';
        var titles = { success: text('done'), error: text('error'), info: text('info') };
        return open({
            ask: false,
            kind: kind,
            title: opts.title || titles[kind] || text('info'),
            message: message,
            okLabel: opts.okLabel || text('ok')
        }).then(function () {});
    };

    // hx-confirm="..." asks with the modal. Destructive actions (a red button,
    // or data-confirm-danger) get the red confirm button.
    document.addEventListener('htmx:confirm', function (e) {
        var question = e.detail && e.detail.question;
        if (!question) {
            return;
        }
        e.preventDefault();
        var elt = e.detail.elt;
        var danger = !!(elt && elt.closest && (elt.closest('[data-confirm-danger]') || elt.matches('.btn-danger') ||
            (elt.tagName === 'FORM' && elt.querySelector('.btn-danger'))));
        var label = elt && elt.getAttribute ? elt.getAttribute('data-confirm-ok') : '';
        window.gmsConfirm(question, { danger: danger, okLabel: label || undefined }).then(function (yes) {
            if (yes) {
                e.detail.issueRequest(true);
            }
        });
    });

    // Background refreshes fail quietly and try again; only actions people
    // started themselves (a click, a submit) show the error.
    function userAction(e) {
        var cfg = e.detail && e.detail.requestConfig;
        var verb = cfg && cfg.verb ? cfg.verb.toLowerCase() : 'get';
        var trigger = cfg && cfg.triggeringEvent;
        return verb !== 'get' || !!(trigger && trigger.isTrusted);
    }

    document.addEventListener('htmx:responseError', function (e) {
        if (!userAction(e)) {
            return;
        }
        var xhr = e.detail.xhr;
        var body = xhr && xhr.responseText ? xhr.responseText.trim() : '';
        // Short plain-text errors from the server are shown as they are.
        var msg = body && body.length < 300 && body.indexOf('<') === -1 ? body : text('failed');
        window.gmsAlert(msg, { kind: 'error' });
    });

    document.addEventListener('htmx:sendError', function (e) {
        if (!userAction(e)) {
            return;
        }
        window.gmsAlert(text('offline'), { kind: 'error' });
    });
}());
