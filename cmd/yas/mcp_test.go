package main

import "testing"

func TestNormalizeMCPAddr(t *testing.T) {
	cases := []struct {
		addr          string
		allowInsecure bool
		want          string
		wantErr       bool
	}{
		{"8770", false, "127.0.0.1:8770", false},
		{":8770", false, "127.0.0.1:8770", false},
		{"127.0.0.1:8770", false, "127.0.0.1:8770", false},
		{"localhost:8770", false, "localhost:8770", false},
		{"[::1]:8770", false, "[::1]:8770", false},
		{"192.0.2.5:8770", false, "", true},               // non-loopback refused
		{"192.0.2.5:8770", true, "192.0.2.5:8770", false}, // ...unless opted in
		{"0.0.0.0:8770", false, "", true},
		{"", false, "", true},
		{"notaport", false, "", true},
	}
	for _, c := range cases {
		got, err := normalizeMCPAddr(c.addr, c.allowInsecure)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizeMCPAddr(%q,%v): want error, got %q", c.addr, c.allowInsecure, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeMCPAddr(%q,%v): unexpected error %v", c.addr, c.allowInsecure, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeMCPAddr(%q,%v): got %q want %q", c.addr, c.allowInsecure, got, c.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, c := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"localhost", true},
		{"192.0.2.5", false},
		{"0.0.0.0", false},
		{"", false}, // empty binds all interfaces — must not count as loopback
	} {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
