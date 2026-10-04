package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
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

type ComboModel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`     // Combo model identifier (e.g. "agrouter", "combo-fast")
	Strategy  string   `json:"strategy"` // "fallback" | "round-robin" | "random"
	Models    []string `json:"models"`   // ordered list of models
	CreatedAt string   `json:"createdAt"`
	cursor    int      `json:"-"`
}

func (c *ComboModel) resolveModelsLocked() []string {
	n := len(c.Models)
	if n == 0 {
		return nil
	}
	switch c.Strategy {
	case "round-robin":
		start := c.cursor % n
		c.cursor = (c.cursor + 1) % n
		res := make([]string, n)
		for i := 0; i < n; i++ {
			res[i] = c.Models[(start+i)%n]
		}
		return res
	case "random":
		shuffled := make([]string, n)
		copy(shuffled, c.Models)
		b := make([]byte, n)
		rand.Read(b)
		for i := n - 1; i > 0; i-- {
			j := int(b[i]) % (i + 1)
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		}
		return shuffled
	default: // "fallback"
		res := make([]string, n)
		copy(res, c.Models)
		return res
	}
}

type Store struct {
	mu                 sync.Mutex
	path               string
	Accounts           []*Account    `json:"accounts"`
	AdminToken         string        `json:"adminToken,omitempty"`
	APIKeys            []*APIKey     `json:"apiKeys,omitempty"`
	AutoDeleteDepleted bool          `json:"autoDeleteDepleted"`
	AutoDeleteMode     string        `json:"autoDeleteMode,omitempty"` // "both" | "gemini" | "claude"
	Combos             []*ComboModel `json:"combos,omitempty"`
	rr                 int           // round-robin cursor
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
	if s.AutoDeleteMode == "" {
		s.AutoDeleteMode = "both"
	}
	if len(s.Combos) == 0 {
		s.Combos = []*ComboModel{
			{
				ID:        newID(),
				Name:      "agrouter",
				Strategy:  "fallback",
				Models:    []string{"gemini-3.8-flash-high", "claude-sonnet-4-6"},
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		}
		_ = s.save()
	}
	return s, nil
}

type storeSnapshot struct {
	Accounts           []*Account    `json:"accounts"`
	AdminToken         string        `json:"adminToken,omitempty"`
	APIKeys            []*APIKey     `json:"apiKeys,omitempty"`
	AutoDeleteDepleted bool          `json:"autoDeleteDepleted"`
	AutoDeleteMode     string        `json:"autoDeleteMode,omitempty"`
	Combos             []*ComboModel `json:"combos,omitempty"`
}

// save writes the store to disk. Caller MUST hold s.mu.
func (s *Store) save() error {
	accs := make([]*Account, len(s.Accounts))
	for i, a := range s.Accounts {
		copyA := *a
		accs[i] = &copyA
	}
	keys := make([]*APIKey, len(s.APIKeys))
	for i, k := range s.APIKeys {
		copyK := *k
		keys[i] = &copyK
	}
	combos := make([]*ComboModel, len(s.Combos))
	for i, c := range s.Combos {
		copyC := *c
		if c.Models != nil {
			copyC.Models = append([]string(nil), c.Models...)
		}
		combos[i] = &copyC
	}

	snap := storeSnapshot{
		Accounts:           accs,
		AdminToken:         s.AdminToken,
		APIKeys:            keys,
		AutoDeleteDepleted: s.AutoDeleteDepleted,
		AutoDeleteMode:     s.AutoDeleteMode,
		Combos:             combos,
	}

	b, err := json.MarshalIndent(snap, "", "  ")
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
	defer s.mu.Unlock()
	a.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	a.ReqCount++
	s.save()
}

func (s *Store) markError(a *Account, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.LastErr = msg
	a.ErrCount++
	s.save()
}

func (s *Store) isAutoDeleteDepleted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AutoDeleteDepleted
}

func (s *Store) getAutoDeleteConfig() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mode := s.AutoDeleteMode
	if mode == "" {
		mode = "gemini"
	}
	return s.AutoDeleteDepleted, mode
}

func (s *Store) setAutoDeleteConfig(enabled bool, mode string) {
	s.mu.Lock()
	s.AutoDeleteDepleted = enabled
	if mode == "" {
		mode = "gemini"
	}
	s.AutoDeleteMode = mode
	s.save()
	s.mu.Unlock()
}

func (s *Store) setAutoDeleteDepleted(v bool) {
	s.mu.Lock()
	s.AutoDeleteDepleted = v
	s.save()
	s.mu.Unlock()
}

func (s *Store) removeAccount(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.Accounts {
		if a.ID == id {
			s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
			s.save()
			return true
		}
	}
	return false
}

func (s *Store) resolveCandidateModels(modelName string) ([]string, string) {
	m := strings.TrimPrefix(modelName, "ag/")
	m = strings.TrimPrefix(m, "agr/")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.Combos {
		if c.Name == m || c.Name == modelName {
			return c.resolveModelsLocked(), c.Name
		}
	}
	return []string{m}, ""
}

func (s *Store) getCombos() []*ComboModel {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*ComboModel, len(s.Combos))
	copy(out, s.Combos)
	return out
}

func (s *Store) upsertCombo(c *ComboModel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == "" {
		c.ID = newID()
	}
	if c.CreatedAt == "" {
		c.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if c.Strategy == "" {
		c.Strategy = "fallback"
	}
	found := false
	for i, existing := range s.Combos {
		if existing.ID == c.ID || existing.Name == c.Name {
			c.ID = existing.ID
			s.Combos[i] = c
			found = true
			break
		}
	}
	if !found {
		s.Combos = append(s.Combos, c)
	}
	return s.save()
}

func (s *Store) deleteCombo(idOrName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.Combos {
		if c.ID == idOrName || c.Name == idOrName {
			s.Combos = append(s.Combos[:i], s.Combos[i+1:]...)
			s.save()
			return true
		}
	}
	return false
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
	agUpstreamTimeout = getEnvDuration("AGROUTER_UPSTREAM_TIMEOUT", 120*time.Second)
)

func getEnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	var sec int
	if _, err := fmt.Sscanf(v, "%d", &sec); err == nil && sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return def
}

var defaultTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 15 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   20,
	IdleConnTimeout:       30 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ResponseHeaderTimeout: getEnvDuration("AGROUTER_HEADER_TIMEOUT", 25*time.Second),
}

var httpClient = &http.Client{
	Timeout:   agUpstreamTimeout,
	Transport: defaultTransport,
}

// proxiedClient returns a client bound to the account's proxy (if any).
func proxiedClient(a *Account) *http.Client {
	if a.ProxyURL == "" {
		return httpClient
	}
	transport := defaultTransport.Clone()
	if u, err := url.Parse(a.ProxyURL); err == nil {
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Timeout: agUpstreamTimeout, Transport: transport}
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
	return proxiedClient(a)
}
