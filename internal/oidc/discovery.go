package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// discoveryTimeout bounds one fetch of the provider metadata. With the
	// token exchange and the JWKS fetch it stays within the server
	// WriteTimeout.
	discoveryTimeout = 3 * time.Second
	// maxDiscoverySize bounds the provider metadata response (1 MiB).
	maxDiscoverySize = 1 << 20
	// discoveryPath is appended to the issuer, without its trailing slash,
	// to read the provider metadata (OpenID Connect Discovery 1.0 section 4).
	discoveryPath = "/.well-known/openid-configuration"
)

// errDiscoveryThrottled is returned without fetching while the last failed
// attempt was less than fetchInterval ago.
var errDiscoveryThrottled = errors.New("the last attempt failed less than 30 seconds ago")

// provider is the part of the provider metadata that is used.
type provider struct {
	// authorization is the authorization endpoint; its query, if any, is
	// kept and the sign-in parameters are added to it.
	authorization *url.URL
	// token and keys are the token endpoint and the JWKS URL, used as they
	// are.
	token, keys string
	// basic is whether the client authenticates with client_secret_basic;
	// otherwise it uses client_secret_post.
	basic bool
}

// discovery reads the provider metadata of issuer once. A success is kept
// until the process ends; a failure is not kept, but no other fetch is made
// within fetchInterval of an attempt, so that anonymous requests cannot
// make many requests to the provider.
type discovery struct {
	issuer string
	client *http.Client
	now    func() time.Time

	// result is the metadata once fetched.
	result atomic.Pointer[provider]
	// mu serializes the fetches and guards attemptedAt.
	mu          sync.Mutex
	attemptedAt time.Time
}

// get returns the provider metadata, fetching it when it has not been
// fetched yet. Concurrent calls wait for one fetch. The fetch has its own
// timeout instead of a request context, so that a client going away cannot
// cancel it and make the next ones wait for fetchInterval.
func (d *discovery) get() (*provider, error) {
	if p := d.result.Load(); p != nil {
		return p, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Fetched while this call waited for the lock.
	if p := d.result.Load(); p != nil {
		return p, nil
	}
	now := d.now()
	if !d.attemptedAt.IsZero() && now.Sub(d.attemptedAt) < fetchInterval {
		return nil, errDiscoveryThrottled
	}
	d.attemptedAt = now
	p, err := d.fetch()
	if err != nil {
		log.Printf("oidc: discovery: %v", err)
		return nil, err
	}
	d.result.Store(p)
	return p, nil
}

// metadata is the part of the provider metadata document that is read.
// Absent and null members are nil.
type metadata struct {
	Issuer                string    `json:"issuer"`
	AuthorizationEndpoint string    `json:"authorization_endpoint"`
	TokenEndpoint         string    `json:"token_endpoint"`
	JWKSURI               string    `json:"jwks_uri"`
	SigningAlgs           []string  `json:"id_token_signing_alg_values_supported"`
	AuthMethods           *[]string `json:"token_endpoint_auth_methods_supported"`
}

// fetch reads and checks the provider metadata. The errors name the reason
// only, never the response.
func (d *discovery) fetch() (*provider, error) {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(d.issuer, "/")+discoveryPath, nil)
	if err != nil {
		return nil, errors.New("cannot build the request")
	}
	req.Header.Set("Accept", "application/json")
	res, err := d.client.Do(req)
	if err != nil {
		// The error names the URL, which is derived from the issuer only.
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", res.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, errors.New("response is not JSON")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxDiscoverySize+1))
	if err != nil {
		return nil, errors.New("cannot read the response")
	}
	if len(body) > maxDiscoverySize {
		return nil, errors.New("response is larger than 1 MiB")
	}
	var m metadata
	if err = json.Unmarshal(body, &m); err != nil {
		return nil, errors.New("response is not provider metadata")
	}
	// OpenID Connect Discovery 1.0 section 4.3: exactly the configured
	// issuer.
	if m.Issuer != d.issuer {
		return nil, errors.New("issuer does not match ASTOVIA_OIDC_ISSUER")
	}
	authorization, err := endpoint(m.AuthorizationEndpoint)
	if err != nil {
		return nil, fmt.Errorf("authorization_endpoint %w", err)
	}
	if _, err = endpoint(m.TokenEndpoint); err != nil {
		return nil, fmt.Errorf("token_endpoint %w", err)
	}
	if _, err = endpoint(m.JWKSURI); err != nil {
		return nil, fmt.Errorf("jwks_uri %w", err)
	}
	if !slices.Contains(m.SigningAlgs, "RS256") {
		return nil, errors.New("id_token_signing_alg_values_supported does not include RS256")
	}
	var basic bool
	switch {
	case m.AuthMethods == nil || slices.Contains(*m.AuthMethods, "client_secret_basic"):
		basic = true
	case slices.Contains(*m.AuthMethods, "client_secret_post"):
		basic = false
	default:
		return nil, errors.New("token_endpoint_auth_methods_supported includes neither client_secret_basic nor client_secret_post")
	}
	return &provider{authorization: authorization, token: m.TokenEndpoint, keys: m.JWKSURI, basic: basic}, nil
}

// endpoint checks that v is an absolute https URL without credentials or
// a fragment. A query is allowed.
func endpoint(v string) (*url.URL, error) {
	if v == "" {
		return nil, errors.New("is missing")
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" || strings.Contains(v, "#") {
		return nil, errors.New("is not an https URL without credentials or fragment")
	}
	return u, nil
}
