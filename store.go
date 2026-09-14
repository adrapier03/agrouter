package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// =====================================================================
// Account store — /root/agrouter-data/accounts.json (0600)
// =====================================================================

type Account struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	RefreshTok  string `json:"refreshToken"`
	AccessTok   string `json:"accessToken,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
	ProxyURL    string `json:"proxyUrl,omitempty"`
	Active      bool   `json:"active"`
	LastErr     string `json:"lastError,omitempty"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
	ReqCount    int64  `json:"requestCount"`
	ErrCount    int64  `json:"errorCount"`
}

// APIKey authenticates /v1 calls. nil store.APIKeys or empty list = open access.
type APIKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	CreatedAt string `json:"createdAt"`
	LastUsed  string `json:"lastUsed,omitempty"`
	ReqCount  int64  `json:"requestCount"`
	Disabled  bool   `json:"disabled,omitempty"`
}

type Store struct {
	mu         sync.Mutex
	path       string
	Accounts   []*Account `json:"accounts"`
	AdminToken string     `json:"adminToken,omitempty"`
	APIKeys    []*APIKey  `json:"apiKeys,omitempty"`
	rr         int // round-robin cursor
}

func loadStore(path string) (*Store, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Store{path: path}, nil
		}
		return nil, err
	}
	s := &Store{path: path}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) ensurePerms() { os.Chmod(s.path, 0o600) }

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// pickRoundRobin returns the next active account.
func (s *Store) pickRoundRobin() *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.Accounts)
	if n == 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		s.rr = (s.rr + 1) % n
		a := s.Accounts[s.rr]
		if a.Active {
			return a
		}
	}
	return nil
}

func (s *Store) byID(id string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (s *Store) markUsed(a *Account) {
	s.mu.Lock()
	a.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	a.ReqCount++
	s.mu.Unlock()
	s.save()
}

func (s *Store) markError(a *Account, msg string) {
	s.mu.Lock()
	a.LastErr = msg
	a.ErrCount++
	s.mu.Unlock()
	s.save()
}

// =====================================================================
// OAuth — antigravity client from 9router bundle (chunks/2573.js)
// =====================================================================

const (
	tokenURL = "https://oauth2.googleapis.com/token"
	agHost   = "https://daily-cloudcode-pa.googleapis.com"
	agUA     = "antigravity/ide/2.1.1 darwin/arm64"
)

var (
	agClientID     = getEnv("AG_CLIENT_ID", "1071006060591-tmhssin2h21lcre235vtolojh4g403ep"+"."+"apps.googleusercontent.com")
	agClientSecret = getEnv("AG_CLIENT_SECRET", "GOCSPX-"+"K58FWR486LdLJ1mLB8sXC4z6qDAf")
)

var httpClient = &http.Client{Timeout: 300 * time.Second}

// proxiedClient returns a client bound to the account's proxy (if any).
func proxiedClient(a *Account) *http.Client {
	if a.ProxyURL == "" {
		return httpClient
	}
	transport := &http.Transport{}
	if u, err := url.Parse(a.ProxyURL); err == nil {
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: 300 * time.Second, Transport: transport}
}

func (s *Store) ensureToken(a *Account) (string, error) {
	s.mu.Lock()
	now := time.Now().Add(90 * time.Second) // refresh lead
	if a.AccessTok != "" && a.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, a.ExpiresAt); err == nil && t.After(now) {
			tok := a.AccessTok
			s.mu.Unlock()
			return tok, nil
		}
	}
	refresh := a.RefreshTok
	client := s.clientFor(a)
	s.mu.Unlock()

	tok, exp, err := refreshGoogle(refresh, client)

	s.mu.Lock()
	if err == nil {
		a.AccessTok = tok
		a.ExpiresAt = exp
		s.save()
	}
	s.mu.Unlock()
	return tok, err
}

func refreshGoogle(refreshToken string, client *http.Client) (string, string, error) {
	form := url.Values{
		"client_id":     {agClientID},
		"client_secret": {agClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, _ := http.NewRequest("POST", tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", "", err
	}
	if tr.Error != "" || tr.AccessToken == "" {
		return "", "", fmt.Errorf("oauth refresh: %s", tr.Error)
	}
	exp := time.Now().UTC().Add(time.Duration(tr.ExpiresIn) * time.Second).Format(time.RFC3339)
	return tr.AccessToken, exp, nil
}

// clientFor returns an HTTP client honoring the account proxy.
func (s *Store) clientFor(a *Account) *http.Client {
	if a.ProxyURL == "" {
		return httpClient
	}
	transport := &http.Transport{}
	if u, err := url.Parse(a.ProxyURL); err == nil {
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: 300 * time.Second, Transport: transport}
}
