package config

import "fmt"

var distributionIdentity = []struct {
	field string
	want  string
}{
	{field: "paths.cron_file", want: "/etc/cron.d/yub_wpanel_cron"},
	{field: "systemd.service_name", want: "yub-wpanel"},
	{field: "systemd.service_path", want: "/etc/systemd/system/yub-wpanel.service"},
	{field: "systemd.binary_path", want: "/usr/local/bin/yub-wpanel"},
}

func validateDistributionIdentity(cfg *Config) error {
	values := map[string]string{
		"paths.cron_file":      cfg.Paths.CronFile,
		"systemd.service_name": cfg.Systemd.ServiceName,
		"systemd.service_path": cfg.Systemd.ServicePath,
		"systemd.binary_path":  cfg.Systemd.BinaryPath,
	}
	for _, identity := range distributionIdentity {
		if values[identity.field] != identity.want {
			return fmt.Errorf("config_distribution_identity_mismatch: %s", identity.field)
		}
	}
	return nil
}
