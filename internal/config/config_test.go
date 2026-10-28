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
