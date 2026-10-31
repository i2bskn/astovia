package config

import (
	"encoding/base64"
	"errors"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type Config struct {
	Addr, PublicURL, Driver, DSN string
	Secret                       []byte
	Secure                       bool
	// Google is the OAuth client of the mail integration, or nil when
	// ASTOVIA_GOOGLE_CLIENT_ID and ASTOVIA_GOOGLE_CLIENT_SECRET are not set.
	Google *GoogleClient
	// Access is the Cloudflare Access SaaS application (OIDC) that signs
	// users in to the web UI instead of passwords, or nil when the
	// ASTOVIA_ACCESS_* variables are not set (password sign-in).
	Access *AccessClient
	// AIAllowPrivateEndpoints lets OpenAI-compatible AI endpoints use http
	// and non-public addresses (ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS).
	AIAllowPrivateEndpoints bool
	// Mail is the service that sends notification mail, or nil when
	// ASTOVIA_MAIL_PROVIDER is not set (no e-mail notifications).
	Mail *MailConfig
}

// MailConfig is the service that sends notification mail, chosen by the
// operator. The API key is never logged or put in an error.
type MailConfig struct {
	Provider  string // "cloudflare" or "resend"
	APIKey    string
	AccountID string // Cloudflare only
	From      string // bare address
	FromName  string // optional; used by Resend only
}

// Mail providers of ASTOVIA_MAIL_PROVIDER.
const (
	MailCloudflare = "cloudflare"
	MailResend     = "resend"
)

// cloudflareAccountIDPattern is the format of a Cloudflare account ID.
var cloudflareAccountIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// AccessClient identifies the Cloudflare Access SaaS application (OIDC)
// whose relying party Astovia is. Issuer and RedirectURI are derived from
// the team domain, the client ID and the public URL. The secret is never
// logged or put in an error.
type AccessClient struct {
	// TeamDomain is the team origin, https://<team>.cloudflareaccess.com.
	TeamDomain string
	// ClientID and Secret are the OIDC client ID and secret.
	ClientID, Secret string
	// Issuer is the OIDC issuer of the application; the authorization,
	// token and JWKS endpoints are under it (no discovery).
	Issuer string
	// RedirectURI is the callback registered in the application.
	RedirectURI string
}

// AccessCallbackPath is the path of the redirect URI of the Access
// application.
const AccessCallbackPath = "/auth/access/callback"

// accessClientIDPattern is the accepted format of an Access client ID. The
// ID is embedded in URL paths, so only unreserved characters are allowed.
var accessClientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,256}$`)

// GoogleClient is the OAuth client (a web application) the operator made in
// Google Cloud for the mail integration. RedirectURI is derived from the
// public URL. The secret is never logged or put in an error.
type GoogleClient struct {
	ID, Secret, RedirectURI string
}

// GoogleCallbackPath is the path of the redirect URI of the Google OAuth
// client, a screen of the web UI.
const GoogleCallbackPath = "/integrations/google/callback"

func Load(requireSecret bool) (Config, error) {
	c := Config{Addr: env("ASTOVIA_ADDR", "127.0.0.1:8080"), PublicURL: env("ASTOVIA_PUBLIC_URL", "http://localhost:8080"), Driver: env("ASTOVIA_DB_DRIVER", "sqlite"), DSN: os.Getenv("ASTOVIA_DB_DSN")}
	if c.Driver != "sqlite" && c.Driver != "postgres" {
		return c, errors.New("ASTOVIA_DB_DRIVER must be sqlite or postgres")
	}
	if c.DSN == "" {
		if c.Driver == "postgres" {
			return c, errors.New("ASTOVIA_DB_DSN is required for postgres")
		}
		c.DSN = "file:./data/astovia.db"
	}
	var err error
	c.PublicURL, c.Secure, err = canonicalOrigin(c.PublicURL)
	if err != nil {
		return c, err
	}
	if c.Google, err = googleClient(c.PublicURL); err != nil {
		return c, err
	}
	if c.Access, err = accessClient(c.PublicURL); err != nil {
		return c, err
	}
	if c.AIAllowPrivateEndpoints, err = boolEnv("ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS"); err != nil {
		return c, err
	}
	if c.Mail, err = mailConfig(); err != nil {
		return c, err
	}
	if requireSecret {
		c.Secret, err = base64.StdEncoding.DecodeString(os.Getenv("ASTOVIA_JWT_SECRET"))
		if err != nil || len(c.Secret) < 32 {
			return c, errors.New("ASTOVIA_JWT_SECRET must be padded base64 encoding at least 32 random bytes")
		}
	}
	return c, nil
}

// googleClient reads the OAuth client of the mail integration. Both values
// empty disable it; one of them alone is an error. The values are not
// trimmed: surrounding spaces are rejected, so that a copy and paste mistake
// does not pass silently.
func googleClient(publicURL string) (*GoogleClient, error) {
	id, secret := os.Getenv("ASTOVIA_GOOGLE_CLIENT_ID"), os.Getenv("ASTOVIA_GOOGLE_CLIENT_SECRET")
	if id == "" && secret == "" {
		return nil, nil
	}
	if id == "" || secret == "" {
		return nil, errors.New("ASTOVIA_GOOGLE_CLIENT_ID and ASTOVIA_GOOGLE_CLIENT_SECRET must be set together")
	}
	if !validGoogleValue(id) {
		return nil, errors.New("ASTOVIA_GOOGLE_CLIENT_ID must be 1 to 512 printable ASCII characters without surrounding spaces")
	}
	if !validGoogleValue(secret) {
		return nil, errors.New("ASTOVIA_GOOGLE_CLIENT_SECRET must be 1 to 512 printable ASCII characters without surrounding spaces")
	}
	return &GoogleClient{ID: id, Secret: secret, RedirectURI: publicURL + GoogleCallbackPath}, nil
}

// validGoogleValue accepts 1-512 printable ASCII characters that do not
// start or end with a space.
func validGoogleValue(v string) bool {
	if len(v) < 1 || len(v) > 512 || v[0] == ' ' || v[len(v)-1] == ' ' {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// accessClient reads the Cloudflare Access application. All three values
// empty keep password sign-in; only some of them is an error. As with the
// Google client, the values are not trimmed: surrounding spaces are
// rejected.
func accessClient(publicURL string) (*AccessClient, error) {
	team, id, secret := os.Getenv("ASTOVIA_ACCESS_TEAM_DOMAIN"), os.Getenv("ASTOVIA_ACCESS_CLIENT_ID"), os.Getenv("ASTOVIA_ACCESS_CLIENT_SECRET")
	if team == "" && id == "" && secret == "" {
		return nil, nil
	}
	if team == "" || id == "" || secret == "" {
		return nil, errors.New("ASTOVIA_ACCESS_TEAM_DOMAIN, ASTOVIA_ACCESS_CLIENT_ID and ASTOVIA_ACCESS_CLIENT_SECRET must be set together")
	}
	domain, err := accessTeamDomain(team)
	if err != nil {
		return nil, err
	}
	if !accessClientIDPattern.MatchString(id) {
		return nil, errors.New("ASTOVIA_ACCESS_CLIENT_ID must be 1-256 characters of A-Z, a-z, 0-9, '.', '_', '~' and '-'")
	}
	if !validAccessSecret(secret) {
		return nil, errors.New("ASTOVIA_ACCESS_CLIENT_SECRET must be 1-1024 printable ASCII characters without spaces")
	}
	// The sign-in cookie uses the Secure attribute and the __Host- prefix.
	if !strings.HasPrefix(publicURL, "https://") {
		return nil, errors.New("ASTOVIA_PUBLIC_URL must use https with Cloudflare Access")
	}
	issuer := domain + "/cdn-cgi/access/sso/oidc/" + id
	return &AccessClient{TeamDomain: domain, ClientID: id, Secret: secret, Issuer: issuer, RedirectURI: publicURL + AccessCallbackPath}, nil
}

// accessTeamDomain checks an Access team domain and returns it lower-case,
// without the one trailing slash it may have.
func accessTeamDomain(raw string) (string, error) {
	invalid := errors.New("ASTOVIA_ACCESS_TEAM_DOMAIN must be an origin such as https://<team>.cloudflareaccess.com")
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.Port() != "" || strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	host := strings.ToLower(u.Host)
	// A trailing dot would not match the iss claim.
	if strings.HasSuffix(host, ".") || !ValidDNSHost(host) || !strings.HasSuffix(host, ".cloudflareaccess.com") {
		return "", invalid
	}
	return "https://" + host, nil
}

// validAccessSecret accepts 1-1024 bytes of printable ASCII other than the
// space.
func validAccessSecret(v string) bool {
	if len(v) == 0 || len(v) > 1024 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// mailConfig reads the mail service. An empty ASTOVIA_MAIL_PROVIDER
// disables e-mail notifications; any other mail variable set without it is
// an error, so that a forgotten provider is noticed. Values are trimmed.
func mailConfig() (*MailConfig, error) {
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	provider, key, from, name, account := strings.ToLower(get("ASTOVIA_MAIL_PROVIDER")), get("ASTOVIA_MAIL_API_KEY"), get("ASTOVIA_MAIL_FROM"), get("ASTOVIA_MAIL_FROM_NAME"), get("ASTOVIA_CLOUDFLARE_ACCOUNT_ID")
	if provider == "" {
		if key != "" || from != "" || name != "" || account != "" {
			return nil, errors.New("ASTOVIA_MAIL_PROVIDER is required when ASTOVIA_MAIL_API_KEY, ASTOVIA_MAIL_FROM, ASTOVIA_MAIL_FROM_NAME or ASTOVIA_CLOUDFLARE_ACCOUNT_ID is set")
		}
		return nil, nil
	}
	if provider != MailCloudflare && provider != MailResend {
		return nil, errors.New("ASTOVIA_MAIL_PROVIDER must be cloudflare or resend")
	}
	if !validMailKey(key) {
		return nil, errors.New("ASTOVIA_MAIL_API_KEY must be 1 to 512 printable ASCII characters")
	}
	if a, err := mail.ParseAddress(from); err != nil || a.Name != "" || a.Address != from {
		return nil, errors.New("ASTOVIA_MAIL_FROM must be a bare e-mail address such as notify@example.com")
	}
	if name != "" && !validMailFromName(name) {
		return nil, errors.New(`ASTOVIA_MAIL_FROM_NAME must be 1 to 100 characters without control characters or "(),:;<>@[\]`)
	}
	switch {
	case provider == MailCloudflare && !cloudflareAccountIDPattern.MatchString(account):
		return nil, errors.New("ASTOVIA_CLOUDFLARE_ACCOUNT_ID must be the 32 hexadecimal characters of the Cloudflare account ID")
	case provider == MailResend && account != "":
		return nil, errors.New("ASTOVIA_CLOUDFLARE_ACCOUNT_ID is used only with ASTOVIA_MAIL_PROVIDER=cloudflare")
	}
	return &MailConfig{Provider: provider, APIKey: key, AccountID: account, From: from, FromName: name}, nil
}

// validMailKey accepts 1-512 bytes of printable ASCII.
func validMailKey(v string) bool {
	if len(v) == 0 || len(v) > 512 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// validMailFromName accepts 1-100 characters without control characters
// and the characters that would need quoting in a mail header.
func validMailFromName(v string) bool {
	n := 0
	for _, c := range v {
		n++
		if c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0) || strings.ContainsRune(`"(),:;<>@[\]`, c) {
			return false
		}
	}
	return n >= 1 && n <= 100
}

// boolEnv reads a boolean variable: true or 1 is true, and unset, empty,
// false or 0 is false, in any case. Any other value is an error.
func boolEnv(k string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "true", "1":
		return true, nil
	case "", "false", "0":
		return false, nil
	}
	return false, errors.New(k + " must be true or false")
}

func env(k, d string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	return v
}

// canonicalOrigin supports explicit ASCII origins, not browser-specific host shorthand.
func canonicalOrigin(raw string) (string, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false, errors.New("ASTOVIA_PUBLIC_URL must be an http(s) origin without path, credentials, query, or fragment")
	}
	host := strings.ToLower(u.Hostname())
	ip, ipErr := netip.ParseAddr(host)
	if ipErr == nil {
		if ip.Zone() != "" || ip.Is4In6() || (strings.HasPrefix(u.Host, "[") && !ip.Is6()) {
			return "", false, errors.New("ASTOVIA_PUBLIC_URL does not support scoped or IPv4-mapped IPv6 hosts, or bracketed IPv4")
		}
		host = ip.String()
	} else if !ValidDNSHost(host) {
		return "", false, errors.New("ASTOVIA_PUBLIC_URL requires an ASCII DNS/punycode hostname or an explicit IP address")
	}
	if u.Scheme == "http" && host != "localhost" && (ipErr != nil || !ip.IsLoopback()) {
		return "", false, errors.New("HTTP public URL is permitted only for loopback development")
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", false, errors.New("ASTOVIA_PUBLIC_URL port must be an integer from 1 to 65535")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false, errors.New("ASTOVIA_PUBLIC_URL port must be an integer from 1 to 65535")
		}
		port = strconv.Itoa(n)
		if (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443) {
			port = ""
		}
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	} else if ipErr == nil && ip.Is6() {
		authority = "[" + host + "]"
	}
	return u.Scheme + "://" + authority, u.Scheme == "https", nil
}

// ValidDNSHost reports whether host is a lower-case ASCII DNS name (punycode for
// international names) that cannot be read as a numeric IPv4 shorthand.
// internal/oauth uses it for client and redirect URI hosts as well.
func ValidDNSHost(host string) bool {
	name := strings.TrimSuffix(host, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	// Browsers parse a final numeric label as IPv4, including hexadecimal shorthand.
	// Require such hosts to have passed netip.ParseAddr instead of guessing their meaning.
	last := labels[len(labels)-1]
	digits := "0123456789"
	if strings.HasPrefix(last, "0x") {
		last = last[2:]
		digits += "abcdef"
	}
	return strings.Trim(last, digits) != ""
}
