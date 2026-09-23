package handlers

import "testing"

func TestParseSystemPackageCatalogUsesSupportedDistribution(t *testing.T) {
	tests := []struct {
		name         string
		osRelease    string
		distribution string
		baseURL      string
	}{
		{
			name:         "debian 13",
			osRelease:    "ID=debian\nVERSION_ID=\"13\"\nVERSION_CODENAME=trixie\n",
			distribution: "Debian 13",
			baseURL:      "https://packages.debian.org/trixie/",
		},
		{
			name:         "ubuntu 24.04",
			osRelease:    "ID=ubuntu\nVERSION_ID='24.04'\nVERSION_CODENAME=noble\n",
			distribution: "Ubuntu 24.04 LTS",
			baseURL:      "https://packages.ubuntu.com/noble/",
		},
		{
			name:      "unsupported release stays unlinked",
			osRelease: "ID=ubuntu\nVERSION_ID=26.04\nVERSION_CODENAME=resolute\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseSystemPackageCatalog(test.osRelease)
			if got.Distribution != test.distribution || got.BaseURL != test.baseURL {
				t.Fatalf("catalog = %+v, want distribution=%q base=%q", got, test.distribution, test.baseURL)
			}
		})
	}
}
