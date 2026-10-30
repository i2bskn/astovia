package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "http://localhost:8080")
	t.Setenv("ASTOVIA_JWT_SECRET", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	c, e := Load(true)
	if e != nil || c.DSN != "file:./data/astovia.db" {
		t.Fatal(c, e)
	}
	for _, v := range []string{"https://host/path", "http://example.com", "https://user:pass@host", "https://host?x=1", "https://host#x", "not-a-url"} {
		t.Setenv("ASTOVIA_PUBLIC_URL", v)
		if _, e = Load(true); e == nil {
			t.Fatalf("accepted %s", v)
		}
	}
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://example.com")
	t.Setenv("ASTOVIA_JWT_SECRET", "short")
	if _, e = Load(true); e == nil {
		t.Fatal("short secret accepted")
	}
	if c, e = Load(false); e != nil || !c.Secure {
		t.Fatal(c, e)
	}
	t.Setenv("ASTOVIA_DB_DRIVER", "postgres")
	if _, e = Load(false); e == nil {
		t.Fatal("missing postgres DSN accepted")
	}
}

func TestPublicURLCanonicalOrigin(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	for _, tc := range []struct {
		input, want string
		secure      bool
	}{
		{"https://EXAMPLE.com:443", "https://example.com", true},
		{"http://LOCALHOST:080", "http://localhost", false},
		{"https://example.com:08443", "https://example.com:8443", true},
		{"http://[::1]:80", "http://[::1]", false},
		{"http://[0:0:0:0:0:0:0:1]:080", "http://[::1]", false},
		{"https://[2001:0DB8:0:0:0:0:0:1]:08443", "https://[2001:db8::1]:8443", true},
		{"http://127.0.0.1:80", "http://127.0.0.1", false},
		{"http://localhost:8080", "http://localhost:8080", false},
		{" https://XN--BCHER-KVA.example:443 ", "https://xn--bcher-kva.example", true},
		{"https://EXAMPLE.com.", "https://example.com.", true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			t.Setenv("ASTOVIA_PUBLIC_URL", tc.input)
			c, err := Load(false)
			if err != nil || c.PublicURL != tc.want || c.Secure != tc.secure {
				t.Fatalf("got origin=%q secure=%v err=%v; want %q secure=%v", c.PublicURL, c.Secure, err, tc.want, tc.secure)
			}
		})
	}
}

func TestPublicURLRejectsUnsupportedOrigins(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	for _, input := range []string{
		"https://example.com?", "https://example.com#", "https://example.com/", "https://example.com/path",
		"https://example.com?x=1", "https://example.com#fragment", "https://user:secret@example.com",
		"https:example.com", "https://", "ftp://example.com", "http://example.com",
		"https://example.com:", "https://example.com:0", "https://example.com:65536", "https://example.com:abc",
		"https://example.com:+443", "https://example.com:9999999999999999999999",
		"https://127.1", "https://2130706433", "https://0x7f000001", "https://0177.0.0.1", "https://127.0.0.01",
		"https://example.123", "https://example.0xff", "https://127.0.0.1.",
		"https://bücher.example", "https://under_score.example", "https://-bad.example", "https://bad-.example", "https://bad..example",
		"https://[::ffff:127.0.0.1]", "https://[::ffff:7f00:1]", "https://[fe80::1%25eth0]", "https://[127.0.0.1]", "https://::1",
	} {
		t.Run(input, func(t *testing.T) {
			t.Setenv("ASTOVIA_PUBLIC_URL", input)
			if _, err := Load(false); err == nil {
				t.Fatalf("accepted unsupported origin %q", input)
			}
		})
	}
}

func TestGoogleClient(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://example.com")
	t.Setenv("ASTOVIA_JWT_SECRET", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("ASTOVIA_GOOGLE_CLIENT_ID", "")
	t.Setenv("ASTOVIA_GOOGLE_CLIENT_SECRET", "")
	if c, e := Load(false); e != nil || c.Google != nil {
		t.Fatal(c.Google, e)
	}
	t.Setenv("ASTOVIA_GOOGLE_CLIENT_ID", "id.apps.googleusercontent.com")
	t.Setenv("ASTOVIA_GOOGLE_CLIENT_SECRET", "secret")
	c, e := Load(false)
	if e != nil || c.Google == nil || c.Google.ID != "id.apps.googleusercontent.com" || c.Google.Secret != "secret" || c.Google.RedirectURI != "https://example.com/integrations/google/callback" {
		t.Fatal(c.Google, e)
	}
	for _, tc := range []struct{ id, secret string }{{"id", ""}, {"", "secret"}, {" id", "secret"}, {"id", "secret "}, {"id\n", "secret"}, {"id", "sécret"}, {strings.Repeat("a", 513), "secret"}} {
		t.Setenv("ASTOVIA_GOOGLE_CLIENT_ID", tc.id)
		t.Setenv("ASTOVIA_GOOGLE_CLIENT_SECRET", tc.secret)
		if _, e := Load(false); e == nil {
			t.Fatalf("accepted %q %q", tc.id, tc.secret)
		} else if tc.secret != "" && strings.Contains(e.Error(), tc.secret) {
			t.Fatalf("error contains the secret: %v", e)
		}
	}
}

func TestAccessClient(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://Astovia.Example.com:443")
	t.Setenv("ASTOVIA_JWT_SECRET", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	const secret = "s3cr3t:with+special/chars%~"
	set := func(team, id, sec string) {
		t.Setenv("ASTOVIA_ACCESS_TEAM_DOMAIN", team)
		t.Setenv("ASTOVIA_ACCESS_CLIENT_ID", id)
		t.Setenv("ASTOVIA_ACCESS_CLIENT_SECRET", sec)
	}

	// None set: password sign-in.
	set("", "", "")
	if c, e := Load(true); e != nil || c.Access != nil {
		t.Fatal(c.Access, e)
	}

	// All three set, normalized; the CLI reads them too.
	for _, requireSecret := range []bool{true, false} {
		set("https://Example.CloudflareAccess.com/", "0123abc._~-", secret)
		c, e := Load(requireSecret)
		want := AccessClient{
			TeamDomain:  "https://example.cloudflareaccess.com",
			ClientID:    "0123abc._~-",
			Secret:      secret,
			Issuer:      "https://example.cloudflareaccess.com/cdn-cgi/access/sso/oidc/0123abc._~-",
			RedirectURI: "https://astovia.example.com/auth/access/callback",
		}
		if e != nil || c.Access == nil || *c.Access != want {
			t.Fatalf("%v: %+v %v", requireSecret, c.Access, e)
		}
	}
	set("https://example.cloudflareaccess.com", "id", strings.Repeat("~", 1024))
	if c, e := Load(false); e != nil || c.Access == nil || c.Access.TeamDomain != "https://example.cloudflareaccess.com" {
		t.Fatal(c.Access, e)
	}

	// One or two of them set.
	const team, id = "https://example.cloudflareaccess.com", "client-id"
	for _, tc := range []struct{ team, id, secret string }{
		{team, "", ""}, {"", id, ""}, {"", "", secret},
		{team, id, ""}, {team, "", secret}, {"", id, secret},
	} {
		set(tc.team, tc.id, tc.secret)
		for _, requireSecret := range []bool{true, false} {
			if _, e := Load(requireSecret); e == nil || !strings.Contains(e.Error(), "must be set together") {
				t.Fatalf("%+v: %v", tc, e)
			}
		}
	}

	// Format violations, never echoed in the error.
	for _, tc := range []struct{ team, id, secret, variable string }{
		{"http://example.cloudflareaccess.com", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com/path", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com//", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com?x=1", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com?", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com#x", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com:443", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com:", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://user:pass@example.cloudflareaccess.com", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com.", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://cloudflareaccess.com", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com.evil.test", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://evil.test", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://ex_ample.cloudflareaccess.com", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"example.cloudflareaccess.com", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{" https://example.cloudflareaccess.com", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{"https://example.cloudflareaccess.com ", id, secret, "ASTOVIA_ACCESS_TEAM_DOMAIN"},
		{team, "client id", secret, "ASTOVIA_ACCESS_CLIENT_ID"},
		{team, "client/id", secret, "ASTOVIA_ACCESS_CLIENT_ID"},
		{team, " client-id", secret, "ASTOVIA_ACCESS_CLIENT_ID"},
		{team, "clïent", secret, "ASTOVIA_ACCESS_CLIENT_ID"},
		{team, strings.Repeat("a", 257), secret, "ASTOVIA_ACCESS_CLIENT_ID"},
		{team, id, "with space", "ASTOVIA_ACCESS_CLIENT_SECRET"},
		{team, id, " " + secret, "ASTOVIA_ACCESS_CLIENT_SECRET"},
		{team, id, secret + "\n", "ASTOVIA_ACCESS_CLIENT_SECRET"},
		{team, id, "sécret", "ASTOVIA_ACCESS_CLIENT_SECRET"},
		{team, id, strings.Repeat("a", 1025), "ASTOVIA_ACCESS_CLIENT_SECRET"},
	} {
		set(tc.team, tc.id, tc.secret)
		_, e := Load(false)
		if e == nil || !strings.Contains(e.Error(), tc.variable) {
			t.Fatalf("%+v: %v", tc, e)
		}
		if strings.Contains(e.Error(), strings.TrimSpace(tc.secret)) {
			t.Fatalf("error contains the secret: %v", e)
		}
	}

	// Access needs an https public URL.
	set(team, id, secret)
	t.Setenv("ASTOVIA_PUBLIC_URL", "http://localhost:8080")
	if _, e := Load(false); e == nil || !strings.Contains(e.Error(), "ASTOVIA_PUBLIC_URL") {
		t.Fatal(e)
	}
}

func TestAIAllowPrivateEndpoints(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "http://localhost:8080")
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"false", false},
		{"False", false},
		{"0", false},
	} {
		t.Setenv("ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS", tc.value)
		c, e := Load(false)
		if e != nil || c.AIAllowPrivateEndpoints != tc.want {
			t.Fatalf("%q: got %v, %v", tc.value, c.AIAllowPrivateEndpoints, e)
		}
	}
	for _, v := range []string{"yes", "on", "2", "truthy"} {
		t.Setenv("ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS", v)
		if _, e := Load(false); e == nil || !strings.Contains(e.Error(), "ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS") {
			t.Fatalf("%q: %v", v, e)
		}
	}
}
