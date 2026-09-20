package authsrv

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sync"
)

// handleAuthorize validates the client's authorization request, starts a
// Telegram QR login, and renders the QR page. Failures in client_id /
// redirect_uri validation render an error page and never redirect
// (open-redirect protection); once the redirect target is trusted, protocol
// errors are returned to it per RFC 6749 §4.1.2.1.
func (a *AuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	client, err := a.parseClientID(q.Get("client_id"))
	if err != nil {
		a.renderErrorPage(w, http.StatusBadRequest, "Unknown client",
			"The client_id is missing or invalid. Re-register the client and try again.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" || !matchRegistered(client.RedirectURIs, redirectURI) || !a.policy.allowed(redirectURI) {
		a.renderErrorPage(w, http.StatusBadRequest, "Invalid redirect URI",
			"The redirect_uri does not match the client's registration.")
		return
	}

	state := q.Get("state")
	if q.Get("response_type") != "code" {
		redirectError(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")
	if challenge == "" || method != "S256" {
		redirectError(w, r, redirectURI, state, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	resource := normalizeResource(q.Get("resource"))
	if resource == "" {
		resource = a.cfg.IssuerURL
	}
	if resource != a.cfg.IssuerURL {
		redirectError(w, r, redirectURI, state, "invalid_target", "unknown resource")
		return
	}

	blob, err := sealBlob(a.sealer, stateBlob, stateClaims{
		ClientID:      q.Get("client_id"),
		RedirectURI:   redirectURI,
		State:         state,
		CodeChallenge: challenge,
		Resource:      resource,
		IssuedAt:      a.now().Unix(),
	})
	if err != nil {
		a.logger.Error("sealing authorize state failed", "err", err)
		redirectError(w, r, redirectURI, state, "server_error", "internal error")
		return
	}

	ip := clientIP(r, a.cfg.TrustedProxyHops)
	p, loginCtx, err := a.reserveLivePending(blob, ip)
	if err != nil {
		if errors.Is(err, errTooManyLogins) {
			a.renderErrorPage(w, http.StatusServiceUnavailable, "Too many login attempts",
				"Too many logins are in flight right now. Wait a minute and try again.")
			return
		}
		a.logger.Error("reserving pending login failed", "err", err)
		redirectError(w, r, redirectURI, state, "temporarily_unavailable", "authorization server is unavailable")
		return
	}

	// The login flow outlives this request and detaches from any context; we
	// pass loginCtx only to carry values, not for cancellation (Close stops
	// flows by aborting them via the registry, not by canceling this ctx).
	// Passing r.Context() would be wrong regardless — the flow must not die
	// when this request returns.
	flow, err := a.startLogin(loginCtx)
	if err != nil {
		a.removePending(p.id)
		a.logger.Error("starting telegram login failed", "err", err)
		redirectError(w, r, redirectURI, state, "server_error", "starting the Telegram login failed")
		return
	}
	if !a.activatePending(p, flow) {
		flow.Abort()
		redirectError(w, r, redirectURI, state, "server_error", "server is shutting down")
		return
	}

	a.renderLoginPage(w, clientDisplayName(client), redirectURI, p.id)
}

// redirectError returns a protocol error to an already-validated redirect URI.
func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	// Callers pass only redirect URIs already validated against the
	// client's registration and the redirect policy (see handleAuthorize).
	http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // G710: pre-validated redirect target
}

// loginTemplate is the self-contained QR login page. No external resources:
// the CSP below allows only same-origin images (the QR PNG), same-origin
// fetch (the poll loop and password submit), and the inline style/script.
var loginTemplate = sync.OnceValue(func() *template.Template {
	return template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize {{.ClientName}}</title>
<style>
body{font-family:system-ui,sans-serif;display:flex;justify-content:center;padding-top:8vh;background:#f6f7f9;color:#1a1a2e;margin:0}
main{background:#fff;border:1px solid #e2e4e8;border-radius:12px;padding:32px;max-width:460px;box-shadow:0 4px 16px rgba(0,0,0,.06);text-align:center}
h1{font-size:20px;margin:0 0 12px}
p{line-height:1.5;margin:8px 0}
code{background:#f0f1f4;border-radius:4px;padding:2px 6px;font-size:13px;word-break:break-all}
#qrwrap{position:relative;width:240px;height:240px;border:1px solid #e2e4e8;border-radius:8px;margin:12px auto;overflow:hidden;background:#fff}
#qr{width:240px;height:240px;image-rendering:pixelated;display:block;opacity:0;transition:opacity .2s ease}
#qr.ready{opacity:1}
#qrloader{position:absolute;inset:0;display:grid;place-items:center;background:linear-gradient(145deg,#f7fcff,#edf8fd)}
.qr-placeholder{position:relative;width:184px;height:184px;animation:qr-pulse 1.6s ease-in-out infinite}
.qr-modules{position:absolute;inset:10px;background-color:#d8f1fc;background-image:linear-gradient(90deg,rgba(42,171,238,.38) 50%,transparent 50%),linear-gradient(rgba(42,171,238,.38) 50%,transparent 50%);background-size:24px 24px;border-radius:4px}
.qr-finder{position:absolute;width:48px;height:48px;border:7px solid #2aabee;border-radius:4px;box-sizing:border-box;background:#f7fcff;z-index:1}
.qr-finder::after{content:"";position:absolute;inset:8px;background:#2aabee;border-radius:2px}
.qr-finder-tl{left:10px;top:10px}.qr-finder-tr{right:10px;top:10px}.qr-finder-bl{left:10px;bottom:10px}
@keyframes qr-pulse{0%,100%{opacity:.55;transform:scale(.97)}50%{opacity:1;transform:scale(1)}}
@media (prefers-reduced-motion:reduce){.qr-placeholder{animation:none}#qr{transition:none}}
button{background:#2aabee;color:#fff;border:0;border-radius:8px;padding:12px 24px;font-size:15px;cursor:pointer;margin-top:12px;width:100%}
button:hover{background:#1e96d6}
.muted{color:#5f6368;font-size:13px}
#status{min-height:1.5em;font-weight:600}
label{display:block;margin-top:8px;font-size:14px;font-weight:600;text-align:left}
input[type=password]{width:100%;box-sizing:border-box;margin-top:6px;padding:10px 12px;font-size:15px;border:1px solid #c4c7cc;border-radius:8px}
[hidden]{display:none!important}
</style></head><body><main>
<h1>Link Telegram to authorize {{.ClientName}}</h1>
<p>Open Telegram on your phone, go to <strong>Settings&nbsp;&rarr;&nbsp;Devices&nbsp;&rarr;&nbsp;Link&nbsp;Desktop&nbsp;Device</strong> and scan this code.</p>
<div id="qrwrap" aria-busy="true">
<div id="qrloader" role="img" aria-label="Preparing Telegram login QR code">
<div class="qr-placeholder" aria-hidden="true"><span class="qr-modules"></span><span class="qr-finder qr-finder-tl"></span><span class="qr-finder qr-finder-tr"></span><span class="qr-finder qr-finder-bl"></span></div>
</div>
<img id="qr" alt="Telegram login QR code" hidden>
</div>
<form id="pwform" hidden>
<label for="password">Two-step verification password</label>
<input type="password" id="password" name="password" autocomplete="current-password" required>
<button type="submit">Submit password</button>
</form>
<p id="status" role="status" aria-live="polite" aria-atomic="true">Preparing secure QR code&hellip;</p>
<p class="muted">After approval you will be redirected to:<br><code>{{.RedirectURI}}</code></p>
</main>
<script>
(function () {
	"use strict";
	var loginID = {{.LoginID}};
	var img = document.getElementById("qr");
	var loader = document.getElementById("qrloader");
	var statusEl = document.getElementById("status");
	var form = document.getElementById("pwform");
	var qrwrap = document.getElementById("qrwrap");
	var displayedRev = 0;
	var loadingRev = 0;
	var latestRev = 0;
	var stopped = false;
	var qrActive = true;

	form.addEventListener("submit", function (e) {
		e.preventDefault();
		var body = new URLSearchParams();
		body.set("login", loginID);
		body.set("password", document.getElementById("password").value);
		fetch("/login/password", {method: "POST", body: body});
		statusEl.textContent = "Checking password…";
	});

	function clearQRImage() {
		img.classList.remove("ready");
		img.hidden = true;
		img.removeAttribute("src");
	}

	function showQRLoader() {
		clearQRImage();
		qrwrap.hidden = false;
		qrwrap.setAttribute("aria-busy", "true");
		loader.hidden = false;
		statusEl.textContent = "Preparing secure QR code…";
	}

	function hideQR() {
		clearQRImage();
		qrwrap.hidden = true;
		loader.hidden = true;
	}

	function loadQR(rev) {
		if (!rev || rev !== latestRev || rev <= displayedRev || rev === loadingRev) { return; }
		loadingRev = rev;
		var next = new Image();
		var src = "/login/qr?login=" + encodeURIComponent(loginID) + "&rev=" + rev;
		next.onload = function () {
			if (stopped || !qrActive || loadingRev !== rev) { return; }
			img.src = src;
			img.hidden = false;
			loader.hidden = true;
			qrwrap.setAttribute("aria-busy", "false");
			displayedRev = rev;
			loadingRev = 0;
			requestAnimationFrame(function () { img.classList.add("ready"); });
			if (form.hidden) { statusEl.textContent = "Waiting for scan…"; }
		};
		next.onerror = function () {
			if (loadingRev !== rev) { return; }
			loadingRev = 0;
			if (!stopped && qrActive && rev === latestRev) {
				setTimeout(function () { loadQR(rev); }, 1000);
			}
		};
		next.src = src;
	}

	function apply(j) {
		switch (j.status) {
		case "waiting":
			qrActive = true;
			qrwrap.hidden = false;
			if (j.qr_rev && j.qr_rev > latestRev) {
				latestRev = j.qr_rev;
				// A new revision means Telegram has expired the displayed token.
				// Remove it before fetching its replacement so it cannot be scanned.
				showQRLoader();
			}
			if (j.qr_rev) { loadQR(j.qr_rev); }
			if (!displayedRev) { statusEl.textContent = "Preparing secure QR code…"; }
			return;
		case "done":
			stopped = true;
			qrActive = false;
			hideQR();
			statusEl.textContent = "Logged in. Redirecting…";
			window.location.href = j.redirect;
			return;
		case "password":
			qrActive = false;
			hideQR();
			form.hidden = false;
			statusEl.textContent = j.message || "Enter your two-step verification password.";
			return;
		case "failed":
		case "expired":
			stopped = true;
			qrActive = false;
			hideQR();
			form.hidden = true;
			statusEl.textContent = j.message || "Login failed.";
			return;
		}
	}

	function poll() {
		if (stopped) { return; }
		fetch("/login/poll?login=" + encodeURIComponent(loginID))
			.then(function (r) { return r.json(); })
			.then(apply)
			.catch(function () {})
			.then(function () { if (!stopped) { setTimeout(poll, 2000); } });
	}
	poll();
})();
</script>
</body></html>`))
})

// setInterstitialHeaders hardens the login/error pages: never cached, never
// frameable (a QR scan or password entry must not be hijackable via an
// embedding frame), and locked down to same-origin subresources only.
func setInterstitialHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	// connect-src 'self' is required for the poll loop and the password
	// submit; everything else stays closed.
	h.Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
}

// renderLoginPage shows the Telegram QR login page.
func (a *AuthServer) renderLoginPage(w http.ResponseWriter, clientName, redirectURI, loginID string) {
	setInterstitialHeaders(w)
	err := loginTemplate().Execute(w, map[string]any{
		"ClientName":  clientName,
		"RedirectURI": redirectURI,
		"LoginID":     loginID,
	})
	if err != nil {
		a.logger.Error("rendering login page failed", "err", err)
	}
}

// renderErrorPage shows a terminal error page (used when redirecting back to
// the client would be unsafe, and for the pending-login capacity limit).
func (a *AuthServer) renderErrorPage(w http.ResponseWriter, status int, title, detail string) {
	setInterstitialHeaders(w)
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s</title></head>
<body style="font-family:system-ui,sans-serif;padding:15vh 20px;text-align:center">
<h1 style="font-size:20px">%s</h1><p style="color:#5f6368">%s</p></body></html>`,
		template.HTMLEscapeString(title), template.HTMLEscapeString(title), template.HTMLEscapeString(detail))
}
