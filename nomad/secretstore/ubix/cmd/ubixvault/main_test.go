package main

import "testing"

func TestIsLoopbackListen(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8200":     true,
		"localhost:8200":     true,
		"[::1]:8200":         true,
		"0.0.0.0:8200":       false,
		":8200":              false,
		"192.168.1.10:8200":  false,
		"10.0.0.5:8200":      false,
		"vault.example:8200": false,
	}
	for listen, want := range cases {
		if got := isLoopbackListen(listen); got != want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", listen, got, want)
		}
	}
}

func TestCheckTLSPolicy(t *testing.T) {
	cases := []struct {
		name       string
		listen     string
		tlsEnabled bool
		devNoTLS   bool
		wantErr    bool
	}{
		{"loopback plaintext ok", "127.0.0.1:8200", false, false, false},
		{"localhost plaintext ok", "localhost:8200", false, false, false},
		{"non-loopback plaintext refused", "0.0.0.0:8200", false, false, true},
		{"non-loopback with TLS ok", "0.0.0.0:8200", true, false, false},
		{"non-loopback with override ok", "0.0.0.0:8200", false, true, false},
		{"specific IP plaintext refused", "192.168.1.10:8200", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkTLSPolicy(c.listen, c.tlsEnabled, c.devNoTLS)
			if (err != nil) != c.wantErr {
				t.Fatalf("checkTLSPolicy(%q, tls=%v, dev=%v) err = %v, wantErr = %v",
					c.listen, c.tlsEnabled, c.devNoTLS, err, c.wantErr)
			}
		})
	}
}
