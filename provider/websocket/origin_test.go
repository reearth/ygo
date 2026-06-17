package websocket

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOriginMatches(t *testing.T) {
	cases := []struct {
		pattern, origin string
		want            bool
	}{
		// exact (no wildcard)
		{"https://flow.test.reearth.dev", "https://flow.test.reearth.dev", true},
		{"https://flow.test.reearth.dev", "https://other.reearth.dev", false},
		{"https://Example.com", "https://example.com", true}, // case-insensitive
		{"http://localhost:3000", "http://localhost:3000", true},
		{"http://localhost:3000", "http://localhost:3001", false},

		// bare "*" allows anything
		{"*", "https://anything.example", true},

		// single subdomain wildcard
		{"https://*.netlify.app", "https://foo.netlify.app", true},
		{"https://*.netlify.app", "https://deploy-preview-12--site.netlify.app", true},
		{"https://*.netlify.app", "https://netlify.app", false},            // no subdomain label
		{"https://*.netlify.app", "https://evil.com", false},               // wrong suffix
		{"https://*.netlify.app", "https://x.netlify.app.evil.com", false}, // suffix anchored

		// multi-wildcard preview host
		{"https://pr-*---reearth-flow-web-*.a.run.app", "https://pr-1701---reearth-flow-web-6yxenhqnsq-uc.a.run.app", true},
		{"https://pr-*---reearth-flow-web-*.a.run.app", "https://pr-1---reearth-flow-web-x.a.run.app", true},
		{"https://pr-*---reearth-flow-web-*.a.run.app", "https://wsgo---reearth-flow-web-x.a.run.app", false},      // wrong prefix
		{"https://pr-*---reearth-flow-web-*.a.run.app", "https://pr-1---reearth-flow-web-x.a.run.app.evil", false}, // suffix anchored

		// trailing wildcard (e.g. allow an optional port)
		{"https://app.example.com*", "https://app.example.com", true},
		{"https://app.example.com*", "https://app.example.com:8443", true},
		{"https://app.example.com*", "https://app.example.org", false},
	}
	for _, c := range cases {
		if got := originMatches(c.pattern, c.origin); got != c.want {
			t.Errorf("originMatches(%q, %q) = %v, want %v", c.pattern, c.origin, got, c.want)
		}
	}
}

func TestCheckOrigin(t *testing.T) {
	mk := func(origin, host string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://"+host+"/room", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	t.Run("wildcard allow-list", func(t *testing.T) {
		s := &Server{AllowedOrigins: []string{
			"http://localhost:3000",
			"https://flow.test.reearth.dev",
			"https://*.netlify.app",
			"https://pr-*---reearth-flow-web-*.a.run.app",
		}}
		ok := []string{
			"https://flow.test.reearth.dev",
			"http://localhost:3000",
			"https://foo.netlify.app",
			"https://pr-1701---reearth-flow-web-6yxenhqnsq-uc.a.run.app",
		}
		for _, o := range ok {
			if !s.checkOrigin(mk(o, "ws.example")) {
				t.Errorf("checkOrigin(%q) = false, want true", o)
			}
		}
		bad := []string{
			"https://evil.com",
			"https://flow.prod.reearth.dev",
			"https://x.netlify.app.evil.com",
			"https://wsgo---reearth-flow-web-x.a.run.app",
		}
		for _, o := range bad {
			if s.checkOrigin(mk(o, "ws.example")) {
				t.Errorf("checkOrigin(%q) = true, want false", o)
			}
		}
	})

	t.Run("absent Origin header is permitted (non-browser client)", func(t *testing.T) {
		s := &Server{AllowedOrigins: []string{"https://flow.test.reearth.dev"}}
		if !s.checkOrigin(mk("", "ws.example")) {
			t.Error("empty Origin should be permitted")
		}
	})

	t.Run("empty AllowedOrigins falls back to same-origin", func(t *testing.T) {
		s := &Server{}
		if !s.checkOrigin(mk("https://ws.example", "ws.example")) {
			t.Error("same-origin should be allowed")
		}
		if s.checkOrigin(mk("https://other.example", "ws.example")) {
			t.Error("cross-origin should be rejected under same-origin fallback")
		}
	})

	t.Run("bare star allows any origin", func(t *testing.T) {
		s := &Server{AllowedOrigins: []string{"*"}}
		if !s.checkOrigin(mk("https://whatever.example", "ws.example")) {
			t.Error(`AllowedOrigins=["*"] should allow any origin`)
		}
	})
}
