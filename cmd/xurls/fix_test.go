// Copyright (c) 2015, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCategorize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"timeout", &url.Error{Op: "Head", Err: &net.DNSError{IsTimeout: true}}, "timeout"},
		{"dns", &url.Error{Op: "Head", Err: &net.DNSError{IsNotFound: true}}, "dns"},
		{"redirect", &url.Error{Op: "Head", Err: errors.New("stopped after 10 redirects")}, "redirect"},
		{"connect", &url.Error{Op: "Head", Err: errors.New("connection refused")}, "connect"},
		{"other", fmt.Errorf("some other failure"), "connect"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := categorize(tc.err); got != tc.want {
				t.Fatalf("categorize(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestCheckURL(t *testing.T) {
	// Do not let any ambient proxy settings interfere with the checks.
	for _, v := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		t.Setenv(v, "")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/perm", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/perm-frag", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok#target", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/temp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusFound)
	})
	mux.HandleFunc("/get-only", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/ok", http.StatusMovedPermanently)
			return
		}
		http.Error(w, "", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "", http.StatusNotFound)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusMovedPermanently)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const defaultTimeout = 5 * time.Second
	tests := []struct {
		name     string
		url      string
		fixAll   bool
		timeout  time.Duration // defaults to defaultTimeout
		want     string        // empty means an error is expected
		wantKind string
	}{
		{"plain", "/ok", false, 0, "/ok", ""},
		{"permanent redirect", "/perm", false, 0, "/ok", ""},
		{"fragment inherited", "/perm#frag", false, 0, "/ok#frag", ""},
		{"fragment replaced", "/perm-frag#frag", false, 0, "/ok#target", ""},
		{"temporary redirect auto", "/temp", false, 0, "/temp", ""},
		{"temporary redirect all", "/temp", true, 0, "/ok", ""},
		{"405 falls back to GET", "/get-only", false, 0, "/ok", ""},
		{"http status", "/missing", false, 0, "", "http_status"},
		{"timeout", "/slow", false, 100 * time.Millisecond, "", "timeout"},
		{"redirect loop", "/loop", false, 0, "", "redirect"},
		{"parse error", "http://exa mple.com/", false, 0, "", "parse"},
		{"dns error", "http://nonexistent.invalid/", false, 0, "", "dns"},
		{"non-http scheme", "mailto:foo@bar.com", false, 0, "mailto:foo@bar.com", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.url
			if strings.HasPrefix(raw, "/") {
				raw = srv.URL + raw
			}
			want := tc.want
			if strings.HasPrefix(want, "/") {
				want = srv.URL + want
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = defaultTimeout
			}
			final, cerr := checkURL(raw, "xurls-test", tc.fixAll, timeout)
			if tc.wantKind != "" {
				if cerr == nil {
					t.Fatalf("got final %q, want error kind %q", final, tc.wantKind)
				}
				if cerr.kind != tc.wantKind {
					t.Fatalf("got kind %q (%s), want %q", cerr.kind, cerr.msg, tc.wantKind)
				}
				return
			}
			if cerr != nil {
				t.Fatalf("unexpected error: %s (kind %s)", cerr.msg, cerr.kind)
			}
			if final != want {
				t.Fatalf("got %q, want %q", final, want)
			}
		})
	}
}
