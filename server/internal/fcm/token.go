package fcm

// This file holds credential loading and the OAuth2 token source.
//
// Google's service-account flow is a self-signed RS256 JWT exchanged for an
// access token at the OAuth2 token endpoint. Both steps use only the Go
// standard library (crypto/rsa, crypto/x509, encoding/{base64,json,pem}),
// which keeps the single static binary free of the large
// google.golang.org/api client tree while remaining fully correct against the
// documented FCM v1 / OAuth2 contracts. The transport is the caller-supplied
// *http.Client, so tests (and the integration skeleton) drive the same code
// path through an injectable client.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// defaultTokenURI is Google's OAuth2 token endpoint.
	defaultTokenURI = "https://oauth2.googleapis.com/token"
	// tokenTTL is the lifetime of a minted access token. Google issues
	// 3600s; we request the same and refresh slightly before expiry.
	tokenTTL = time.Hour
	// tokenRefreshLead refreshes a cached token this long before it expires,
	// so a refresh never races real expiry.
	tokenRefreshLead = 30 * time.Second
	// jwtMaxTTL bounds the assertion lifetime per Google's guidance.
	jwtMaxTTL = time.Hour
)

// serviceAccount is the subset of the Google service-account JSON the token
// source needs. Only the fields used are decoded so unrelated keys are
// tolerated.
type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

// loadServiceAccount reads and parses a service-account JSON file from path.
// It validates that the key parses as RSA so a bad credential fails at load
// rather than at first send.
func loadServiceAccount(path string) (*serviceAccount, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var sa serviceAccount
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	if strings.ToLower(strings.TrimSpace(sa.Type)) != "service_account" {
		return nil, fmt.Errorf("credentials %q: type %q is not \"service_account\"", path, sa.Type)
	}
	if sa.PrivateKey == "" {
		return nil, fmt.Errorf("credentials %q: private_key is empty", path)
	}
	// Eagerly validate the key so load-time errors surface before send.
	if _, err := parseRSAPrivateKey([]byte(sa.PrivateKey)); err != nil {
		return nil, fmt.Errorf("credentials %q: parse private_key: %w", path, err)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = defaultTokenURI
	}
	return &sa, nil
}

// parseRSAPrivateKey parses a PEM-encoded RSA private key in either PKCS#8 or
// PKCS#1 form (Google emits PKCS#8 "BEGIN PRIVATE KEY").
func parseRSAPrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is not RSA (%T)", k)
		}
		return rk, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	// Fall back to EC/PKIX last; RSA is what FCM service accounts use.
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		return nil, fmt.Errorf("PEM contains a public key, not a private key (%T)", k)
	}
	return nil, fmt.Errorf("key is neither PKCS#8 nor PKCS#1 RSA")
}

// serviceAccountTokenSource mints OAuth2 access tokens by signing a RS256 JWT
// assertion with the service-account private key and exchanging it at the
// token endpoint. Tokens are cached until shortly before expiry.
type serviceAccountTokenSource struct {
	http     *http.Client
	sa       *serviceAccount
	key      *rsa.PrivateKey
	tokenURI string

	mu       sync.Mutex
	cached   string
	notAfter time.Time
	now      func() time.Time
}

// newServiceAccountTokenSource builds a ready token source.
func newServiceAccountTokenSource(httpClient *http.Client, sa *serviceAccount) (*serviceAccountTokenSource, error) {
	key, err := parseRSAPrivateKey([]byte(sa.PrivateKey))
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &serviceAccountTokenSource{
		http:     httpClient,
		sa:       sa,
		key:      key,
		tokenURI: sa.TokenURI,
		now:      nowUTC,
	}, nil
}

// Token returns a valid bearer token, refreshing the cache when it is missing
// or about to expire. Refreshing is serialized under mu so concurrent sends
// share one token.
func (s *serviceAccountTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.cached != "" && s.now().Before(s.notAfter) {
		tok := s.cached
		s.mu.Unlock()
		return tok, nil
	}
	s.mu.Unlock()

	assertion, err := s.assertion()
	if err != nil {
		return "", err
	}
	tok, exp, err := s.exchange(ctx, assertion)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.cached = tok
	s.notAfter = exp.Add(-tokenRefreshLead)
	s.mu.Unlock()
	return tok, nil
}

// assertion builds and signs the RS256 JWT bearer assertion.
func (s *serviceAccountTokenSource) assertion() (string, error) {
	now := s.now()
	header := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	}
	if s.sa.PrivateKeyID != "" {
		header["kid"] = s.sa.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   s.sa.ClientEmail,
		"scope": tokenScope,
		"aud":   s.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(jwtMaxTTL).Unix(),
	}
	headerB, _ := json.Marshal(header)
	claimsB, _ := json.Marshal(claims)
	signingInput := b64url(headerB) + "." + b64url(claimsB)
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("sign assertion: %w", err)
	}
	return signingInput + "." + b64url(sig), nil
}

// exchange posts the JWT assertion to the token endpoint and returns the
// access token plus its absolute expiry.
func (s *serviceAccountTokenSource) exchange(ctx context.Context, assertion string) (string, time.Time, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("token response missing access_token")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = tokenTTL
	}
	return tr.AccessToken, s.now().Add(ttl), nil
}

// b64url is raw URL-safe base64 without padding (JWT encoding).
func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
