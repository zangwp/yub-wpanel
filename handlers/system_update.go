package handlers

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/zangwp/yub-wpanel/config"
	"github.com/zangwp/yub-wpanel/executor"
	"github.com/zangwp/yub-wpanel/i18n"
	"github.com/zangwp/yub-wpanel/models"

	"github.com/gin-gonic/gin"
)

type SystemUpdateHandler struct {
	Config *config.Config
}

type systemPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Repo    string `json:"repo"`
}

var sysPkgCache struct {
	mu       sync.Mutex
	expireAt time.Time
	pkgs     []systemPackage
}

func (h *SystemUpdateHandler) Check(c *gin.Context) {
	sysPkgCache.mu.Lock()
	if c.Query("fresh") != "1" && time.Now().Before(sysPkgCache.expireAt) {
		pkgs := sysPkgCache.pkgs
		sysPkgCache.mu.Unlock()
		c.JSON(http.StatusOK, models.SuccessResponse(systemUpdateCheckResponse(pkgs)))
		return
	}
	sysPkgCache.mu.Unlock()

	pkgs := getUpgradablePackages()

	sysPkgCache.mu.Lock()
	sysPkgCache.expireAt = time.Now().Add(5 * time.Minute)
	sysPkgCache.pkgs = pkgs
	sysPkgCache.mu.Unlock()

	c.JSON(http.StatusOK, models.SuccessResponse(systemUpdateCheckResponse(pkgs)))
}

type systemPackageCatalog struct {
	Distribution string
	BaseURL      string
}

func systemUpdateCheckResponse(pkgs []systemPackage) gin.H {
	catalog := readSystemPackageCatalog("/etc/os-release")
	return gin.H{
		"packages":            pkgs,
		"count":               len(pkgs),
		"distribution":        catalog.Distribution,
		"package_catalog_url": catalog.BaseURL,
	}
}

func readSystemPackageCatalog(path string) systemPackageCatalog {
	data, err := os.ReadFile(path)
	if err != nil {
		return systemPackageCatalog{}
	}
	return parseSystemPackageCatalog(string(data))
}

func parseSystemPackageCatalog(osRelease string) systemPackageCatalog {
	fields := make(map[string]string)
	for _, line := range strings.Split(osRelease, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	switch fields["ID"] + ":" + fields["VERSION_ID"] + ":" + fields["VERSION_CODENAME"] {
	case "debian:13:trixie":
		return systemPackageCatalog{Distribution: "Debian 13", BaseURL: "https://packages.debian.org/trixie/"}
	case "ubuntu:24.04:noble":
		return systemPackageCatalog{Distribution: "Ubuntu 24.04 LTS", BaseURL: "https://packages.ubuntu.com/noble/"}
	default:
		return systemPackageCatalog{}
	}
}

func (h *SystemUpdateHandler) Update(c *gin.Context) {
	status, err := executor.StartSystemPackageUpdate(h.Config)
	if err != nil {
		if strings.Contains(err.Error(), "正在执行") {
			c.JSON(http.StatusConflict, models.ErrorResponse(i18n.TE(c.Request, "settings.system_update_already_running")))
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(i18n.TE(c.Request, "settings.system_update_start_failed")))
		return
	}
	c.JSON(http.StatusAccepted, models.SuccessResponse(systemUpdateStatusResponse(c, status)))
}

func (h *SystemUpdateHandler) Status(c *gin.Context) {
	status := executor.ReconcileSystemPackageUpdateStatus(h.Config)
	if status.Status == "success" {
		sysPkgCache.mu.Lock()
		sysPkgCache.expireAt = time.Time{}
		sysPkgCache.pkgs = nil
		sysPkgCache.mu.Unlock()
		executor.ClearSystemUpdateAlertCache()
	}
	c.JSON(http.StatusOK, models.SuccessResponse(systemUpdateStatusResponse(c, status)))
}

func systemUpdateStatusResponse(c *gin.Context, status executor.SystemPackageUpdateStatus) gin.H {
	message := ""
	if status.MessageKey != "" {
		message = i18n.TE(c.Request, status.MessageKey)
	}
	return gin.H{
		"id":          status.ID,
		"status":      status.Status,
		"stage":       status.Stage,
		"message":     message,
		"message_key": status.MessageKey,
		"started_at":  status.StartedAt,
		"updated_at":  status.UpdatedAt,
	}
}

func getUpgradablePackages() []systemPackage {
	out, err := exec.Command("bash", "-c", "apt list --upgradable 2>/dev/null").Output()
	if err != nil {
		return []systemPackage{}
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var pkgs []systemPackage
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Listing...") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			continue
		}
		nameRepo := strings.SplitN(parts[0], "/", 2)
		name := nameRepo[0]
		repo := ""
		if len(nameRepo) > 1 {
			repo = nameRepo[1]
		}
		pkgs = append(pkgs, systemPackage{
			Name:    name,
			Version: parts[1],
			Repo:    repo,
		})
	}
	if pkgs == nil {
		pkgs = []systemPackage{}
	}
	return pkgs
}
