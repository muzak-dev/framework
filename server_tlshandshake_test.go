package muzak

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// selfSignedCertificate returns a certificate for 127.0.0.1, good for one test.
func selfSignedCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A client that opens a TLS connection and never finishes the handshake, or
// finishes it a byte at a time, is bounded by ReadHeaderTimeout: net/http
// gives the handshake the shorter of it and ReadTimeout. Neither the silent
// connect nor the trickle can hold a connection, and its goroutine, longer.
func TestTLSHandshakeIsBoundedByTheHeaderTimeout(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.ReadHeaderTimeout = 300 * time.Millisecond
	opts.ReadTimeout = 5 * time.Second
	opts.TLSConfig = &tls.Config{Certificates: []tls.Certificate{selfSignedCertificate(t)}}
	app := New(opts)
	app.Get("/x", okHandler)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	var addr string
	for deadline := time.Now().Add(5 * time.Second); addr == ""; addr = app.Addr() {
		if time.Now().After(deadline) {
			t.Fatal("the server never listened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = app.Shutdown(context.Background())
		<-done
	})

	for name, trickle := range map[string]bool{"silent": false, "one byte at a time": true} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			start := time.Now()
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				buf := make([]byte, 1)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
			// The record header of a handshake that claims 1 KiB to come.
			hello := append([]byte{0x16, 0x03, 0x01, 0x04, 0x00, 0x01}, make([]byte, 1024)...)
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			sent := 0
			for {
				select {
				case <-closed:
					if elapsed := time.Since(start); elapsed > 2*time.Second {
						t.Errorf("the handshake was cut after %s, want about the 300ms header timeout", elapsed)
					}
					return
				case <-ticker.C:
					if trickle && sent < len(hello) {
						_, _ = conn.Write(hello[sent : sent+1])
						sent++
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the server held an unfinished handshake for 5 seconds")
				}
			}
		})
	}
}
