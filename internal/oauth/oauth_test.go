package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"github.com/i2bskn/astovia/internal/config"
	"strings"
	"testing"
	"time"
)

func TestNormalizeScope(t *testing.T) {
	for raw, want := range map[string]string{
		"":                           "mcp",
		"   ":                        "mcp",
		"mcp":                        "mcp",
		"offline_access mcp":         "mcp offline_access",
		"mcp mcp offline_access mcp": "mcp offline_access",
		" mcp\toffline_access ":      "mcp offline_access",
	} {
		if got, err := NormalizeScope(raw); err != nil || got != want {
			t.Fatalf("%q: %q %v, want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"offline_access", "openid", "mcp openid", "MCP", "mcp:read"} {
		if got, err := NormalizeScope(raw); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("%q accepted: %q", raw, got)
		}
	}
	if !ScopeSubset("mcp", "mcp offline_access") || !ScopeSubset("mcp offline_access", "mcp offline_access") || ScopeSubset("mcp offline_access", "mcp") {
		t.Fatal("ScopeSubset")
	}
}

func TestRedirectURIRules(t *testing.T) {
	for _, v := range []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://chatgpt.com/connector_platform_oauth_redirect",
		"https://example.com:8443/cb?x=1",
		"https://example.com/Path/With/Upper?Q=Upper",
		"https://xn--bcher-kva.example/cb",
		"https://203.0.113.5/cb",
		"https://[2001:db8::1]/cb",
		"http://localhost/callback",
		"http://localhost:53682/callback",
		"http://127.0.0.1/callback",
		"http://127.0.0.2:8080/callback",
		"http://[::1]:8080/callback",
		"https://" + strings.Repeat("a", 63) + ".example/" + strings.Repeat("p", 400),
	} {
		if !ValidRedirectURI(v) {
			t.Fatalf("rejected %q", v)
		}
	}
	for _, v := range []string{
		"",
		"http://example.com/cb",          // http, not loopback
		"http://192.168.1.10/cb",         // http, private but not loopback
		"myapp://callback",               // custom scheme
		"javascript:alert(1)",            // script
		"https://example.com/cb#frag",    // fragment
		"https://example.com/cb#",        // empty fragment
		"https://user:pw@example.com/cb", // userinfo
		"https:///cb",                    // no host
		"https://Example.com/cb",         // upper-case host
		"https://bücher.example/cb",      // non-ASCII host
		"https://example.com/café",       // non-ASCII path
		"https://exa%6dple.com/cb",       // percent-encoded host
		"https://example.com/a b",        // space
		"https://example.com/\x7f",       // control character
		"https://example.123/cb",         // numeric final label
		"https://0x7f.1/cb",              // hexadecimal IPv4 shorthand
		"https://[127.0.0.1]/cb",         // bracketed IPv4
		"https://[::ffff:127.0.0.1]/cb",  // IPv4-mapped IPv6
		"https://[fe80::1%25en0]/cb",     // zone
		"https://-bad.example/cb",        // label starts with a hyphen
		"https://example.com/" + strings.Repeat("p", 500),
	} {
		if ValidRedirectURI(v) {
			t.Fatalf("accepted %q", v)
		}
	}
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "https://example.com/cb"
	}
	for name, uris := range map[string][]string{"none": nil, "empty": {}, "nine": nine, "one bad": {"https://example.com/cb", "http://example.com/cb"}} {
		if ValidateRedirectURIs(uris) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if ValidateRedirectURIs(nine[:8]) != nil {
		t.Fatal("eight rejected")
	}
}

// The host rule is config.ValidDNSHost itself; these are the cases that
// config's canonicalOrigin rejects as well.
func TestHostRuleMatchesConfig(t *testing.T) {
	for _, host := range []string{"example.123", "0x7f", "1.2.3.0x10", "a..b", "-a.example", "a-.example", strings.Repeat("a", 64) + ".example"} {
		if config.ValidDNSHost(host) || ValidRedirectURI("https://"+host+"/cb") {
			t.Fatalf("accepted %q", host)
		}
	}
	for _, host := range []string{"localhost", "example.com", "xn--bcher-kva.example", "a-b.example"} {
		if !config.ValidDNSHost(host) || !ValidRedirectURI("https://"+host+"/cb") {
			t.Fatalf("rejected %q", host)
		}
	}
}

func TestRedirectURIMatches(t *testing.T) {
	for _, tc := range []struct {
		registered, requested string
		want                  bool
	}{
		{"https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback", true},
		{"http://localhost/callback", "http://localhost:53682/callback", true},
		{"http://localhost:1234/callback", "http://localhost:53682/callback", true},
		{"http://127.0.0.1/callback", "http://127.0.0.1:53682/callback", true},
		{"http://[::1]/callback", "http://[::1]:53682/callback", true},
		{"http://localhost/callback?x=1", "http://localhost:5/callback?x=1", true},
		{"http://localhost/callback", "http://127.0.0.1:53682/callback", false},
		{"http://127.0.0.1/callback", "http://localhost:53682/callback", false},
		{"http://localhost/callback", "http://localhost:53682/other", false},
		{"http://localhost/callback", "http://localhost:53682/callback?x=1", false},
		{"http://localhost/callback", "http://localhost:53682/callback#x", false},
		{"http://localhost/callback", "http://user@localhost:53682/callback", false},
		{"http://localhost/callback", "https://localhost:53682/callback", false},
		// https never ignores the port, even on a loopback host.
		{"https://localhost/callback", "https://localhost:8443/callback", false},
		{"https://claude.ai/api/mcp/auth_callback", "https://claude.ai:8443/api/mcp/auth_callback", false},
		{"https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback/", false},
	} {
		if got := RedirectURIMatches(tc.registered, tc.requested); got != tc.want {
			t.Fatalf("%q vs %q: %v", tc.registered, tc.requested, got)
		}
	}
	if !MatchRedirectURI([]string{"https://a.example/cb", "http://localhost/callback"}, "http://localhost:9/callback") || MatchRedirectURI(nil, "https://a.example/cb") {
		t.Fatal("MatchRedirectURI")
	}
	if !IsLoopbackRedirect("http://127.0.0.1:9/cb") || !IsLoopbackRedirect("http://[::1]/cb") || IsLoopbackRedirect("https://claude.ai/cb") {
		t.Fatal("IsLoopbackRedirect")
	}
}

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestPKCE(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	challenge := challengeOf(verifier)
	if !ValidCodeChallenge(challenge) || !VerifyPKCE(verifier, challenge) {
		t.Fatal("valid pair rejected")
	}
	long := strings.Repeat("A-._~z9", 19)[:128]
	if !VerifyPKCE(long, challengeOf(long)) {
		t.Fatal("128-character verifier rejected")
	}
	for name, v := range map[string]string{"other verifier": strings.Repeat("w", 43), "short": strings.Repeat("v", 42), "too long": strings.Repeat("v", 129), "bad character": strings.Repeat("v", 42) + "+"} {
		if VerifyPKCE(v, challenge) {
			t.Fatalf("%s accepted", name)
		}
	}
	if VerifyPKCE(strings.Repeat("v", 42)+"+", challengeOf(strings.Repeat("v", 42)+"+")) {
		t.Fatal("verifier with + accepted")
	}
	for _, c := range []string{"", challenge[:42], challenge + "A", strings.Repeat("+", 43), verifier + "="} {
		if ValidCodeChallenge(c) {
			t.Fatalf("challenge %q accepted", c)
		}
	}
}

func TestSignedRequest(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	now := time.Unix(1_800_000_000, 0)
	r := Request{ClientID: "https://claude.ai/oauth/claude-code-client-metadata", ClientName: "Claude <&>", RedirectURI: "http://localhost:5/callback", Scope: "mcp offline_access", State: "s", CodeChallenge: challengeOf(strings.Repeat("v", 43)), Exp: now.Add(RequestLifetime).Unix()}
	signed, err := SignRequest(secret, r)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(signed, ".")[0])
	if !strings.Contains(string(raw), "Claude <&>") {
		t.Fatalf("HTML escaped: %s", raw)
	}
	if got, err := ParseRequest(secret, signed, now); err != nil || got != r {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err = ParseRequest(secret, signed, now.Add(RequestLifetime)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expired: %v", err)
	}
	if _, err = ParseRequest([]byte(strings.Repeat("x", 32)), signed, now); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("other key: %v", err)
	}
	body, sig, _ := strings.Cut(signed, ".")
	tampered := r
	tampered.RedirectURI = "https://evil.example/cb"
	other, err := SignRequest([]byte(strings.Repeat("x", 32)), tampered)
	if err != nil {
		t.Fatal(err)
	}
	otherBody, _, _ := strings.Cut(other, ".")
	for name, v := range map[string]string{"empty": "", "no dot": body, "no signature": body + ".", "no body": "." + sig, "swapped body": otherBody + "." + sig, "bad base64": body + ".***", "extra part": signed + ".x"} {
		if _, err = ParseRequest(secret, v, now); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A signed request missing a required field is still rejected; state is
	// optional.
	noState := r
	noState.State = ""
	for name, req := range map[string]Request{"client_id": {ClientName: "x", RedirectURI: "x", Scope: "mcp", CodeChallenge: "x", Exp: r.Exp}, "code_challenge": {ClientID: "x", ClientName: "x", RedirectURI: "x", Scope: "mcp", Exp: r.Exp}, "no state": noState} {
		s, err := SignRequest(secret, req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParseRequest(secret, s, now)
		if (name == "no state") != (err == nil) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestValidCIMDClientID(t *testing.T) {
	for _, v := range []string{"https://claude.ai/oauth/claude-code-client-metadata", "https://example.com:8443/client.json", "https://example.com/a/"} {
		if !ValidCIMDClientID(v) {
			t.Fatalf("rejected %q", v)
		}
	}
	for _, v := range []string{"https://example.com", "https://example.com/", "http://example.com/client", "https://user@example.com/client", "https://example.com/client#x", "https://example.com/client?x=1", "https://example.com/client?", "https://Example.com/client", "https://bücher.example/client", "https://example.com/" + strings.Repeat("a", 500), "dcr_x"} {
		if ValidCIMDClientID(v) {
			t.Fatalf("accepted %q", v)
		}
	}
}

func TestClientName(t *testing.T) {
	for _, tc := range []struct{ raw, fallback, want string }{
		{"  Claude  ", "x", "Claude"},
		{"Cl\u0000au​de ", "x", "Claude"},
		{"　\t\n", "claude.ai", "claude.ai"},
		{"‮evil", "", "evil"},
		{"", "", ""},
		{strings.Repeat("あ", 200), "", strings.Repeat("あ", 128)},
	} {
		if got := ClientName(tc.raw, tc.fallback); got != tc.want {
			t.Fatalf("%q: %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestMatchResource(t *testing.T) {
	const issuer = "https://astovia.test"
	for _, v := range []string{"https://astovia.test/mcp", "https://astovia.test/mcp/", "https://astovia.test", "https://astovia.test/"} {
		if !MatchResource(issuer, v) {
			t.Fatalf("rejected %q", v)
		}
	}
	for _, v := range []string{"https://astovia.test/mcp//", "https://astovia.test//", "https://astovia.test/api", "https://other.test/mcp", "http://astovia.test/mcp", "https://astovia.test:443/mcp"} {
		if MatchResource(issuer, v) {
			t.Fatalf("accepted %q", v)
		}
	}
}

func TestBuildRedirectURL(t *testing.T) {
	got, err := BuildRedirectURL("https://example.com/cb?keep=1", map[string]string{"code": "c", "state": "", "iss": "https://astovia.test"})
	if err != nil || got != "https://example.com/cb?code=c&iss=https%3A%2F%2Fastovia.test&keep=1" {
		t.Fatalf("%q %v", got, err)
	}
}
