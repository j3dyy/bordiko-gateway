package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// OAuth2 authorization-code flow for Google and GitHub, implemented on the
// standard library (no oauth2 dependency). Each provider knows how to build a
// consent URL, exchange a code for an access token, and fetch a normalized
// User. Providers are only "configured" (and offered to the browser) when their
// client id + secret are present in the environment.

type oauthProvider struct {
	name         string
	clientID     string
	clientSecret string
	authURL      string
	tokenURL     string
	scopes       string
	fetchUser    func(ctx context.Context, hc *http.Client, accessToken string) (*User, error)
}

func (p *oauthProvider) configured() bool {
	return p.clientID != "" && p.clientSecret != ""
}

// authCodeURL builds the provider consent URL to redirect the browser to.
func (p *oauthProvider) authCodeURL(redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", p.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", p.scopes)
	q.Set("state", state)
	if p.name == "google" {
		q.Set("access_type", "online")
		q.Set("prompt", "select_account")
	}
	return p.authURL + "?" + q.Encode()
}

// exchange trades an authorization code for an access token.
func (p *oauthProvider) exchange(ctx context.Context, hc *http.Client, code, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json") // GitHub returns form-encoded otherwise
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange failed: %d %s", resp.StatusCode, truncate(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("no access_token in response (%s %s)", tok.Error, tok.ErrorDesc)
	}
	return tok.AccessToken, nil
}

func googleProvider(clientID, clientSecret string) *oauthProvider {
	return &oauthProvider{
		name:         "google",
		clientID:     clientID,
		clientSecret: clientSecret,
		authURL:      "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:     "https://oauth2.googleapis.com/token",
		scopes:       "openid email profile",
		fetchUser: func(ctx context.Context, hc *http.Client, accessToken string) (*User, error) {
			var info struct {
				Sub     string `json:"sub"`
				Name    string `json:"name"`
				Email   string `json:"email"`
				Picture string `json:"picture"`
			}
			if err := getJSON(ctx, hc, "https://openidconnect.googleapis.com/v1/userinfo", accessToken, &info); err != nil {
				return nil, err
			}
			if info.Sub == "" {
				return nil, fmt.Errorf("google userinfo missing sub")
			}
			name := info.Name
			if name == "" {
				name = info.Email
			}
			return &User{
				ID:          "google:" + info.Sub,
				Provider:    "google",
				ProviderID:  info.Sub,
				DisplayName: name,
				AvatarURL:   info.Picture,
				Email:       info.Email,
			}, nil
		},
	}
}

func githubProvider(clientID, clientSecret string) *oauthProvider {
	return &oauthProvider{
		name:         "github",
		clientID:     clientID,
		clientSecret: clientSecret,
		authURL:      "https://github.com/login/oauth/authorize",
		tokenURL:     "https://github.com/login/oauth/access_token",
		scopes:       "read:user user:email",
		fetchUser: func(ctx context.Context, hc *http.Client, accessToken string) (*User, error) {
			var info struct {
				ID        int64  `json:"id"`
				Login     string `json:"login"`
				Name      string `json:"name"`
				AvatarURL string `json:"avatar_url"`
				Email     string `json:"email"`
			}
			if err := getJSON(ctx, hc, "https://api.github.com/user", accessToken, &info); err != nil {
				return nil, err
			}
			if info.ID == 0 {
				return nil, fmt.Errorf("github user missing id")
			}
			name := info.Name
			if name == "" {
				name = info.Login
			}
			email := info.Email
			if email == "" {
				email = primaryGitHubEmail(ctx, hc, accessToken)
			}
			return &User{
				ID:          "github:" + strconv.FormatInt(info.ID, 10),
				Provider:    "github",
				ProviderID:  strconv.FormatInt(info.ID, 10),
				DisplayName: name,
				AvatarURL:   info.AvatarURL,
				Email:       email,
			}, nil
		},
	}
}

// primaryGitHubEmail best-effort fetches the account's primary verified email
// (GitHub omits it from /user when the user keeps it private).
func primaryGitHubEmail(ctx context.Context, hc *http.Client, accessToken string) string {
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := getJSON(ctx, hc, "https://api.github.com/user/emails", accessToken, &emails); err != nil {
		return ""
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email
		}
	}
	return ""
}

// getJSON performs an authenticated GET and decodes a JSON response.
func getJSON(ctx context.Context, hc *http.Client, endpoint, accessToken string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bordiko-gateway")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", endpoint, resp.StatusCode, truncate(body))
	}
	return json.Unmarshal(body, out)
}
