package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestAllowedCIMDAddr(t *testing.T) {
	for _, v := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", // loopback, RFC 1918
		"169.254.169.254", "100.64.0.1", "100.127.255.255", "0.1.2.3", "0.0.0.0", // link-local, CGNAT, 0/8
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1", // special-use, documentation, benchmark
		"240.0.0.1", "255.255.255.255", "224.0.0.1", "239.255.255.250", // reserved, broadcast, multicast
		"::1", "::", "fc00::1", "fd12:3456::1", "fe80::1", "ff02::1", // IPv6 loopback, unspecified, ULA, link-local, multicast
		"2001::1", "2001:1ff::1", "2001:db8::1", "2002:7f00:1::1", "3fff::1", // Teredo, 2001::/23, documentation, 6to4, 3fff::/20
		"64:ff9b::7f00:1", "::127.0.0.1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", // NAT64, IPv4-compatible, IPv4-mapped
		"fe80::1%eth0",
	} {
		if AllowedCIMDAddr(netip.MustParseAddr(v)) {
			t.Fatalf("allowed %s", v)
		}
	}
	for _, v := range []string{"8.8.8.8", "1.1.1.1", "160.79.104.10", "100.63.255.255", "100.128.0.0", "::ffff:8.8.8.8", "2606:4700::1111", "2001:4860:4860::8888", "2400:cb00::1"} {
		if !AllowedCIMDAddr(netip.MustParseAddr(v)) {
			t.Fatalf("refused %s", v)
		}
	}
}

func TestCacheTTL(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"":                             time.Hour,
		"no-cache":                     time.Hour,
		"max-age=600":                  10 * time.Minute,
		"public, Max-Age = 120":        2 * time.Minute,
		"max-age=0":                    time.Hour,
		"max-age=-5":                   time.Hour,
		"max-age=abc":                  time.Hour,
		"max-age=86400":                24 * time.Hour,
		"max-age=999999999999":         24 * time.Hour,
		"max-age=99999999999999999999": time.Hour,
	} {
		if got := cacheTTL(header); got != want {
			t.Fatalf("%q: %v, want %v", header, got, want)
		}
	}
}

func cimdDocument(clientID, name string, uris ...string) string {
	quoted := make([]string, len(uris))
	for i, u := range uris {
		quoted[i] = `"` + u + `"`
	}
	return fmt.Sprintf(`{"client_id":%q,"client_name":%q,"redirect_uris":[%s],"token_endpoint_auth_method":"private_key_jwt"}`, clientID, name, strings.Join(quoted, ","))
}

func TestFetchCIMD(t *testing.T) {
	var base string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := base + r.URL.Path
		if r.Header.Get("Accept") != "application/json" {
			w.WriteHeader(406)
			return
		}
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Cache-Control", "public, max-age=600")
			fmt.Fprint(w, cimdDocument(id, "  Claude Code  ", "http://localhost/callback", "http://127.0.0.1/callback"))
		case "/default":
			fmt.Fprint(w, cimdDocument(id, "Claude", "https://claude.ai/api/mcp/auth_callback"))
		case "/limit":
			doc := cimdDocument(id, "Claude", "https://claude.ai/api/mcp/auth_callback")
			fmt.Fprint(w, doc+strings.Repeat(" ", CIMDBodyLimit-len(doc)))
		case "/large":
			doc := cimdDocument(id, "Claude", "https://claude.ai/api/mcp/auth_callback")
			fmt.Fprint(w, doc+strings.Repeat(" ", CIMDBodyLimit-len(doc)+1))
		case "/mismatch":
			fmt.Fprint(w, cimdDocument(base+"/other", "Claude", "https://claude.ai/api/mcp/auth_callback"))
		case "/noname":
			fmt.Fprint(w, cimdDocument(id, " ​ ", "https://claude.ai/api/mcp/auth_callback"))
		case "/badredirect":
			fmt.Fprint(w, cimdDocument(id, "Claude", "myapp://callback"))
		case "/array":
			fmt.Fprint(w, "[]")
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/notfound":
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	tlsConfig := srv.Client().Transport.(*http.Transport).TLSClientConfig
	client := newCIMDClient(func(netip.Addr) bool { return true }, tlsConfig)
	ctx := context.Background()

	doc, err := FetchCIMD(ctx, client, base+"/ok")
	if err != nil || doc.Name != "Claude Code" || len(doc.RedirectURIs) != 2 || doc.RedirectURIs[0] != "http://localhost/callback" || doc.TTL != 10*time.Minute {
		t.Fatalf("ok: %+v %v", doc, err)
	}
	if doc, err = FetchCIMD(ctx, client, base+"/default"); err != nil || doc.TTL != time.Hour {
		t.Fatalf("default TTL: %+v %v", doc, err)
	}
	if _, err = FetchCIMD(ctx, client, base+"/limit"); err != nil {
		t.Fatalf("body of exactly 64 KiB: %v", err)
	}
	for _, path := range []string{"/large", "/mismatch", "/noname", "/badredirect", "/array", "/redirect", "/notfound"} {
		if doc, err = FetchCIMD(ctx, client, base+path); err == nil {
			t.Fatalf("%s accepted: %+v", path, doc)
		}
	}

	// The production client refuses the loopback server after resolving the
	// name, both for the IP literal and for localhost.
	for _, u := range []string{base + "/ok", strings.Replace(base, "127.0.0.1", "localhost", 1) + "/ok"} {
		if _, err = FetchCIMD(ctx, NewCIMDClient(), u); !errors.Is(err, errBlockedAddress) {
			t.Fatalf("%s: %v", u, err)
		}
	}
}
