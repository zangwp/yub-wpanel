package main

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestReleaseWorkflowSupplyChainBoundaries(t *testing.T) {
	workflowBytes, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	workflow := string(workflowBytes)
	goModBytes, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if !strings.Contains(string(goModBytes), "\ntoolchain go1.26.8\n") {
		t.Fatal("go.mod must pin the reviewed Go 1.26.8 toolchain")
	}

	distributionBytes, err := os.ReadFile("config/distribution.go")
	if err != nil {
		t.Fatalf("read distribution metadata: %v", err)
	}
	publicKeyMatch := regexp.MustCompile(`ReleasePublicKeyHex\s*=\s*"([0-9a-f]{64})"`).FindSubmatch(distributionBytes)
	if len(publicKeyMatch) != 2 {
		t.Fatal("distribution metadata is missing one 32-byte Ed25519 public key")
	}
	publicKeyHex := string(publicKeyMatch[1])
	publicKey, err := hex.DecodeString(publicKeyHex)
	if err != nil || len(publicKey) != 32 {
		t.Fatalf("invalid configured Ed25519 public key: length=%d err=%v", len(publicKey), err)
	}
	spkiPrefix, err := hex.DecodeString("302a300506032b6570032100")
	if err != nil {
		t.Fatalf("decode Ed25519 SPKI prefix: %v", err)
	}
	spkiDER := append(spkiPrefix, publicKey...)
	publicKeySPKIBase64 := base64.StdEncoding.EncodeToString(spkiDER)
	if !strings.Contains(workflow, "RELEASE_PUBLIC_KEY_HEX: "+publicKeyHex) {
		t.Fatal("release workflow public key differs from the application update verifier")
	}
	if !strings.Contains(workflow, "RELEASE_PUBLIC_KEY_SPKI_B64: "+publicKeySPKIBase64) {
		t.Fatal("release workflow SPKI public key differs from the application update verifier")
	}

	for _, forbidden := range []string{
		"curl ",
		"wget ",
		"cdn.jsdelivr.net",
		"tailwindcss/releases/download",
		"softprops/action-gh-release",
		"--clobber",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("release workflow contains forbidden network build input or action %q", forbidden)
		}
	}

	actionRefPattern := regexp.MustCompile(`(?m)^\s*uses:\s+([^@\s]+)@([^\s#]+)`)
	actionRefs := actionRefPattern.FindAllStringSubmatch(workflow, -1)
	if len(actionRefs) == 0 {
		t.Fatal("release workflow contains no actions")
	}
	fullSHA := regexp.MustCompile(`^[0-9a-f]{40}$`)
	allowedActionRefs := map[string]string{
		"actions/checkout":          "3d3c42e5aac5ba805825da76410c181273ba90b1",
		"actions/setup-go":          "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		"actions/upload-artifact":   "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
		"actions/download-artifact": "3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c",
	}
	seenActions := make(map[string]bool, len(allowedActionRefs))
	for _, actionRef := range actionRefs {
		if !fullSHA.MatchString(actionRef[2]) {
			t.Errorf("action %q is not pinned to a full commit SHA: %q", actionRef[1], actionRef[2])
		}
		expectedSHA, allowed := allowedActionRefs[actionRef[1]]
		if !allowed {
			t.Errorf("release workflow uses an action outside the reviewed allowlist: %q", actionRef[1])
		} else if actionRef[2] != expectedSHA {
			t.Errorf("action %q changed from reviewed SHA %q to %q", actionRef[1], expectedSHA, actionRef[2])
		}
		seenActions[actionRef[1]] = true
	}
	for action := range allowedActionRefs {
		if !seenActions[action] {
			t.Errorf("release workflow is missing reviewed action %q", action)
		}
	}

	buildStart := strings.Index(workflow, "\n  build:")
	signStart := strings.Index(workflow, "\n  sign:")
	releaseStart := strings.Index(workflow, "\n  release:")
	if buildStart < 0 || signStart <= buildStart || releaseStart <= signStart {
		t.Fatal("release workflow must keep distinct build, sign, and release jobs")
	}
	buildJob := workflow[buildStart:signStart]
	signJob := workflow[signStart:releaseStart]
	releaseJob := workflow[releaseStart:]

	secretExpression := "secrets.RELEASE_SIGNING_KEY_B64"
	if strings.Contains(buildJob, secretExpression) || strings.Contains(releaseJob, secretExpression) {
		t.Fatal("release signing key escaped the isolated sign job")
	}
	if strings.Count(signJob, secretExpression) != 1 {
		t.Fatal("isolated sign job must receive the signing key in exactly one step")
	}
	if !strings.Contains(signJob, "environment:\n      name: release-signing") {
		t.Fatal("isolated sign job must use the protected release-signing environment")
	}
	for _, forbidden := range []string{
		"actions/checkout",
		"go run",
		"go test",
		"./yub-wpanel",
		"./install.sh",
		"./install-cn.sh",
		"bash install.sh",
		"bash install-cn.sh",
		"bash scripts/",
	} {
		if strings.Contains(signJob, forbidden) {
			t.Errorf("isolated sign job may not execute repository code: found %q", forbidden)
		}
		if strings.Contains(releaseJob, forbidden) {
			t.Errorf("isolated release job may not execute repository code: found %q", forbidden)
		}
	}
	shellExecutionPattern := regexp.MustCompile(`(?m)^\s*sh\s+(?:\./)?(?:install(?:-cn)?\.sh|scripts/)`)
	for jobName, job := range map[string]string{"sign": signJob, "release": releaseJob} {
		if shellExecutionPattern.MatchString(job) {
			t.Errorf("isolated %s job may not execute repository shell scripts", jobName)
		}
	}

	if strings.Count(workflow, "contents: write") != 1 || !strings.Contains(releaseJob, "contents: write") {
		t.Fatal("only the release job may receive contents: write")
	}
	for _, required := range []string{
		`=~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`,
		`test "$(git rev-parse refs/remotes/origin/main)" = "$GITHUB_SHA"`,
		"bash -n install.sh install-cn.sh",
		"go mod verify",
		`./dist/yub-wpanel --info --config "$preflight_dir/config.json"`,
		"sha256sum --check --strict",
		"openssl pkeyutl -sign -rawin",
		"openssl pkeyutl -verify -rawin -pubin",
		"RELEASE_PUBLIC_KEY_HEX",
		"RELEASE_PUBLIC_KEY_SPKI_B64",
		"yub-wpanel.sha256.sig",
		"install.sh.sha256.sig",
		"install-cn.sh.sha256.sig",
		"yub-wpanel-third-party-licenses.tar.gz.sha256.sig",
		`CGO_ENABLED=0 go list -deps -f '{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}' .`,
		`required_go_version="$(awk '$1 == "toolchain" { print $2; exit }' go.mod)"`,
		`test "$required_go_version" = 'go1.26.8'`,
		`INSTALLER_RELEASE_VERSION=\"$version\"`,
		`BOOTSTRAP_RELEASE_VERSION=\"$version\"`,
		`printf '%s\n' "$version" > "$license_root/RELEASE_VERSION"`,
		`install -m 0644 "$go_root/LICENSE" "$license_root/go-toolchain/LICENSE"`,
		"third_party/adminer-6.0.1/LICENSE-APACHE-2.0.txt",
		`license_file_list="$RUNNER_TEMP/yub-wpanel-license-files-$(printf '%03d' "$module_index")"`,
		`-print0 | LC_ALL=C sort -z > "$license_file_list"`,
		`done < "$license_file_list"`,
		`case "$module_dir_real" in`,
		`"$gomodcache_real"/*) ;;`,
		`No top-level license or notice found for $module_path@$module_version`,
		`test "$(find . -mindepth 1 -maxdepth 1 | wc -l)" -eq 12`,
		`gh api "/repos/$GH_REPO/git/ref/tags/$GITHUB_REF_NAME"`,
		`if [[ "$resolved_tag_commit" != "$GITHUB_SHA" ]]`,
		"refusing to replace published assets",
		"IMPORTANT for v2.0.0: do not use its built-in online updater",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release workflow is missing required hardening control %q", required)
		}
	}
	if marker := `printf '%s\n' "$version" > "$license_root/RELEASE_VERSION"`; strings.Count(workflow, marker) != 1 {
		t.Fatalf("release workflow must create exactly one license archive RELEASE_VERSION marker; count=%d", strings.Count(workflow, marker))
	}
	markerOffset := strings.Index(buildJob, `printf '%s\n' "$version" > "$license_root/RELEASE_VERSION"`)
	archiveOffset := strings.Index(buildJob, `gzip -n -9 > dist/yub-wpanel-third-party-licenses.tar.gz`)
	if markerOffset < 0 || archiveOffset < 0 || markerOffset >= archiveOffset {
		t.Fatalf("license RELEASE_VERSION marker must be created before archive assembly: marker=%d archive=%d", markerOffset, archiveOffset)
	}
}

func TestThirdPartyBrowserNoticesAreComplete(t *testing.T) {
	noticeBytes, err := os.ReadFile("THIRD_PARTY_NOTICES.md")
	if err != nil {
		t.Fatalf("read third-party notices: %v", err)
	}
	notice := string(noticeBytes)
	for _, required := range []string{
		"Alpine.js 3.13.7",
		"Copyright © 2019-2021 Caleb Porzio and contributors",
		"Chart.js 4.4.1",
		"Copyright (c) 2014-2022 Chart.js Contributors",
		"@kurkle/color 0.3.2",
		"Copyright (c) 2018-2021 Jukka Kurkela",
		"Tailwind CSS 3.4.19",
		"Copyright (c) Tailwind Labs, Inc.",
		"Adminer 6.0.1",
		"Copyright 2007 Jakub Vrana",
		"Go 1.26.8 runtime and standard library",
		"Copyright 2009 The Go Authors. All rights reserved.",
		"Permission is hereby granted, free of charge",
		"The above copyright notice and this permission notice shall be included",
		"THE SOFTWARE IS PROVIDED \"AS IS\"",
		"yub-wpanel-third-party-licenses.tar.gz",
	} {
		if !strings.Contains(notice, required) {
			t.Errorf("third-party notice is missing %q", required)
		}
	}
}
