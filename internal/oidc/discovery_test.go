package oidc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDiscovery(t *testing.T) {
	s := newProviderServer(t)
	c, _ := newTestClient(s)
	p, err := c.discovery.get()
	if err != nil {
		t.Fatal(err)
	}
	base := s.URL + oidcPath
	if p.authorization.String() != base+"/auth" || p.token != base+"/token" || p.keys != base+"/jwks" || !p.basic {
		t.Fatalf("%+v", p)
	}
	// Endpoints with queries and ports other than the issuer's.
	s.set(func(s *providerServer) {
		s.edit = func(m map[string]any) {
			m["token_endpoint"] = "https://token.example.com:8443/t?x=1"
			m["jwks_uri"] = "https://keys.example.com/k?v=1"
			m["id_token_signing_alg_values_supported"] = []string{"ES256", "RS256"}
		}
	})
	c, _ = newTestClient(s)
	if p, err = c.discovery.get(); err != nil || p.token != "https://token.example.com:8443/t?x=1" || p.keys != "https://keys.example.com/k?v=1" {
		t.Fatalf("%+v %v", p, err)
	}
}

// An issuer with a trailing slash reads the metadata without a double
// slash, and the metadata must have the same issuer, slash included.
func TestDiscoveryIssuerWithSlash(t *testing.T) {
	s := newProviderServer(t)
	s.set(func(s *providerServer) {
		s.edit = func(m map[string]any) { m["issuer"] = testIssuer(s.URL) + "/" }
	})
	c, _ := newTestClientWithIssuer(s, testIssuer(s.URL)+"/")
	if _, err := c.discovery.get(); err != nil {
		t.Fatal(err)
	}
	// The same metadata does not match the issuer without the slash.
	c, _ = newTestClient(s)
	if _, err := c.discovery.get(); err == nil {
		t.Fatal("issuer with a slash accepted")
	}
}

func TestDiscoveryRejects(t *testing.T) {
	logs := captureLog(t)
	for name, edit := range map[string]func(s *providerServer){
		"status 404":       func(s *providerServer) { s.metaStatus = http.StatusNotFound },
		"status 500":       func(s *providerServer) { s.metaStatus = http.StatusInternalServerError },
		"redirect":         func(s *providerServer) { s.metaStatus = http.StatusFound },
		"text/html":        func(s *providerServer) { s.metaType = "text/html" },
		"no content type":  func(s *providerServer) { s.metaType = "" },
		"not JSON":         func(s *providerServer) { s.metaBody = "<html>" },
		"larger than 1MiB": func(s *providerServer) { s.metaBody = `{"x":"` + strings.Repeat("a", 1<<20) + `"}` },
		"issuer mismatch": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["issuer"] = "https://other.example.com" }
		},
		"issuer with slash": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["issuer"] = m["issuer"].(string) + "/" }
		},
		"no issuer": func(s *providerServer) { s.edit = func(m map[string]any) { m["issuer"] = nil } },
		"issuer number": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["issuer"] = 1 }
		},
		"no authorization_endpoint": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["authorization_endpoint"] = nil }
		},
		"http authorization_endpoint": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["authorization_endpoint"] = "http://example.com/auth" }
		},
		"relative authorization_endpoint": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["authorization_endpoint"] = "/auth" }
		},
		"authorization_endpoint with userinfo": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["authorization_endpoint"] = "https://user:pass@example.com/auth" }
		},
		"authorization_endpoint with fragment": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["authorization_endpoint"] = "https://example.com/auth#x" }
		},
		"no token_endpoint": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["token_endpoint"] = nil }
		},
		"http token_endpoint": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["token_endpoint"] = "http://example.com/token" }
		},
		"token_endpoint with fragment": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["token_endpoint"] = "https://example.com/token#" }
		},
		"no jwks_uri": func(s *providerServer) { s.edit = func(m map[string]any) { m["jwks_uri"] = nil } },
		"http jwks_uri": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["jwks_uri"] = "http://example.com/jwks" }
		},
		"jwks_uri with userinfo": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["jwks_uri"] = "https://u@example.com/jwks" }
		},
		"no signing algorithms": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["id_token_signing_alg_values_supported"] = nil }
		},
		"no RS256": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["id_token_signing_alg_values_supported"] = []string{"ES256", "rs256"} }
		},
		"RS256 as a string": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["id_token_signing_alg_values_supported"] = "RS256" }
		},
		"no auth method": func(s *providerServer) {
			s.edit = func(m map[string]any) { m["token_endpoint_auth_methods_supported"] = []string{} }
		},
		"unsupported auth methods": func(s *providerServer) {
			s.edit = func(m map[string]any) {
				m["token_endpoint_auth_methods_supported"] = []string{"private_key_jwt", "client_secret_jwt"}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newProviderServer(t)
			s.set(edit)
			c, _ := newTestClient(s)
			if _, err := c.AuthorizationURL("s", "n", "c"); !errors.Is(err, ErrProviderUnavailable) {
				t.Fatalf("%v", err)
			}
		})
	}
	// Only the reasons are logged, never the response.
	if strings.Contains(logs.String(), "<html>") || strings.Contains(logs.String(), "other.example.com") {
		t.Fatalf("log contains the response: %s", logs.String())
	}
	for _, reason := range []string{"oidc: discovery: issuer does not match", "oidc: discovery: id_token_signing_alg_values_supported does not include RS256", "oidc: discovery: response is not JSON"} {
		if !strings.Contains(logs.String(), reason) {
			t.Fatalf("log lacks %q: %s", reason, logs.String())
		}
	}
}

// A failure is not kept, but no other fetch is made within 30 seconds of an
// attempt. A success is kept for good.
func TestDiscoveryRetryAndCache(t *testing.T) {
	s := newProviderServer(t)
	s.set(func(s *providerServer) { s.metaStatus = http.StatusServiceUnavailable })
	c, clk := newTestClient(s)
	for range 3 {
		if _, err := c.AuthorizationURL("s", "n", "c"); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatal(err)
		}
	}
	if n := s.discoveries.Load(); n != 1 {
		t.Fatalf("discoveries within 30s: %d", n)
	}
	// The provider is back, but the last attempt was less than 30 seconds
	// ago.
	s.set(func(s *providerServer) { s.metaStatus = http.StatusOK })
	clk.add(29 * time.Second)
	if _, err := c.AuthorizationURL("s", "n", "c"); !errors.Is(err, ErrProviderUnavailable) || s.discoveries.Load() != 1 {
		t.Fatalf("%v, discoveries %d", err, s.discoveries.Load())
	}
	clk.add(2 * time.Second)
	if _, err := c.AuthorizationURL("s", "n", "c"); err != nil {
		t.Fatal(err)
	}
	if n := s.discoveries.Load(); n != 2 {
		t.Fatalf("discoveries after 30s: %d", n)
	}
	// Kept for good, even when the provider fails later.
	s.set(func(s *providerServer) { s.metaStatus = http.StatusInternalServerError })
	clk.add(24 * time.Hour)
	for range 3 {
		if _, err := c.AuthorizationURL("s", "n", "c"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exchange(context.Background(), "code", "verifier"); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.discoveries.Load(); n != 2 {
		t.Fatalf("discoveries after success: %d", n)
	}
}

// Concurrent first uses wait for one fetch.
func TestDiscoveryConcurrentFetch(t *testing.T) {
	s := newProviderServer(t)
	s.set(func(s *providerServer) {
		s.block = make(chan struct{})
		s.started = make(chan struct{}, 4)
	})
	c, _ := newTestClient(s)
	errs := make(chan error, 3)
	for range 3 {
		go func() {
			_, err := c.AuthorizationURL("s", "n", "c")
			errs <- err
		}()
	}
	<-s.started
	// Give the other calls time to reach the lock.
	time.Sleep(50 * time.Millisecond)
	s.set(func(s *providerServer) { close(s.block) })
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := s.discoveries.Load(); n != 1 {
		t.Fatalf("discoveries: %d", n)
	}
}

// The fetch has its own context: a caller that went away does not cancel
// it, and its result is kept for the next calls.
func TestDiscoveryIgnoresCallerContext(t *testing.T) {
	s := newProviderServer(t)
	c, _ := newTestClient(s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The token request itself uses the canceled context.
	if _, err := c.Exchange(ctx, "code", "verifier"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("exchange: %v", err)
	}
	if n := s.discoveries.Load(); n != 1 {
		t.Fatalf("discoveries: %d", n)
	}
	if _, err := c.AuthorizationURL("s", "n", "c"); err != nil {
		t.Fatal(err)
	}
	if n := s.discoveries.Load(); n != 1 {
		t.Fatalf("discoveries: %d", n)
	}
}
