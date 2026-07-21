package app

import (
	"strings"
	"testing"
)

// envMap builds a getenv function from a fixed map.
func envMap(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestResolveEgressRouteDirect proves that with no proxy configured the route is
// direct and reports both hostname/port and address-class guarantees.
func TestResolveEgressRouteDirect(t *testing.T) {
	t.Parallel()
	res, err := resolveEgressRoute(envMap(nil))
	if err != nil {
		t.Fatalf("resolveEgressRoute(empty) error = %v", err)
	}
	if res.Upstream {
		t.Error("empty environment resolved an upstream route")
	}
	if res.Route.String() != "direct" {
		t.Errorf("route = %q, want direct", res.Route.String())
	}
	if !res.Route.TargetGuarantee() || !res.Route.AddressGuarantee() {
		t.Errorf("direct route guarantees = target %v address %v, want both true", res.Route.TargetGuarantee(), res.Route.AddressGuarantee())
	}
	if res.Route.Fingerprint() == "" {
		t.Error("direct route has empty fingerprint")
	}
}

// TestResolveEgressRouteUpstreamRedactsCredentials proves an upstream proxy with
// embedded credentials produces an upstream route whose identity and fingerprint
// never contain the secret.
func TestResolveEgressRouteUpstreamRedactsCredentials(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t"
	res, err := resolveEgressRoute(envMap(map[string]string{
		"HTTPS_PROXY": "http://alice:" + secret + "@proxy.corp.example:3128",
	}))
	if err != nil {
		t.Fatalf("resolveEgressRoute(upstream) error = %v", err)
	}
	if !res.Upstream {
		t.Fatal("upstream proxy did not resolve an upstream route")
	}
	// The organization proxy resolves DNS, so only hostname/port is guaranteed.
	if !res.Route.TargetGuarantee() {
		t.Error("upstream route lost its target guarantee")
	}
	if res.Route.AddressGuarantee() {
		t.Error("upstream route falsely claims an address-class guarantee")
	}
	// Credentials never leak into the non-secret identity surfaces.
	for _, surface := range []string{res.Route.String(), res.Route.Fingerprint()} {
		if strings.Contains(surface, secret) || strings.Contains(surface, "alice") {
			t.Errorf("credential leaked into route surface %q", surface)
		}
	}
	if !strings.Contains(res.Route.String(), "proxy.corp.example") {
		t.Errorf("route identity %q lost its non-secret endpoint", res.Route.String())
	}
}

// TestResolveEgressRouteNoProxyWildcard proves NO_PROXY="*" with an upstream is an
// explicit, validated direct policy (not a silent bypass).
func TestResolveEgressRouteNoProxyWildcard(t *testing.T) {
	t.Parallel()
	res, err := resolveEgressRoute(envMap(map[string]string{
		"HTTPS_PROXY": "http://proxy.corp.example:3128",
		"NO_PROXY":    "*",
	}))
	if err != nil {
		t.Fatalf("resolveEgressRoute error = %v", err)
	}
	if res.Upstream || res.Route.String() != "direct" {
		t.Errorf("NO_PROXY=* did not select an explicit direct route: upstream=%v route=%q", res.Upstream, res.Route.String())
	}
}

// TestResolveEgressRouteNoSilentDirectFallback proves specific NO_PROXY entries
// alongside an upstream proxy fail closed rather than silently bypassing the
// upstream for those targets.
func TestResolveEgressRouteNoSilentDirectFallback(t *testing.T) {
	t.Parallel()
	_, err := resolveEgressRoute(envMap(map[string]string{
		"HTTPS_PROXY": "http://proxy.corp.example:3128",
		"NO_PROXY":    "internal.example,localhost",
	}))
	if err == nil {
		t.Fatal("upstream + specific NO_PROXY = nil error; want fail-closed (no silent direct fallback)")
	}
}

// TestResolveEgressRouteMalformed proves malformed proxy and NO_PROXY input fail
// closed, and that an error message never echoes a proxy URL that could carry a
// credential.
func TestResolveEgressRouteMalformed(t *testing.T) {
	t.Parallel()

	// A malformed upstream URL fails closed without echoing the raw URL.
	const secret = "leakme"
	_, err := resolveEgressRoute(envMap(map[string]string{
		"HTTPS_PROXY": "://bob:" + secret + "@:notaport",
	}))
	if err == nil {
		t.Fatal("malformed upstream = nil error; want fail-closed")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked credential: %v", err)
	}

	// A malformed NO_PROXY entry (embedded scheme/credentials) fails closed
	// without echoing the credential-bearing entry into the error.
	const noProxySecret = "npsecret"
	_, npErr := resolveEgressRoute(envMap(map[string]string{
		"NO_PROXY": "http://user:" + noProxySecret + "@host",
	}))
	if npErr == nil {
		t.Fatal("malformed NO_PROXY = nil error; want fail-closed")
	}
	if strings.Contains(npErr.Error(), noProxySecret) {
		t.Errorf("NO_PROXY error leaked credential: %v", npErr)
	}
}

// TestResolveEgressRouteNilGetter fails closed when no environment getter is
// supplied.
func TestResolveEgressRouteNilGetter(t *testing.T) {
	t.Parallel()
	if _, err := resolveEgressRoute(nil); err == nil {
		t.Fatal("nil getenv = nil error; want fail-closed")
	}
}
