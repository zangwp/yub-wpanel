package main

import (
	"os"
	"strings"
	"testing"
)

func TestCloudflareInstallEntryVerifiesPinnedRelease(t *testing.T) {
	workerBytes, err := os.ReadFile("deploy/cloudflare/wpanel-entry.js")
	if err != nil {
		t.Fatalf("read Cloudflare Worker: %v", err)
	}
	worker := string(workerBytes)
	for _, required := range []string{
		`const RELEASE_VERSION = "v2.1.1"`,
		`https://github.com/zangwp/yub-wpanel/releases/download/${RELEASE_VERSION}`,
		`const BOOTSTRAP_NAME = "bootstrap.sh"`,
		`MCowBQYDK2VwAyEAc1EJlyDurxR/SJS8MTpUVsAbvSmtfUAatoabx/f5KvU=`,
		`{ name: "Ed25519" }`,
		`crypto.subtle.verify(`,
		`crypto.subtle.digest("SHA-256", script)`,
		`/^([0-9a-f]{64})  bootstrap\.sh\n?$/`,
		`BOOTSTRAP_RELEASE_VERSION="${RELEASE_VERSION}"`,
		`BOOTSTRAP_DEFAULT_PREFER_CN=0`,
		`url.pathname !== "/install"`,
		`status: 503`,
	} {
		if !strings.Contains(worker, required) {
			t.Errorf("Cloudflare install entry missing security control %q", required)
		}
	}
	for _, forbidden := range []string{
		"raw.githubusercontent.com/zangwp/yub-wpanel/main",
		"cdn.jsdelivr.net/gh/zangwp/yub-wpanel@main",
	} {
		if strings.Contains(worker, forbidden) {
			t.Errorf("Cloudflare install entry contains mutable source %q", forbidden)
		}
	}
	verify := strings.Index(worker, "const signatureValid = await crypto.subtle.verify(")
	hash := strings.Index(worker, `const actualDigest = hex(new Uint8Array(await crypto.subtle.digest("SHA-256", script)))`)
	serve := strings.Index(worker, "return script;")
	if verify < 0 || hash < 0 || serve < 0 || !(verify < hash && hash < serve) {
		t.Fatalf("Cloudflare bootstrap verification order is invalid: signature=%d hash=%d serve=%d", verify, hash, serve)
	}
}

func TestCloudflareInstallEntryUsesExactCustomDomain(t *testing.T) {
	configBytes, err := os.ReadFile("deploy/cloudflare/wrangler.jsonc")
	if err != nil {
		t.Fatalf("read Wrangler config: %v", err)
	}
	config := string(configBytes)
	for _, required := range []string{
		`"name": "yub-wpanel-entry"`,
		`"workers_dev": false`,
		`"pattern": "wpanel.zangyubin.top"`,
		`"custom_domain": true`,
	} {
		if !strings.Contains(config, required) {
			t.Errorf("Wrangler config missing %q", required)
		}
	}
}
