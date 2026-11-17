package webhook_test

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/webhook"
)

func signHMAC(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	if ts != "" {
		_, _ = mac.Write([]byte(ts))
		_, _ = mac.Write([]byte("."))
	}
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookHMACGuard(t *testing.T) {
	t.Parallel()
	secret := []byte("whsec_super_secret_key_for_testing")
	now := time.Unix(1_700_000_000, 0)

	type event struct {
		Type string `json:"type"`
	}
	app := muzak.New(muzak.AppOptions{DisableDocs: true, DisableAccessLog: true})
	app.Post("/hooks", func(_ *muzak.Context, ev event) (map[string]string, error) {
		return map[string]string{"event": ev.Type}, nil
	}, muzak.CaptureBody(), muzak.WithDependencies(webhook.Guard(webhook.Options{
		Secret:       secret,
		MaxBodyBytes: 64,
		Now:          func() time.Time { return now },
	})))

	body := []byte(`{"type":"invoice.paid"}`)
	ts := strconv.FormatInt(now.Unix(), 10)

	// Valid signature + timestamp -> 200 and body binds cleanly
	req := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.DefaultTimestampHeader, ts)
	req.Header.Set(webhook.DefaultHeader, signHMAC(secret, ts, body))
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "invoice.paid") {
		t.Fatalf("valid webhook status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Tampered body -> 401
	reqTampered := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(`{"type":"invoice.void"}`))
	reqTampered.Header.Set("Content-Type", "application/json")
	reqTampered.Header.Set(webhook.DefaultTimestampHeader, ts)
	reqTampered.Header.Set(webhook.DefaultHeader, signHMAC(secret, ts, body))
	recTampered := httptest.NewRecorder()
	app.ServeHTTP(recTampered, reqTampered)
	if recTampered.Code != http.StatusUnauthorized {
		t.Fatalf("tampered status = %d, want 401", recTampered.Code)
	}

	// Expired timestamp -> 401
	oldTS := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
	reqExpired := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(string(body)))
	reqExpired.Header.Set("Content-Type", "application/json")
	reqExpired.Header.Set(webhook.DefaultTimestampHeader, oldTS)
	reqExpired.Header.Set(webhook.DefaultHeader, signHMAC(secret, oldTS, body))
	recExpired := httptest.NewRecorder()
	app.ServeHTTP(recExpired, reqExpired)
	if recExpired.Code != http.StatusUnauthorized {
		t.Fatalf("expired status = %d, want 401", recExpired.Code)
	}

	// Oversized body -> 413
	big := strings.Repeat("a", 80)
	reqBig := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(big))
	reqBig.Header.Set(webhook.DefaultTimestampHeader, ts)
	reqBig.Header.Set(webhook.DefaultHeader, signHMAC(secret, ts, []byte(big)))
	recBig := httptest.NewRecorder()
	app.ServeHTTP(recBig, reqBig)
	if recBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", recBig.Code)
	}
}

func TestWebhookEd25519AndNoCaptureBody(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	app := muzak.New(muzak.AppOptions{DisableDocs: true, DisableAccessLog: true})
	app.Post("/ed", func(_ *muzak.Context, _ muzak.Empty) (string, error) {
		return "ok", nil
	}, muzak.CaptureBody(), muzak.WithDependencies(webhook.Guard(webhook.Options{
		Algorithm: webhook.Ed25519,
		PublicKey: pub,
		Tolerance: -1,
	})))
	app.Post("/missing-capture", func(_ *muzak.Context, _ muzak.Empty) (string, error) {
		return "ok", nil
	}, muzak.WithDependencies(webhook.Guard(webhook.Options{Secret: []byte("s"), Tolerance: -1})))

	body := []byte(`{"ping":1}`)
	sig := hex.EncodeToString(ed25519.Sign(priv, body))
	req := httptest.NewRequest(http.MethodPost, "/ed", strings.NewReader(string(body)))
	req.Header.Set(webhook.DefaultHeader, sig)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ed25519 status = %d, want 200", rec.Code)
	}

	reqMissing := httptest.NewRequest(http.MethodPost, "/missing-capture", strings.NewReader(string(body)))
	recMissing := httptest.NewRecorder()
	app.ServeHTTP(recMissing, reqMissing)
	if recMissing.Code != http.StatusInternalServerError {
		t.Fatalf("missing captureBody status = %d, want 500", recMissing.Code)
	}

	// Base64URL and StdEncoding Ed25519 signatures + error branches
	app2 := muzak.New(muzak.AppOptions{DisableDocs: true, DisableAccessLog: true})
	app2.Post("/ed", func(_ *muzak.Context, _ muzak.Empty) (string, error) { return "ok", nil },
		muzak.CaptureBody(), muzak.WithDependencies(webhook.Guard(webhook.Options{
			Algorithm: webhook.Ed25519,
			PublicKey: pub,
			Tolerance: -1,
		})))
	app2.Post("/bad-alg", func(_ *muzak.Context, _ muzak.Empty) (string, error) { return "ok", nil },
		muzak.CaptureBody(), muzak.WithDependencies(webhook.Guard(webhook.Options{Algorithm: 99, Tolerance: -1})))
	app2.Post("/no-secret", func(_ *muzak.Context, _ muzak.Empty) (string, error) { return "ok", nil },
		muzak.CaptureBody(), muzak.WithDependencies(webhook.Guard(webhook.Options{Tolerance: -1})))
	app2.Post("/no-pub", func(_ *muzak.Context, _ muzak.Empty) (string, error) { return "ok", nil },
		muzak.CaptureBody(), muzak.WithDependencies(webhook.Guard(webhook.Options{Algorithm: webhook.Ed25519, Tolerance: -1})))

	rawSig := ed25519.Sign(priv, body)
	for _, encSig := range []string{
		base64.RawURLEncoding.EncodeToString(rawSig),
		base64.StdEncoding.EncodeToString(rawSig),
	} {
		r := httptest.NewRequest(http.MethodPost, "/ed", strings.NewReader(string(body)))
		r.Header.Set(webhook.DefaultHeader, encSig)
		rec2 := httptest.NewRecorder()
		app2.ServeHTTP(rec2, r)
		if rec2.Code != http.StatusOK {
			t.Fatalf("encoded Ed25519 status = %d", rec2.Code)
		}
	}

	for _, tc := range []struct {
		path   string
		header map[string]string
		want   int
	}{
		{"/ed", nil, http.StatusUnauthorized},
		{"/ed", map[string]string{webhook.DefaultHeader: "not-valid-sig"}, http.StatusUnauthorized},
		{"/bad-alg", map[string]string{webhook.DefaultHeader: "aa"}, http.StatusInternalServerError},
		{"/no-secret", map[string]string{webhook.DefaultHeader: "aa"}, http.StatusInternalServerError},
		{"/no-pub", map[string]string{webhook.DefaultHeader: "aa"}, http.StatusInternalServerError},
	} {
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(string(body)))
		for k, v := range tc.header {
			r.Header.Set(k, v)
		}
		rec2 := httptest.NewRecorder()
		app2.ServeHTTP(rec2, r)
		if rec2.Code != tc.want {
			t.Fatalf("%s status = %d, want %d", tc.path, rec2.Code, tc.want)
		}
	}

	// Timestamp header validation edges
	appTS := muzak.New(muzak.AppOptions{DisableDocs: true, DisableAccessLog: true})
	appTS.Post("/ts", func(_ *muzak.Context, _ muzak.Empty) (string, error) { return "ok", nil },
		muzak.CaptureBody(), muzak.WithDependencies(webhook.Guard(webhook.Options{Secret: []byte("sec")})))
	for _, tc := range []struct {
		headers map[string]string
		want    int
	}{
		{map[string]string{webhook.DefaultHeader: "zz"}, http.StatusUnauthorized},
		{map[string]string{webhook.DefaultHeader: "zz", webhook.DefaultTimestampHeader: "not-int"}, http.StatusUnauthorized},
		{map[string]string{webhook.DefaultHeader: "not-hex", webhook.DefaultTimestampHeader: strconv.FormatInt(time.Now().Unix(), 10)}, http.StatusUnauthorized},
	} {
		r := httptest.NewRequest(http.MethodPost, "/ts", strings.NewReader(string(body)))
		for k, v := range tc.headers {
			r.Header.Set(k, v)
		}
		rec2 := httptest.NewRecorder()
		appTS.ServeHTTP(rec2, r)
		if rec2.Code != tc.want {
			t.Fatalf("ts edge status = %d, want %d", rec2.Code, tc.want)
		}
	}
}
