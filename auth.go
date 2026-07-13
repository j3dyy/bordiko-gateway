package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	sessionCookie = "bordiko_session"
	stateCookie   = "bordiko_oauth_state"
	sessionTTL    = 30 * 24 * time.Hour
)

// Auth owns accounts, session cookies, and the OAuth providers. It is mounted by
// the gateway and also exposes sessionUser() so the lobby and WebSocket paths can
// require a logged-in user (login is required to play).
type Auth struct {
	users        UserStore
	secret       []byte
	publicURL    string // gateway's externally reachable base (for OAuth redirect_uri)
	frontendURL  string // where to send the browser after login
	cookieSecure bool
	devEnabled   bool
	providers    map[string]*oauthProvider
	hc           *http.Client
}

// NewAuth builds the auth subsystem from the environment.
func NewAuth(users UserStore) *Auth {
	secret := []byte(os.Getenv("SESSION_SECRET"))
	if len(secret) == 0 {
		secret = randomBytes(32)
		log.Printf("auth: SESSION_SECRET unset — using an ephemeral secret (sessions won't survive a restart)")
	}
	a := &Auth{
		users:        users,
		secret:       secret,
		publicURL:    strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"),
		frontendURL:  strings.TrimRight(env("FRONTEND_URL", "http://localhost:5173"), "/"),
		cookieSecure: os.Getenv("COOKIE_SECURE") == "true",
		devEnabled:   env("AUTH_DEV_ENABLED", "true") == "true",
		providers:    map[string]*oauthProvider{},
		hc:           &http.Client{Timeout: 15 * time.Second},
	}
	if p := googleProvider(os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET")); p.configured() {
		a.providers["google"] = p
	}
	if p := githubProvider(os.Getenv("GITHUB_CLIENT_ID"), os.Getenv("GITHUB_CLIENT_SECRET")); p.configured() {
		a.providers["github"] = p
	}
	log.Printf("auth: providers=%v dev-login=%v frontend=%s", a.providerNames(), a.devEnabled, a.frontendURL)
	return a
}

func (a *Auth) providerNames() []string {
	names := make([]string, 0, len(a.providers))
	for n := range a.providers {
		names = append(names, n)
	}
	return names
}

// RegisterRoutes wires the /auth/* endpoints onto the gateway mux.
func (a *Auth) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/providers", a.handleProviders)
	mux.HandleFunc("GET /auth/me", a.handleMe)
	mux.HandleFunc("POST /auth/username", a.handleSetName)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
	mux.HandleFunc("GET /auth/{provider}/login", a.handleLogin)
	mux.HandleFunc("GET /auth/{provider}/callback", a.handleCallback)
}

func (a *Auth) handleProviders(w http.ResponseWriter, _ *http.Request) {
	// accounts = which user store backs display names: "postgres" (durable) or
	// "memory" (wiped on every redeploy → names fall back to raw ids). Diagnostic
	// for the "board/leaderboard shows google:<id>" symptom.
	store := "memory"
	if _, ok := a.users.(*PostgresUserStore); ok {
		store = "postgres"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": a.providerNames(),
		"dev":       a.devEnabled,
		"accounts":  store,
	})
}

func (a *Auth) handleMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := a.sessionUser(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not_authenticated"})
		return
	}
	// Self-heal: if this account isn't in the store yet (e.g. logged in before
	// accounts were persisted, session still valid so never re-logged-in), add it
	// from the session so the leaderboard/board can resolve the name — no re-login
	// needed. Skip if already present (never clobber a saved name/email).
	if u, _ := a.users.Get(r.Context(), claims.Sub); u == nil {
		provider, pid := "unknown", claims.Sub
		if i := strings.IndexByte(claims.Sub, ':'); i >= 0 {
			provider, pid = claims.Sub[:i], claims.Sub[i+1:]
		}
		_ = a.users.Upsert(r.Context(), &User{ID: claims.Sub, Provider: provider, ProviderID: pid, DisplayName: claims.Name, AvatarURL: claims.Avatar})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          claims.Sub,
		"displayName": claims.Name,
		"avatarUrl":   claims.Avatar,
	})
}

// handleSetName lets a signed-in user pick a display name (so the board shows a
// real name, not the raw provider id). Updates the account and re-issues the
// session cookie so the new name takes effect everywhere immediately.
func (a *Auth) handleSetName(w http.ResponseWriter, r *http.Request) {
	claims, ok := a.sessionUser(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not_authenticated"})
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
		return
	}
	name := sanitizeName(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_name", "message": "name must be 1–24 characters"})
		return
	}
	u, err := a.users.Get(r.Context(), claims.Sub)
	if err != nil || u == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no_account"})
		return
	}
	if err := a.users.SetDisplayName(r.Context(), u.ID, name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save_failed"})
		return
	}
	u.DisplayName = name
	a.setSession(w, u) // refresh the cookie so claims.Name updates now
	writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "displayName": u.DisplayName, "avatarUrl": u.AvatarURL})
}

// sanitizeName trims, collapses inner whitespace, strips control characters, and
// caps the length at 24 runes.
func sanitizeName(raw string) string {
	s := strings.Join(strings.Fields(raw), " ")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	runes := []rune(s)
	if len(runes) > 24 {
		runes = runes[:24]
	}
	return strings.TrimSpace(string(runes))
}

// DisplayNames resolves a set of user ids to their display names (best-effort),
// so the gateway can label players on the board by name instead of raw id.
func (a *Auth) DisplayNames(ctx context.Context, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	users, err := a.users.GetMany(ctx, ids)
	if err != nil {
		return out
	}
	for id, u := range users {
		if u != nil && u.DisplayName != "" {
			out[id] = u.DisplayName
		}
	}
	return out
}

func (a *Auth) handleLogout(w http.ResponseWriter, _ *http.Request) {
	a.clearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleLogin starts a provider flow (or, for "dev", issues a session directly).
func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")

	if provider == "dev" {
		if !a.devEnabled {
			http.Error(w, "dev login is disabled", http.StatusNotFound)
			return
		}
		a.devLogin(w, r)
		return
	}

	p, ok := a.providers[provider]
	if !ok {
		http.Error(w, "unknown or unconfigured provider: "+provider, http.StatusNotFound)
		return
	}
	state := hex.EncodeToString(randomBytes(16))
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	http.Redirect(w, r, p.authCodeURL(a.redirectURI(provider), state), http.StatusFound)
}

// devLogin issues a session for a typed name without any external provider.
// Enabled by default in dev; disable in production with AUTH_DEV_ENABLED=false.
func (a *Auth) devLogin(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "guest"
	}
	slug := slugify(name)
	if slug == "" {
		slug = "guest"
	}
	u := &User{
		ID:          "dev:" + slug,
		Provider:    "dev",
		ProviderID:  slug,
		DisplayName: name,
		AvatarURL:   "",
	}
	if err := a.users.Upsert(r.Context(), u); err != nil {
		http.Error(w, "could not create user", http.StatusInternalServerError)
		return
	}
	a.setSession(w, u)
	http.Redirect(w, r, a.frontendURL, http.StatusFound)
}

// handleCallback completes a provider flow: verify state, exchange the code,
// fetch the user, upsert, set the session cookie, and bounce to the frontend.
func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	p, ok := a.providers[provider]
	if !ok {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		a.failLogin(w, r, "provider returned error: "+errParam)
		return
	}
	// CSRF: the state echoed back must match the one we stored in a cookie.
	stateCk, err := r.Cookie(stateCookie)
	if err != nil || stateCk.Value == "" || stateCk.Value != r.URL.Query().Get("state") {
		a.failLogin(w, r, "invalid oauth state")
		return
	}
	a.clearCookie(w, stateCookie)

	code := r.URL.Query().Get("code")
	if code == "" {
		a.failLogin(w, r, "missing authorization code")
		return
	}
	token, err := p.exchange(r.Context(), a.hc, code, a.redirectURI(provider))
	if err != nil {
		a.failLogin(w, r, "token exchange failed")
		log.Printf("auth: %s exchange: %v", provider, err)
		return
	}
	u, err := p.fetchUser(r.Context(), a.hc, token)
	if err != nil {
		a.failLogin(w, r, "could not fetch profile")
		log.Printf("auth: %s fetchUser: %v", provider, err)
		return
	}
	if err := a.users.Upsert(r.Context(), u); err != nil {
		a.failLogin(w, r, "could not save user")
		log.Printf("auth: upsert: %v", err)
		return
	}
	a.setSession(w, u)
	http.Redirect(w, r, a.frontendURL, http.StatusFound)
}

func (a *Auth) failLogin(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, a.frontendURL+"/?auth_error="+url.QueryEscape(reason), http.StatusFound)
}

/* -------------------------------- sessions -------------------------------- */

func (a *Auth) setSession(w http.ResponseWriter, u *User) {
	claims := sessionClaims{
		Sub:    u.ID,
		Name:   u.DisplayName,
		Avatar: u.AvatarURL,
		Exp:    time.Now().Add(sessionTTL).Unix(),
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    signToken(a.secret, claims),
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (a *Auth) clearSession(w http.ResponseWriter) { a.clearCookie(w, sessionCookie) }

func (a *Auth) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// sessionUser reads and verifies the session cookie.
func (a *Auth) sessionUser(r *http.Request) (*sessionClaims, bool) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return nil, false
	}
	claims, err := verifyToken(a.secret, ck.Value)
	if err != nil {
		return nil, false
	}
	return claims, true
}

func (a *Auth) redirectURI(provider string) string {
	return a.publicURL + "/auth/" + provider + "/callback"
}

/* -------------------------------- helpers --------------------------------- */

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
