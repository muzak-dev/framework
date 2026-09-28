package muzak

import (
	"strings"
	"testing"
)

// TestHalfAConfiguredCertificatePairIsABuildError is the regression test for a
// TLS setup that quietly became plaintext: with only one of CertFile and
// KeyFile set, which is what a typo or a missing environment variable
// produces, the application built and served HTTP on the port its operator
// believed was HTTPS.
func TestHalfAConfiguredCertificatePairIsABuildError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts ServerOptions
		want string
	}{
		{"a certificate without a key", ServerOptions{CertFile: "cert.pem"}, "KeyFile"},
		{"a key without a certificate", ServerOptions{KeyFile: "key.pem"}, "CertFile"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.ServerOptions = tc.opts
			app := New(opts)
			app.Get("/x", okHandler)
			if msg := buildError(t, app); !strings.Contains(msg, tc.want) {
				t.Errorf("Build() = %q, want it to name %s", msg, tc.want)
			}
		})
	}
}

// TestACompleteOrAbsentCertificatePairBuilds pins the configurations the check
// must leave alone.
func TestACompleteOrAbsentCertificatePairBuilds(t *testing.T) {
	t.Parallel()
	for name, server := range map[string]ServerOptions{
		"no TLS":             {},
		"a certificate pair": {CertFile: "cert.pem", KeyFile: "key.pem"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.ServerOptions = server
			app := New(opts)
			app.Get("/x", okHandler)
			mustBuild(t, app)
		})
	}
}
