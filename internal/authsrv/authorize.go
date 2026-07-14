package authsrv

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
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
	if res := q.Get("resource"); res != "" && strings.TrimRight(res, "/") != a.cfg.IssuerURL {
		redirectError(w, r, redirectURI, state, "invalid_target", "unknown resource")
		return
	}

	blob, err := sealBlob(a.sealer, stateBlob, stateClaims{
		ClientID:      q.Get("client_id"),
		RedirectURI:   redirectURI,
		State:         state,
		CodeChallenge: challenge,
		Resource:      q.Get("resource"),
		IssuedAt:      a.now().Unix(),
	})
	if err != nil {
		a.logger.Error("sealing authorize state failed", "err", err)
		redirectError(w, r, redirectURI, state, "server_error", "internal error")
		return
	}

	// The login flow outlives this request and detaches from any context; we
	// pass loginCtx only to carry values, not for cancellation (Close stops
	// flows by aborting them via the registry, not by canceling this ctx).
	// Passing r.Context() would be wrong regardless — the flow must not die
	// when this request returns.
	flow, err := a.startLogin(a.loginCtx)
	if err != nil {
		a.logger.Error("starting telegram login failed", "err", err)
		redirectError(w, r, redirectURI, state, "server_error", "starting the Telegram login failed")
		return
	}
	p, err := a.addPending(blob, flow, clientIP(r))
	if err != nil {
		flow.Abort()
		if errors.Is(err, errTooManyLogins) {
			a.renderErrorPage(w, http.StatusServiceUnavailable, "Too many login attempts",
				"Too many logins are in flight right now. Wait a minute and try again.")
			return
		}
		a.logger.Error("registering pending login failed", "err", err)
		redirectError(w, r, redirectURI, state, "server_error", "internal error")
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
img{width:240px;height:240px;image-rendering:pixelated;border:1px solid #e2e4e8;border-radius:8px;margin:12px auto;display:block}
button{background:#2aabee;color:#fff;border:0;border-radius:8px;padding:12px 24px;font-size:15px;cursor:pointer;margin-top:12px;width:100%}
button:hover{background:#1e96d6}
.muted{color:#5f6368;font-size:13px}
#status{min-height:1.5em;font-weight:600}
label{display:block;margin-top:8px;font-size:14px;font-weight:600;text-align:left}
input[type=password]{width:100%;box-sizing:border-box;margin-top:6px;padding:10px 12px;font-size:15px;border:1px solid #c4c7cc;border-radius:8px}
</style></head><body><main>
<h1>Link Telegram to authorize {{.ClientName}}</h1>
<p>Open Telegram on your phone, go to <strong>Settings&nbsp;&rarr;&nbsp;Devices&nbsp;&rarr;&nbsp;Link&nbsp;Desktop&nbsp;Device</strong> and scan this code.</p>
<div id="qrwrap"><img id="qr" src="/login/qr?login={{.LoginID}}" alt="Telegram login QR code"></div>
<form id="pwform" hidden>
<label for="password">Two-step verification password</label>
<input type="password" id="password" name="password" autocomplete="current-password" required>
<button type="submit">Submit password</button>
</form>
<p id="status">Waiting for scan&hellip;</p>
<p class="muted">After approval you will be redirected to:<br><code>{{.RedirectURI}}</code></p>
</main>
<script>
(function () {
	"use strict";
	var loginID = {{.LoginID}};
	var img = document.getElementById("qr");
	var statusEl = document.getElementById("status");
	var form = document.getElementById("pwform");
	var qrwrap = document.getElementById("qrwrap");
	var rev = 0;
	var stopped = false;

	form.addEventListener("submit", function (e) {
		e.preventDefault();
		var body = new URLSearchParams();
		body.set("login", loginID);
		body.set("password", document.getElementById("password").value);
		fetch("/login/password", {method: "POST", body: body});
		statusEl.textContent = "Checking password…";
	});

	function apply(j) {
		if (j.qr_rev && j.qr_rev !== rev) {
			rev = j.qr_rev;
			img.src = "/login/qr?login=" + encodeURIComponent(loginID) + "&rev=" + rev;
		}
		switch (j.status) {
		case "done":
			stopped = true;
			statusEl.textContent = "Logged in. Redirecting…";
			window.location.href = j.redirect;
			return;
		case "password":
			qrwrap.hidden = true;
			form.hidden = false;
			statusEl.textContent = j.message || "Enter your two-step verification password.";
			break;
		case "failed":
		case "expired":
			stopped = true;
			qrwrap.hidden = true;
			form.hidden = true;
			statusEl.textContent = j.message || "Login failed.";
			return;
		default:
			if (form.hidden) { statusEl.textContent = "Waiting for scan…"; }
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
	setTimeout(poll, 1500);
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
