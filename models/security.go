package models

import "time"

type SecuritySetting struct {
	ID          int       `json:"id"`
	Key         string    `json:"skey"`
	Value       string    `json:"svalue"`
	Description string    `json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type UpdateSecuritySettingsRequest struct {
	Fail2banMaxRetry *int    `json:"fail2ban_maxretry"`
	Fail2banFindTime *int    `json:"fail2ban_findtime"`
	Fail2banBanTime  *int    `json:"fail2ban_bantime"`
	AutoWhitelist    *bool   `json:"auto_whitelist_enabled"`
	WhitelistIPs     *string `json:"whitelist_ips"`
}

type CDNRealIPGroup struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	HeaderName  string    `json:"header_name"`
	IPRanges    string    `json:"ip_ranges"`
	Builtin     bool      `json:"builtin"`
	Enabled     bool      `json:"enabled"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type OperationLog struct {
	ID        int       `json:"id"`
	Operation string    `json:"operation"`
	Target    string    `json:"target"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}
