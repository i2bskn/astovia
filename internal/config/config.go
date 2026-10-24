package config

import (
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Addr, PublicURL, Driver, DSN string
	Secret                       []byte
	Secure                       bool
}

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
	if requireSecret {
		c.Secret, err = base64.StdEncoding.DecodeString(os.Getenv("ASTOVIA_JWT_SECRET"))
		if err != nil || len(c.Secret) < 32 {
			return c, errors.New("ASTOVIA_JWT_SECRET must be padded base64 encoding at least 32 random bytes")
		}
	}
	return c, nil
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
