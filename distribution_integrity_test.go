package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDistributionLicenseAndNoticeAreComplete(t *testing.T) {
	license, err := os.ReadFile("LICENSE")
	if err != nil {
		t.Fatalf("read LICENSE: %v", err)
	}
	licenseText := string(license)
	if len(license) < 34000 {
		t.Fatalf("LICENSE is unexpectedly short: %d bytes", len(license))
	}
	for _, required := range []string{
		"GNU GENERAL PUBLIC LICENSE",
		"17. Interpretation of Sections 15 and 16.",
		"END OF TERMS AND CONDITIONS",
		"How to Apply These Terms to Your New Programs",
	} {
		if !strings.Contains(licenseText, required) {
			t.Errorf("LICENSE is missing %q", required)
		}
	}

	notice, err := os.ReadFile("NOTICE.md")
	if err != nil {
		t.Fatalf("read NOTICE.md: %v", err)
	}
	noticeText := string(notice)
	for _, required := range []string{"YUB WPanel", "GNU General Public License", "GPL-3.0-only", "zangwp", "Copyright (C) 2026"} {
		if !strings.Contains(noticeText, required) {
			t.Errorf("NOTICE.md is missing %q", required)
		}
	}

	thirdParty, err := os.ReadFile("THIRD_PARTY_NOTICES.md")
	if err != nil {
		t.Fatalf("read THIRD_PARTY_NOTICES.md: %v", err)
	}
	thirdPartyText := string(thirdParty)
	for _, required := range []string{"YUB WPanel Open Source License", "GPL-3.0-only", "YUB WPanel project notice"} {
		if !strings.Contains(thirdPartyText, required) {
			t.Errorf("THIRD_PARTY_NOTICES.md is missing %q", required)
		}
	}
}

func TestAllWorkflowActionsArePinnedToCommitSHAs(t *testing.T) {
	workflowPaths, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(workflowPaths) == 0 {
		t.Fatal("no GitHub Actions workflows found")
	}
	actionRefPattern := regexp.MustCompile(`(?m)^\s*uses:\s+[^@\s]+@([^\s#]+)`)
	fullSHA := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, path := range workflowPaths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		refs := actionRefPattern.FindAllStringSubmatch(string(content), -1)
		if len(refs) == 0 {
			t.Errorf("%s contains no pinned action references", path)
		}
		for _, ref := range refs {
			if !fullSHA.MatchString(ref[1]) {
				t.Errorf("%s contains an action not pinned to a full commit SHA: %q", path, ref[1])
			}
		}
	}
}

func TestLegacyDistributionNamesDoNotReturn(t *testing.T) {
	legacyNames := []string{
		"naiba" + "biji",
		"wp" + "-panel",
		"wp" + "_panel",
		"wp" + " panel",
		"wpp" + "anel",
		"wpp" + "_optimizer",
	}
	textExtensions := map[string]bool{
		".css": true, ".go": true, ".html": true, ".js": true,
		".json": true, ".md": true, ".php": true, ".sh": true,
		".txt": true, ".yaml": true, ".yml": true,
	}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			base := filepath.Base(path)
			if path == ".git" || path == "scratch" || strings.HasPrefix(base, ".codex-go-") {
				return filepath.SkipDir
			}
			return nil
		}
		if !textExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > 2<<20 {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lowerContent := strings.ToLower(string(content))
		for _, legacyName := range legacyNames {
			if strings.Contains(lowerContent, legacyName) {
				t.Errorf("legacy distribution name %q found in %s", legacyName, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFixedVersionRepairUpgradeBridgesRemainExplicit(t *testing.T) {
	checks := map[string][]string{
		"README.md": {
			"v2.0.0 到 v2.0.1",
			"v2.0.1 到 v2.0.2",
			"不要使用只替换二进制的面板在线更新器",
		},
		"README.en.md": {
			"v2.0.0 to v2.0.1",
			"v2.0.1 to v2.0.2",
			"do not use the panel's binary-only online updater",
		},
		"docs/upgrade-compatibility.md": {
			"v2.0.1 到 v2.0.2",
			"v2.0.0 到 v2.0.1 的一次性安全升级",
			"install.sh.sha256.sig",
			"yub-wpanel.sha256.sig",
			"yub-wpanel-third-party-licenses.tar.gz.sha256.sig",
			"/usr/share/doc/yub-wpanel/RELEASE_VERSION",
			"不要使用 `latest`",
		},
		"docs/verified-install.md": {
			"v2.0.1 升级到 v2.0.2",
			"version='v2.0.2'",
			"yub-wpanel-third-party-licenses.tar.gz.sha256.sig",
			"binary-only online updater",
		},
	}
	for path, required := range checks {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, phrase := range required {
			if !strings.Contains(string(content), phrase) {
				t.Errorf("%s is missing the fixed-version repair upgrade warning %q", path, phrase)
			}
		}
	}
}
