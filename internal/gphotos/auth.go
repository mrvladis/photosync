// Package gphotos talks to the Google Photos Library API.
//
// Since 31 March 2025 the broad photoslibrary scopes are gone: an application
// can add media to the library and read back only what it created itself. That
// shapes the whole design here - we can upload, and we can verify our own
// uploads, but we cannot enumerate the user's existing library to check whether
// a photo is already there. What makes blind uploading safe is that Google
// Photos deduplicates byte-identical uploads within an account, so re-sending a
// file that is already present adds no second copy.
package gphotos

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"

	// ScopeAppend permits uploading media and creating albums.
	ScopeAppend = "https://www.googleapis.com/auth/photoslibrary.appendonly"
	// ScopeReadAppCreated permits reading back only what this app created,
	// which is exactly what the verification pass needs.
	ScopeReadAppCreated = "https://www.googleapis.com/auth/photoslibrary.readonly.appcreateddata"
)

// ClientSecret is the installed-application credential downloaded from the
// Google Cloud console. Both the "installed" and "web" shapes are accepted.
type ClientSecret struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func LoadClientSecret(path string) (ClientSecret, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ClientSecret{}, fmt.Errorf("read client secret: %w", err)
	}
	var wrapper struct {
		Installed *ClientSecret `json:"installed"`
		Web       *ClientSecret `json:"web"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return ClientSecret{}, fmt.Errorf("parse client secret: %w", err)
	}
	switch {
	case wrapper.Installed != nil && wrapper.Installed.ClientID != "":
		return *wrapper.Installed, nil
	case wrapper.Web != nil && wrapper.Web.ClientID != "":
		return *wrapper.Web, nil
	}
	var flat ClientSecret
	if err := json.Unmarshal(raw, &flat); err == nil && flat.ClientID != "" {
		return flat, nil
	}
	return ClientSecret{}, fmt.Errorf("%s contains no client_id", path)
}

// Token is the stored credential. Only the refresh token is durable; the access
// token is re-minted as needed.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	Scope        string    `json:"scope"`
}

func (t Token) valid() bool {
	return t.AccessToken != "" && time.Now().Add(2*time.Minute).Before(t.Expiry)
}

// Auth holds credentials and keeps the access token fresh.
type Auth struct {
	secret    ClientSecret
	tokenPath string

	mu    sync.Mutex
	token Token
}

func NewAuth(secret ClientSecret, tokenPath string) (*Auth, error) {
	a := &Auth{secret: secret, tokenPath: tokenPath}
	if raw, err := os.ReadFile(tokenPath); err == nil {
		if err := json.Unmarshal(raw, &a.token); err != nil {
			return nil, fmt.Errorf("parse %s: %w", tokenPath, err)
		}
	}
	return a, nil
}

// HasToken reports whether a refresh token is already stored.
func (a *Auth) HasToken() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token.RefreshToken != ""
}

// AccessToken returns a usable bearer token, refreshing it if necessary.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token.valid() {
		return a.token.AccessToken, nil
	}
	if a.token.RefreshToken == "" {
		return "", fmt.Errorf("not authorised yet - run `photosync auth`")
	}
	if err := a.refresh(ctx); err != nil {
		return "", err
	}
	return a.token.AccessToken, nil
}

func (a *Auth) refresh(ctx context.Context) error {
	form := url.Values{
		"client_id":     {a.secret.ClientID},
		"client_secret": {a.secret.ClientSecret},
		"refresh_token": {a.token.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	var resp tokenResponse
	if err := postForm(ctx, tokenEndpoint, form, &resp); err != nil {
		return fmt.Errorf("refresh access token: %w", err)
	}
	a.token.AccessToken = resp.AccessToken
	a.token.Expiry = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	if resp.RefreshToken != "" {
		a.token.RefreshToken = resp.RefreshToken
	}
	return a.save()
}

func (a *Auth) save() error {
	if err := os.MkdirAll(filepath.Dir(a.tokenPath), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(a.token, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.tokenPath, blob, 0o600)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// Authorize runs the loopback OAuth flow: start a local listener, send the user
// to Google's consent page, and swap the returned code for tokens. PKCE is used
// so the exchange cannot be replayed by anything else that saw the redirect.
func (a *Auth) Authorize(ctx context.Context, open bool) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open loopback listener: %w", err)
	}
	defer listener.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	verifier := randomString(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString(24)

	authURL := authEndpoint + "?" + url.Values{
		"client_id":             {a.secret.ClientID},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"scope":                 {strings.Join([]string{ScopeAppend, ScopeReadAppCreated}, " ")},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"access_type":           {"offline"},
		// Without this, a re-authorisation returns no refresh token and the
		// next multi-day run dies when the access token lapses.
		"prompt": {"consent"},
	}.Encode()

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			results <- result{err: fmt.Errorf("state mismatch on OAuth callback")}
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			results <- result{err: fmt.Errorf("authorisation refused: %s", e)}
			return
		}
		fmt.Fprint(w, "photosync is authorised. You can close this tab and return to the terminal.")
		results <- result{code: q.Get("code")}
	})}
	go srv.Serve(listener)
	defer srv.Close()

	fmt.Println("Open this URL to authorise photosync:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	if open {
		_ = exec.Command("open", authURL).Start()
	}

	var res result
	select {
	case res = <-results:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Minute):
		return fmt.Errorf("timed out waiting for authorisation")
	}
	if res.err != nil {
		return res.err
	}

	var resp tokenResponse
	err = postForm(ctx, tokenEndpoint, url.Values{
		"client_id":     {a.secret.ClientID},
		"client_secret": {a.secret.ClientSecret},
		"code":          {res.code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirect},
	}, &resp)
	if err != nil {
		return fmt.Errorf("exchange authorisation code: %w", err)
	}
	if resp.RefreshToken == "" {
		return fmt.Errorf("Google returned no refresh token; revoke photosync at " +
			"https://myaccount.google.com/permissions and authorise again")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = Token{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second),
		Scope:        resp.Scope,
	}
	return a.save()
}

func postForm(ctx context.Context, endpoint string, form url.Values, out *tokenResponse) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode token response (HTTP %d): %w", resp.StatusCode, err)
	}
	if out.Error != "" {
		return fmt.Errorf("%s: %s", out.Error, out.ErrorDesc)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}
