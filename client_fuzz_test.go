package muzak

import (
	"errors"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

// FuzzClientAddressPolicy feeds arbitrary hosts and IPv4 addresses to the
// policy and holds it to what makes it safe: a refusal is always the one error
// type with a bounded message; a host written as an address is judged exactly
// as that address is; a host that a browser or the C resolver would read as an
// IPv4 address is never let through as a name; and no IPv6 spelling of a
// refused IPv4 address, mapped, NAT64, 6to4 or compatible, is allowed where
// the address itself is not.
func FuzzClientAddressPolicy(f *testing.F) {
	for _, vector := range ssrfVectors {
		if host := strings.TrimSuffix(strings.TrimPrefix(vector.url, "http://"), "/"); host != "" {
			f.Add(host, byte(127), byte(0), byte(0), byte(1))
		}
	}
	for _, seed := range []string{"example.com", "api.example.com.", "93.184.216.34", "[::1]", "0x", "0x.0x",
		"1.2.3.4.5", "\uff11\uff12\uff17.0.0.1", "xn--bcher-kva.example", "a.localhost.", "LocalHost", "::ffff:1.2.3.4",
		"fe80::1%eth0", "08.1", "0.0.0.0.", "4294967295", "4294967296"} {
		f.Add(seed, byte(169), byte(254), byte(169), byte(254))
	}
	f.Add("8.8.8.8", byte(8), byte(8), byte(8), byte(8))
	f.Add("10.0.0.1", byte(10), byte(0), byte(0), byte(1))

	strict := newAddressPolicy(ClientOptions{})
	relaxed := newAddressPolicy(ClientOptions{AllowPrivateNetworks: true})

	refusedAs := func(t *testing.T, err error) *AddressRefusedError {
		t.Helper()
		if err == nil {
			return nil
		}
		var refused *AddressRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("refusal %v is not an *AddressRefusedError", err)
		}
		if message := err.Error(); !strings.HasPrefix(message, "muzak: ") || len(message) > 1024 {
			t.Fatalf("refusal message %q is not a bounded muzak: sentence", message)
		}
		return refused
	}

	f.Fuzz(func(t *testing.T, host string, a, b, c, d byte) {
		for _, policy := range []*addressPolicy{strict, relaxed} {
			for _, proxied := range []bool{false, true} {
				err := policy.checkHost(host, proxied)
				refusedAs(t, err)
				if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
					if want := policy.check(host, addr); (err == nil) != (want == nil) {
						t.Fatalf("checkHost(%q) = %v, but check of the address = %v", host, err, want)
					}
				}
			}
		}
		name := strings.ToLower(strings.TrimSuffix(host, "."))
		if _, parseErr := netip.ParseAddr(host); parseErr != nil {
			if _, legacy := parseLegacyIPv4(name); legacy && strict.checkHost(host, false) == nil {
				t.Fatalf("checkHost(%q) let through a host that reads as an IPv4 address", host)
			}
		}

		v4 := netip.AddrFrom4([4]byte{a, b, c, d})
		if round, ok := parseLegacyIPv4(v4.String()); !ok || round != v4 {
			t.Fatalf("parseLegacyIPv4(%s) = %v, %v, want the address back", v4, round, ok)
		}
		var ipv6 [16]byte
		for _, policy := range []*addressPolicy{strict, relaxed} {
			base := refusedAs(t, policy.check(v4.String(), v4))
			mapped := refusedAs(t, policy.check("mapped", netip.AddrFrom16(netip.AddrFrom4([4]byte{a, b, c, d}).As16())))
			if (base == nil) != (mapped == nil) || (base != nil && base.kind != mapped.kind) {
				t.Fatalf("%s judged %v, but its IPv4-mapped form %v", v4, base, mapped)
			}
			if base == nil {
				continue
			}
			for _, prefix := range []string{"64:ff9b::", "2002::", "::"} {
				ipv6 = netip.MustParseAddr(prefix).As16()
				switch prefix {
				case "2002::":
					copy(ipv6[2:6], []byte{a, b, c, d})
				default:
					copy(ipv6[12:16], []byte{a, b, c, d})
				}
				embedded := netip.AddrFrom16(ipv6)
				if prefix == "::" && (embedded == netip.IPv6Unspecified() || embedded == netip.IPv6Loopback()) {
					continue
				}
				form := refusedAs(t, policy.check(embedded.String(), embedded))
				if form == nil {
					t.Fatalf("%s is refused (%s) but %s, which reaches it, is allowed", v4, base.Reason, embedded)
				}
				if base.kind == refusedMetadata && form.kind != refusedMetadata {
					t.Fatalf("%s is a metadata address but %s was refused as %v", v4, embedded, form.kind)
				}
			}
		}
	})
}

// FuzzParseRetryAfter holds the Retry-After parser to the properties the
// retry loop relies on: it never panics, never reports a negative wait,
// reports nothing for a value it did not accept, reads a number of seconds
// exactly or saturates it, does not parse a date out of an overlong value,
// and reads back any date the HTTP date format can write.
func FuzzParseRetryAfter(f *testing.F) {
	for _, seed := range []string{"0", "120", " 7 ", strings.Repeat("9", 40), "9223372037", "-1", "1.5", "",
		"Fri, 09 Oct 2026 12:00:30 GMT", "Friday, 09-Oct-26 12:00:30 GMT", "Fri Oct  9 12:01:00 2026",
		"Thu, 01 Jan 1970 00:00:00 GMT", "Fri, 31 Dec 9999 23:59:59 GMT", "tomorrow", "\t\t", "1 2"} {
		f.Add(seed, int64(1791547200), int32(90))
	}
	f.Fuzz(func(t *testing.T, value string, unix int64, delta int32) {
		// A clock from 1970 to a few thousand years on, so that the date
		// formatted below stays within the four digit years HTTP can write.
		now := time.Unix(int64(uint64(unix)%200_000_000_000), 0).UTC()
		got, ok := parseRetryAfter(value, now)
		if got < 0 || (!ok && got != 0) {
			t.Fatalf("parseRetryAfter(%q) = %v, %v", value, got, ok)
		}
		trimmed := strings.Trim(value, " \t")
		digits := trimmed != "" && strings.Trim(trimmed, "0123456789") == ""
		if digits {
			if !ok {
				t.Fatalf("parseRetryAfter(%q) refused a number of seconds", value)
			}
			want := time.Duration(math.MaxInt64)
			if seconds, err := strconv.ParseUint(trimmed, 10, 64); err == nil && seconds <= uint64(math.MaxInt64/int64(time.Second)) {
				want = time.Duration(seconds) * time.Second
			}
			if got != want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", value, got, want)
			}
		} else if ok && len(trimmed) > maxRetryAfterDate {
			t.Fatalf("parseRetryAfter parsed a date out of %d bytes", len(trimmed))
		}

		when := now.Add(time.Duration(delta) * time.Second)
		if when.Year() > 9999 {
			return
		}
		got, ok = parseRetryAfter(when.Format(http.TimeFormat), now)
		if want := max(time.Duration(delta)*time.Second, 0); !ok || got != want {
			t.Fatalf("parseRetryAfter(%s) at %s = %v, %v, want %v", when.Format(http.TimeFormat), now, got, ok, want)
		}
	})
}
