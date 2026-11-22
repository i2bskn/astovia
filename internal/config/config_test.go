package config

import (
	"encoding/base64"
	"reflect"
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

// setOIDC sets the sign-in variables, clearing the Cloudflare Access ones of
// earlier releases.
func setOIDC(t *testing.T, mode, issuer, id, secret string) {
	t.Helper()
	t.Setenv("ASTOVIA_ACCESS_TEAM_DOMAIN", "")
	t.Setenv("ASTOVIA_ACCESS_CLIENT_ID", "")
	t.Setenv("ASTOVIA_ACCESS_CLIENT_SECRET", "")
	t.Setenv("ASTOVIA_AUTH_MODE", mode)
	t.Setenv("ASTOVIA_OIDC_ISSUER", issuer)
	t.Setenv("ASTOVIA_OIDC_CLIENT_ID", id)
	t.Setenv("ASTOVIA_OIDC_CLIENT_SECRET", secret)
}

func TestOIDCClient(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://Astovia.Example.com:443")
	t.Setenv("ASTOVIA_JWT_SECRET", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("ASTOVIA_ALLOWED_EMAILS", "")
	t.Setenv("ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED", "")
	t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", "")
	const secret = "s3cr3t:with+special/chars%~"
	const issuer, id = "https://accounts.example.com", "client/id+1"

	// Unset, empty or password: password sign-in.
	// ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED and ASTOVIA_OIDC_REQUIRE_GOOGLE_HD
	// are ignored, even when invalid.
	for _, mode := range []string{"", "password", " Password "} {
		setOIDC(t, mode, "", "", "")
		t.Setenv("ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED", "1")
		t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", "1")
		t.Setenv("ASTOVIA_PUBLIC_URL", "http://localhost:8080")
		if c, e := Load(true); e != nil || c.OIDC != nil {
			t.Fatalf("%q: %+v %v", mode, c.OIDC, e)
		}
	}
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://Astovia.Example.com:443")
	t.Setenv("ASTOVIA_ALLOWED_EMAILS", "")
	t.Setenv("ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED", "")
	t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", "")

	// ASTOVIA_OIDC_* without ASTOVIA_AUTH_MODE=oidc.
	for _, tc := range []struct{ mode, issuer, id, secret string }{
		{"", issuer, "", ""}, {"", "", id, ""}, {"", "", "", secret}, {"password", issuer, id, secret},
	} {
		setOIDC(t, tc.mode, tc.issuer, tc.id, tc.secret)
		for _, requireSecret := range []bool{true, false} {
			if _, e := Load(requireSecret); e == nil || !strings.Contains(e.Error(), "ASTOVIA_AUTH_MODE is not oidc") {
				t.Fatalf("%+v: %v", tc, e)
			}
		}
	}

	// An unknown mode.
	for _, mode := range []string{"cloudflare_access", "oauth", "OIDC2"} {
		setOIDC(t, mode, issuer, id, secret)
		if _, e := Load(false); e == nil || e.Error() != "ASTOVIA_AUTH_MODE must be password or oidc" {
			t.Fatalf("%q: %v", mode, e)
		}
	}

	// All set; the CLI reads them too. The issuer is kept as it is.
	for _, requireSecret := range []bool{true, false} {
		setOIDC(t, " OIDC ", issuer+"/", id, secret)
		c, e := Load(requireSecret)
		want := OIDCClient{
			Issuer:               issuer + "/",
			ClientID:             id,
			Secret:               secret,
			RedirectURI:          "https://astovia.example.com/auth/oidc/callback",
			RequireEmailVerified: true,
			RequireGoogleHD:      false,
		}
		if e != nil || c.OIDC == nil || !reflect.DeepEqual(*c.OIDC, want) {
			t.Fatalf("%v: %+v %v", requireSecret, c.OIDC, e)
		}
	}
	// Ports, IP literals and paths are allowed in the issuer.
	for _, v := range []string{"https://keycloak.internal:8443/realms/astovia", "https://10.0.0.1/realms/x/", "https://[2001:db8::1]:8443", "https://example.cloudflareaccess.com/cdn-cgi/access/sso/oidc/abc"} {
		setOIDC(t, "oidc", v, id, secret)
		if c, e := Load(false); e != nil || c.OIDC == nil || c.OIDC.Issuer != v {
			t.Fatalf("%s: %+v %v", v, c.OIDC, e)
		}
	}
	setOIDC(t, "oidc", issuer, strings.Repeat("~", 256), strings.Repeat("~", 1024))
	if c, e := Load(false); e != nil || c.OIDC == nil {
		t.Fatal(c.OIDC, e)
	}

	// ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED.
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", true}, {"true", true}, {" true ", true}, {"false", false}, {" false", false}} {
		setOIDC(t, "oidc", issuer, id, secret)
		t.Setenv("ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED", tc.value)
		if c, e := Load(false); e != nil || c.OIDC == nil || c.OIDC.RequireEmailVerified != tc.want {
			t.Fatalf("%q: %+v %v", tc.value, c.OIDC, e)
		}
	}
	for _, v := range []string{"1", "0", "TRUE", "False", "yes"} {
		t.Setenv("ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED", v)
		if _, e := Load(false); e == nil || !strings.Contains(e.Error(), "ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED") {
			t.Fatalf("%q: %v", v, e)
		}
	}
	t.Setenv("ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED", "")

	// ASTOVIA_OIDC_REQUIRE_GOOGLE_HD: empty leaves the check to the issuer.
	// The server and the CLI read it alike.
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", false}, {"true", true}, {" true ", true}, {"false", false}, {" false", false}} {
		setOIDC(t, "oidc", issuer, id, secret)
		t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", tc.value)
		for _, requireSecret := range []bool{true, false} {
			if c, e := Load(requireSecret); e != nil || c.OIDC == nil || c.OIDC.RequireGoogleHD != tc.want {
				t.Fatalf("%q %v: %+v %v", tc.value, requireSecret, c.OIDC, e)
			}
		}
	}
	// Any other value is an error that does not echo it, with Google as the
	// issuer too.
	const invalidGoogleHD = "ASTOVIA_OIDC_REQUIRE_GOOGLE_HD must be true or false"
	for _, iss := range []string{issuer, GoogleIssuer} {
		setOIDC(t, "oidc", iss, id, secret)
		for _, v := range []string{"1", "0", "TRUE", "False", "yes"} {
			t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", v)
			for _, requireSecret := range []bool{true, false} {
				if _, e := Load(requireSecret); e == nil || e.Error() != invalidGoogleHD {
					t.Fatalf("%s %q %v: %v", iss, v, requireSecret, e)
				}
			}
		}
	}
	// With Google as the issuer the check cannot be turned off: false is an
	// error, and empty and true are accepted.
	setOIDC(t, "oidc", GoogleIssuer, id, secret)
	for _, v := range []string{"false", " false "} {
		t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", v)
		for _, requireSecret := range []bool{true, false} {
			if _, e := Load(requireSecret); e == nil || e.Error() != "ASTOVIA_OIDC_REQUIRE_GOOGLE_HD cannot be false with the issuer https://accounts.google.com: the hd check of Google accounts cannot be turned off" {
				t.Fatalf("%q %v: %v", v, requireSecret, e)
			}
		}
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", false}, {"true", true}} {
		t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", tc.value)
		if c, e := Load(false); e != nil || c.OIDC == nil || c.OIDC.Issuer != GoogleIssuer || c.OIDC.RequireGoogleHD != tc.want {
			t.Fatalf("%q: %+v %v", tc.value, c.OIDC, e)
		}
	}
	// The issuer is compared exactly: with a trailing slash it is not Google.
	setOIDC(t, "oidc", GoogleIssuer+"/", id, secret)
	t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", "false")
	if c, e := Load(false); e != nil || c.OIDC == nil || c.OIDC.RequireGoogleHD {
		t.Fatalf("%+v %v", c.OIDC, e)
	}
	t.Setenv("ASTOVIA_OIDC_REQUIRE_GOOGLE_HD", "")

	// Missing values and format violations, never echoed in the error.
	for _, tc := range []struct{ issuer, id, secret, variable string }{
		{"", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{issuer, "", secret, "ASTOVIA_OIDC_CLIENT_ID"},
		{issuer, id, "", "ASTOVIA_OIDC_CLIENT_SECRET"},
		{"http://accounts.example.com", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"accounts.example.com", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://:443", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https:accounts.example.com", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://user:pass@accounts.example.com", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com?x=1", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com?", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com#x", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com#", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com//", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com/realm//", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{" https://accounts.example.com", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com ", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com/a b", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com/\x01", id, secret, "ASTOVIA_OIDC_ISSUER"},
		{"https://accounts.example.com/" + strings.Repeat("a", 2048), id, secret, "ASTOVIA_OIDC_ISSUER"},
		{issuer, "client id", secret, "ASTOVIA_OIDC_CLIENT_ID"},
		{issuer, " client-id", secret, "ASTOVIA_OIDC_CLIENT_ID"},
		{issuer, "clïent", secret, "ASTOVIA_OIDC_CLIENT_ID"},
		{issuer, strings.Repeat("a", 257), secret, "ASTOVIA_OIDC_CLIENT_ID"},
		{issuer, id, "with space", "ASTOVIA_OIDC_CLIENT_SECRET"},
		{issuer, id, " " + secret, "ASTOVIA_OIDC_CLIENT_SECRET"},
		{issuer, id, secret + "\n", "ASTOVIA_OIDC_CLIENT_SECRET"},
		{issuer, id, "sécret", "ASTOVIA_OIDC_CLIENT_SECRET"},
		{issuer, id, strings.Repeat("a", 1025), "ASTOVIA_OIDC_CLIENT_SECRET"},
	} {
		setOIDC(t, "oidc", tc.issuer, tc.id, tc.secret)
		_, e := Load(false)
		if e == nil || !strings.Contains(e.Error(), tc.variable) {
			t.Fatalf("%+v: %v", tc, e)
		}
		if tc.secret != "" && strings.Contains(e.Error(), strings.TrimSpace(tc.secret)) {
			t.Fatalf("error contains the secret: %v", e)
		}
	}

	// OIDC needs an https public URL.
	setOIDC(t, "oidc", issuer, id, secret)
	t.Setenv("ASTOVIA_PUBLIC_URL", "http://localhost:8080")
	if _, e := Load(false); e == nil || e.Error() != "ASTOVIA_PUBLIC_URL must use https with ASTOVIA_AUTH_MODE=oidc" {
		t.Fatal(e)
	}
}

// The Cloudflare Access variables of earlier releases stop the server and
// the CLI, whatever the mode, instead of falling back to passwords.
func TestLegacyAccessVariables(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://astovia.example.com")
	const secret = "legacy-secret-value"
	for _, mode := range []string{"", "oidc"} {
		for _, name := range []string{"ASTOVIA_ACCESS_TEAM_DOMAIN", "ASTOVIA_ACCESS_CLIENT_ID", "ASTOVIA_ACCESS_CLIENT_SECRET"} {
			setOIDC(t, mode, "", "", "")
			if mode == "oidc" {
				setOIDC(t, mode, "https://accounts.example.com", "id", "secret")
			}
			t.Setenv(name, secret)
			for _, requireSecret := range []bool{true, false} {
				_, e := Load(requireSecret)
				if e == nil || e.Error() != legacyAccessError {
					t.Fatalf("%s %q: %v", name, mode, e)
				}
				if strings.Contains(e.Error(), secret) {
					t.Fatalf("error contains the value: %v", e)
				}
			}
		}
	}
	// Access can send email_verified: turning the check off is the last
	// resort, not a step of the migration.
	for _, part := range []string{"send the email_verified claim", "only if that is not possible, set ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED=false"} {
		if !strings.Contains(legacyAccessError, part) {
			t.Fatalf("%q is missing: %s", part, legacyAccessError)
		}
	}
}

// ASTOVIA_ALLOWED_EMAILS of earlier releases stops the server, whatever the
// mode, and tells the operator what to do first. The users commands ignore
// it, so that users can be disabled before it is removed.
func TestAllowedEmailsVariable(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://astovia.example.com")
	t.Setenv("ASTOVIA_JWT_SECRET", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	const value = "mallory@example.org"
	for _, mode := range []string{"", "password", "oidc"} {
		setOIDC(t, mode, "", "", "")
		if mode == "oidc" {
			setOIDC(t, mode, "https://accounts.example.com", "id", "secret")
		}
		for _, v := range []string{value, "*", " example.org , " + value, "x"} {
			t.Setenv("ASTOVIA_ALLOWED_EMAILS", v)
			_, e := Load(true)
			if e == nil || e.Error() != allowedEmailsError {
				t.Fatalf("%q %q: %v", mode, v, e)
			}
			if strings.Contains(e.Error(), value) {
				t.Fatalf("error contains the value: %v", e)
			}
			// The users commands load the configuration without the secret.
			if c, e := Load(false); e != nil || (c.OIDC != nil) != (mode == "oidc") {
				t.Fatalf("%q %q for the CLI: %+v %v", mode, v, c.OIDC, e)
			}
		}
		// Empty or blank is not set.
		for _, v := range []string{"", " ", " \t "} {
			t.Setenv("ASTOVIA_ALLOWED_EMAILS", v)
			for _, requireSecret := range []bool{true, false} {
				if c, e := Load(requireSecret); e != nil || (c.OIDC != nil) != (mode == "oidc") {
					t.Fatalf("%q %q: %+v %v", mode, v, c.OIDC, e)
				}
			}
		}
	}
	// The steps are in the order the operator takes them.
	last := -1
	for _, step := range []string{"astovia users disable --email <address>", "remove ASTOVIA_ALLOWED_EMAILS", "astovia users create --email <address> --no-password", "invite"} {
		i := strings.Index(allowedEmailsError, step)
		if i <= last {
			t.Fatalf("%q is missing or out of order: %s", step, allowedEmailsError)
		}
		last = i
	}

	// The Cloudflare Access variables are reported first.
	setOIDC(t, "", "", "", "")
	t.Setenv("ASTOVIA_ALLOWED_EMAILS", value)
	t.Setenv("ASTOVIA_ACCESS_CLIENT_ID", "legacy")
	if _, e := Load(true); e == nil || e.Error() != legacyAccessError {
		t.Fatal(e)
	}
	// It comes before the other checks of the sign-in mode.
	setOIDC(t, "unknown", "", "", "")
	if _, e := Load(true); e == nil || e.Error() != allowedEmailsError {
		t.Fatal(e)
	}
	if strings.Contains(legacyAccessError, "ASTOVIA_ALLOWED_EMAILS") || !strings.Contains(legacyAccessError, "invitation") {
		t.Fatal(legacyAccessError)
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

func TestMailConfig(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://example.com")
	vars := []string{"ASTOVIA_MAIL_PROVIDER", "ASTOVIA_MAIL_API_KEY", "ASTOVIA_MAIL_FROM", "ASTOVIA_MAIL_FROM_NAME", "ASTOVIA_CLOUDFLARE_ACCOUNT_ID"}
	set := func(values map[string]string) {
		for _, k := range vars {
			t.Setenv(k, values[k])
		}
	}
	account := "0123456789abcdef0123456789abcdef"
	cloudflare := map[string]string{"ASTOVIA_MAIL_PROVIDER": "Cloudflare", "ASTOVIA_MAIL_API_KEY": " cf-token ", "ASTOVIA_MAIL_FROM": "notify@example.com", "ASTOVIA_CLOUDFLARE_ACCOUNT_ID": account}
	resend := map[string]string{"ASTOVIA_MAIL_PROVIDER": "resend", "ASTOVIA_MAIL_API_KEY": "re_key", "ASTOVIA_MAIL_FROM": "notify@example.com", "ASTOVIA_MAIL_FROM_NAME": "Astovia 通知"}
	with := func(base map[string]string, k, v string) map[string]string {
		out := map[string]string{}
		for key, value := range base {
			out[key] = value
		}
		out[k] = v
		return out
	}

	set(nil)
	if c, err := Load(false); err != nil || c.Mail != nil {
		t.Fatalf("unset: %+v %v", c.Mail, err)
	}
	set(cloudflare)
	if c, err := Load(false); err != nil || *c.Mail != (MailConfig{Provider: "cloudflare", APIKey: "cf-token", AccountID: account, From: "notify@example.com"}) {
		t.Fatalf("cloudflare: %+v %v", c.Mail, err)
	}
	set(resend)
	if c, err := Load(false); err != nil || *c.Mail != (MailConfig{Provider: "resend", APIKey: "re_key", From: "notify@example.com", FromName: "Astovia 通知"}) {
		t.Fatalf("resend: %+v %v", c.Mail, err)
	}
	for name, values := range map[string]map[string]string{
		"unknown provider":         with(resend, "ASTOVIA_MAIL_PROVIDER", "sendgrid"),
		"no key":                   with(resend, "ASTOVIA_MAIL_API_KEY", ""),
		"key not ASCII":            with(resend, "ASTOVIA_MAIL_API_KEY", "キー"),
		"long key":                 with(resend, "ASTOVIA_MAIL_API_KEY", strings.Repeat("k", 513)),
		"no from":                  with(resend, "ASTOVIA_MAIL_FROM", ""),
		"from with a name":         with(resend, "ASTOVIA_MAIL_FROM", "Astovia <notify@example.com>"),
		"invalid from":             with(resend, "ASTOVIA_MAIL_FROM", "notify"),
		"from name with <":         with(resend, "ASTOVIA_MAIL_FROM_NAME", "a<b"),
		"from name with a quote":   with(resend, "ASTOVIA_MAIL_FROM_NAME", `a"b`),
		"from name with a control": with(resend, "ASTOVIA_MAIL_FROM_NAME", "a\x01b"),
		"long from name":           with(resend, "ASTOVIA_MAIL_FROM_NAME", strings.Repeat("あ", 101)),
		"cloudflare no account":    with(cloudflare, "ASTOVIA_CLOUDFLARE_ACCOUNT_ID", ""),
		"cloudflare bad account":   with(cloudflare, "ASTOVIA_CLOUDFLARE_ACCOUNT_ID", strings.ToUpper(account)),
		"resend with account":      with(resend, "ASTOVIA_CLOUDFLARE_ACCOUNT_ID", account),
		"no provider with key":     {"ASTOVIA_MAIL_API_KEY": "re_key"},
		"no provider with from":    {"ASTOVIA_MAIL_FROM": "notify@example.com"},
		"no provider with account": {"ASTOVIA_CLOUDFLARE_ACCOUNT_ID": account},
	} {
		set(values)
		_, err := Load(false)
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if key := values["ASTOVIA_MAIL_API_KEY"]; key != "" && strings.Contains(err.Error(), key) {
			t.Fatalf("%s: key in error: %v", name, err)
		}
	}
}

func TestMFARequired(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://example.com")
	t.Setenv("ASTOVIA_JWT_SECRET", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("ASTOVIA_OIDC_REQUIRE_EMAIL_VERIFIED", "")
	const invalid = "ASTOVIA_MFA_REQUIRED must be off, owners or all"
	for _, serve := range []bool{true, false} {
		for _, mode := range []string{"password", "oidc"} {
			if mode == "oidc" {
				setOIDC(t, "oidc", "https://idp.example.com", "client", "secret")
			} else {
				setOIDC(t, "", "", "", "")
			}
			for input, want := range map[string]string{
				"": MFAOff, "off": MFAOff, "OFF": MFAOff, "  off ": MFAOff, " \t": MFAOff,
				"owners": MFAOwners, "Owners": MFAOwners, " owners\n": MFAOwners,
				"all": MFAAll, "ALL": MFAAll, "\tall ": MFAAll,
			} {
				t.Setenv("ASTOVIA_MFA_REQUIRED", input)
				c, err := Load(serve)
				if err != nil || c.MFARequired != want {
					t.Fatalf("serve=%v mode=%s %q: %q %v", serve, mode, input, c.MFARequired, err)
				}
			}
			for _, input := range []string{"owner", "on", "true", "1", "required", "owners,all", "o w n e r s", "全員"} {
				t.Setenv("ASTOVIA_MFA_REQUIRED", input)
				if _, err := Load(serve); err == nil || err.Error() != invalid {
					t.Fatalf("serve=%v mode=%s %q: %v", serve, mode, input, err)
				}
			}
		}
	}
	if MFAOff != "off" || MFAOwners != "owners" || MFAAll != "all" {
		t.Fatalf("values: %q %q %q", MFAOff, MFAOwners, MFAAll)
	}
}
