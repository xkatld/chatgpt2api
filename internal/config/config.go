package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Config struct {
	APIKey         string          `yaml:"api-key" json:"-"`
	DatabaseURL    string          `yaml:"-" json:"-"`
	BaseURL        string          `yaml:"base_url" json:"base_url"`
	Proxy          string          `yaml:"proxy" json:"proxy"`
	Proxies        []string        `yaml:"proxies" json:"proxies"`
	ProxyList      string          `yaml:"proxy_list" json:"proxy_list"`
	SystemPrompt   string          `yaml:"global_system_prompt" json:"global_system_prompt"`
	DefaultModel   string          `yaml:"default_upstream_model_name" json:"default_upstream_model_name"`
	ThinkingEffort string          `yaml:"default_thinking_effort" json:"default_thinking_effort"`
	ImageModelName string          `yaml:"image_model_name" json:"image_model_name"`
	Account        AccountSettings `yaml:"account" json:"account"`
	Image          ImageSettings   `yaml:"image" json:"image"`
	ProxyRuntime   ProxyRuntime    `yaml:"proxy_runtime" json:"proxy_runtime"`
	Cache          CacheSettings   `yaml:"chat_completion_cache" json:"chat_completion_cache"`
	MaxUploadMB    int64           `yaml:"max_upload_mb" json:"max_upload_mb"`
	Port           string          `yaml:"-" json:"-"`
	Version        string          `yaml:"-" json:"-"`
	path           string
	raw            map[string]any
	mu             sync.RWMutex
}

const DEFAULT_IMAGE_MODEL_NAME = "gpt-image-2.6"

type AccountSettings struct {
	RefreshIntervalMinute int  `yaml:"refresh_interval_minute" json:"refresh_interval_minute"`
	AutoRemoveInvalid     bool `yaml:"auto_remove_invalid" json:"auto_remove_invalid"`
	RemoveConversation    bool `yaml:"remove_conversation_after_result" json:"remove_conversation_after_result"`
}

type ImageSettings struct {
	RetentionDays          int          `yaml:"retention_days" json:"retention_days"`
	PollTimeoutSeconds     int          `yaml:"poll_timeout_secs" json:"poll_timeout_secs"`
	PollIntervalSeconds    float64      `yaml:"poll_interval_secs" json:"poll_interval_secs"`
	PollInitialWaitSeconds float64      `yaml:"poll_initial_wait_secs" json:"poll_initial_wait_secs"`
	AccountConcurrency     int          `yaml:"account_concurrency" json:"account_concurrency"`
	AccountRetries         int          `yaml:"account_retries" json:"account_retries"`
	CodexModel             string       `yaml:"codex_model" json:"codex_model"`
	CodexImageModel        string       `yaml:"codex_image_model" json:"codex_image_model"`
	MinFreeMB              int          `yaml:"min_free_mb" json:"min_free_mb"`
	Storage                ImageStorage `yaml:"storage" json:"storage"`
}

type ImageStorage struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	Mode           string `yaml:"mode" json:"mode"`
	WebDAVURL      string `yaml:"webdav_url" json:"webdav_url"`
	WebDAVUsername string `yaml:"webdav_username" json:"webdav_username"`
	WebDAVPassword string `yaml:"webdav_password" json:"-"`
	WebDAVRootPath string `yaml:"webdav_root_path" json:"webdav_root_path"`
	PublicBaseURL  string `yaml:"public_base_url" json:"public_base_url"`
}

type ProxyRuntime struct {
	Enabled          bool              `yaml:"enabled" json:"enabled"`
	EgressMode       string            `yaml:"egress_mode" json:"egress_mode"`
	ProxyURL         string            `yaml:"proxy_url" json:"proxy_url"`
	ProxyList        string            `yaml:"proxy_list" json:"proxy_list"`
	ResourceProxyURL string            `yaml:"resource_proxy_url" json:"resource_proxy_url"`
	Clearance        ClearanceSettings `yaml:"clearance" json:"clearance"`
}

type ClearanceSettings struct {
	Mode            string `yaml:"mode" json:"mode"`
	CFCookies       string `yaml:"cf_cookies" json:"-"`
	CFClearance     string `yaml:"cf_clearance" json:"-"`
	UserAgent       string `yaml:"user_agent" json:"user_agent"`
	FlareSolverrURL string `yaml:"flaresolverr_url" json:"flaresolverr_url"`
	TimeoutSeconds  int    `yaml:"timeout_sec" json:"timeout_sec"`
	RefreshInterval int    `yaml:"refresh_interval" json:"refresh_interval"`
}

type CacheSettings struct {
	Enabled                bool `yaml:"enabled" json:"enabled"`
	TTLSeconds             int  `yaml:"ttl_seconds" json:"ttl_seconds"`
	MaxEntries             int  `yaml:"max_entries" json:"max_entries"`
	StreamCache            bool `yaml:"stream_cache" json:"stream_cache"`
	NormalizeMessages      bool `yaml:"normalize_messages" json:"normalize_messages"`
	DropAdjacentDuplicates bool `yaml:"drop_adjacent_duplicates" json:"drop_adjacent_duplicates"`
	DropAssistantHistory   bool `yaml:"drop_assistant_history" json:"drop_assistant_history"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	cfg := &Config{DefaultModel: "auto", ThinkingEffort: "auto", MaxUploadMB: 50, Port: "3000", Version: "0.0.0", path: path, raw: map[string]any{}, Account: AccountSettings{RefreshIntervalMinute: 5, AutoRemoveInvalid: true}, Image: ImageSettings{RetentionDays: 30, PollTimeoutSeconds: 300, PollIntervalSeconds: 5, PollInitialWaitSeconds: 10, AccountConcurrency: 3, AccountRetries: 2, CodexModel: "gpt-6-luna", CodexImageModel: "gpt-image-2.6", MinFreeMB: 500, Storage: ImageStorage{Mode: "local", WebDAVRootPath: "chatgpt2api/images"}}, Cache: CacheSettings{Enabled: true, TTLSeconds: 60, MaxEntries: 256, StreamCache: true, NormalizeMessages: true, DropAdjacentDuplicates: true}}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, &cfg.raw); err != nil {
			return nil, err
		}
	}
	if len(cfg.Proxies) > 0 && cfg.ProxyList == "" {
		cfg.ProxyList = strings.Join(cfg.Proxies, "\n")
	} else if cfg.ProxyList != "" && len(cfg.Proxies) == 0 {
		lines := strings.Split(cfg.ProxyList, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				cfg.Proxies = append(cfg.Proxies, trimmed)
			}
		}
	}
	if cfg.ImageModelName == "" {
		cfg.ImageModelName = DEFAULT_IMAGE_MODEL_NAME
	}
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = "sqlite:///" + filepath.Join("data", "chatgpt2api.db")
	}
	if value := os.Getenv("CHATGPT2API_AUTH_KEY"); value != "" {
		cfg.APIKey = value
	}
	if value := os.Getenv("PORT"); value != "" {
		cfg.Port = value
	}
	if data, err := os.ReadFile("VERSION"); err == nil && strings.TrimSpace(string(data)) != "" {
		cfg.Version = strings.TrimSpace(string(data))
	}
	if cfg.APIKey == "" {
		return nil, errors.New("请先在 config.yaml 或 CHATGPT2API_AUTH_KEY 中设置管理员密钥")
	}
	return cfg, nil
}

func (c *Config) Public() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.public_locked()
}

func (c *Config) Update(values map[string]any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	image, _ := c.raw["image"].(map[string]any)
	if image == nil {
		image = map[string]any{}
	}
	account, _ := c.raw["account"].(map[string]any)
	if account == nil {
		account = map[string]any{}
	}
	image_fields := map[string]string{"image_retention_days": "retention_days", "image_poll_timeout_secs": "poll_timeout_secs", "image_poll_interval_secs": "poll_interval_secs", "image_poll_initial_wait_secs": "poll_initial_wait_secs", "image_account_concurrency": "account_concurrency", "image_account_retries": "account_retries", "image_codex_model": "codex_model", "image_codex_image_model": "codex_image_model", "image_min_free_mb": "min_free_mb"}
	account_fields := map[string]string{"refresh_account_interval_minute": "refresh_interval_minute", "auto_remove_invalid_accounts": "auto_remove_invalid", "remove_conversation_after_result": "remove_conversation_after_result"}
	for field, key := range image_fields {
		if value, ok := values[field]; ok {
			image[key] = value
			delete(values, field)
		}
	}
	for field, key := range account_fields {
		if value, ok := values[field]; ok {
			account[key] = value
			delete(values, field)
		}
	}
	if value, ok := values["image_storage"]; ok {
		if storage, ok := value.(map[string]any); ok {
			previous, _ := image["storage"].(map[string]any)
			if storage["webdav_password"] == nil || storage["webdav_password"] == "" {
				storage["webdav_password"] = previous["webdav_password"]
			}
			delete(storage, "has_webdav_password")
		}
		image["storage"] = value
		delete(values, "image_storage")
	}
	c.raw["image"], c.raw["account"] = image, account
	for key, value := range values {
		if key != "api-key" {
			if key == "proxy_runtime" {
				value = preserve_clearance(c.raw[key], value)
				value = preserve_proxy_list_map(c.raw[key], value)
			}
			if key == "proxy_list" {
				value = preserve_proxy_list(c.raw[key], value)
			}
			if key == "proxies" {
				value = preserve_proxies_slice(c.raw[key], value)
			}
			c.raw[key] = value
		}
	}
	sync_proxy_fields(c.raw, values)
	data, err := yaml.Marshal(c.raw)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.path, data, 0600); err != nil {
		return nil, err
	}
	var current Config
	if err := yaml.Unmarshal(data, &current); err != nil {
		return nil, err
	}
	c.Proxy, c.Proxies, c.ProxyList = current.Proxy, current.Proxies, current.ProxyList
	if len(c.Proxies) > 0 && c.ProxyList == "" {
		c.ProxyList = strings.Join(c.Proxies, "\n")
	} else if c.ProxyList != "" && len(c.Proxies) == 0 {
		lines := strings.Split(c.ProxyList, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				c.Proxies = append(c.Proxies, trimmed)
			}
		}
	}
	c.BaseURL, c.SystemPrompt, c.DefaultModel, c.ThinkingEffort = current.BaseURL, current.SystemPrompt, current.DefaultModel, current.ThinkingEffort
	c.Account, c.Image, c.ProxyRuntime, c.Cache = current.Account, current.Image, current.ProxyRuntime, current.Cache
	c.ImageModelName = current.ImageModelName
	if current.MaxUploadMB > 0 {
		c.MaxUploadMB = current.MaxUploadMB
	}
	if c.ImageModelName == "" {
		c.ImageModelName = DEFAULT_IMAGE_MODEL_NAME
	}
	return c.public_locked(), nil
}

func (c *Config) public_locked() map[string]any {
	result := public_copy(c.raw)
	delete(result, "image")
	delete(result, "account")
	result["image_model_name"] = c.ImageModelName
	result["image_retention_days"] = c.Image.RetentionDays
	result["image_poll_timeout_secs"] = c.Image.PollTimeoutSeconds
	result["image_poll_interval_secs"] = c.Image.PollIntervalSeconds
	result["image_poll_initial_wait_secs"] = c.Image.PollInitialWaitSeconds
	result["image_account_concurrency"] = c.Image.AccountConcurrency
	result["image_account_retries"] = c.Image.AccountRetries
	result["image_codex_model"] = c.Image.CodexModel
	result["image_codex_image_model"] = c.Image.CodexImageModel
	result["image_min_free_mb"] = c.Image.MinFreeMB
	result["refresh_account_interval_minute"] = c.Account.RefreshIntervalMinute
	result["auto_remove_invalid_accounts"] = c.Account.AutoRemoveInvalid
	result["remove_conversation_after_result"] = c.Account.RemoveConversation
	storage := map[string]any{}
	data, _ := yaml.Marshal(c.Image.Storage)
	yaml.Unmarshal(data, &storage)
	storage["has_webdav_password"] = c.Image.Storage.WebDAVPassword != ""
	delete(storage, "webdav_password")
	result["image_storage"] = storage
	mask_secrets(result)
	return result
}

func public_copy(values map[string]any) map[string]any {
	data, _ := yaml.Marshal(values)
	result := map[string]any{}
	yaml.Unmarshal(data, &result)
	delete(result, "api-key")
	return result
}

func mask_proxy_line(line string) string {
	raw_line := strings.TrimSpace(line)
	if raw_line == "" {
		return line
	}
	if strings.Contains(raw_line, "[REDACTED]") {
		return raw_line
	}
	scheme := ""
	rest := raw_line
	if strings.Contains(raw_line, "://") {
		parts := strings.SplitN(raw_line, "://", 2)
		scheme = parts[0] + "://"
		rest = parts[1]
	}
	idx1 := strings.Index(rest, ":")
	if idx1 > 0 {
		host := rest[:idx1]
		if !strings.Contains(host, "@") && !strings.Contains(host, "/") {
			rest1 := rest[idx1+1:]
			idx2 := strings.Index(rest1, ":")
			if idx2 > 0 {
				port_str := rest1[:idx2]
				if port, err := strconv.Atoi(port_str); err == nil && port > 0 && port <= 65535 {
					return scheme + host + ":" + port_str + ":[REDACTED]:[REDACTED]"
				}
			}
		}
	}
	last_colon := strings.LastIndex(rest, ":")
	if last_colon > 0 {
		port_str := rest[last_colon+1:]
		if port, err := strconv.Atoi(port_str); err == nil && port > 0 && port <= 65535 {
			left := rest[:last_colon]
			second_colon := strings.LastIndex(left, ":")
			if second_colon > 0 {
				host := left[second_colon+1:]
				if host != "" && !strings.Contains(host, "@") && !strings.Contains(host, "/") && !strings.Contains(host, ":") {
					return scheme + "[REDACTED]:[REDACTED]:" + host + ":" + port_str
				}
			}
		}
	}
	idx := strings.LastIndex(rest, "@")
	if idx != -1 {
		return scheme + "[REDACTED]@" + rest[idx+1:]
	}
	return raw_line
}

func mask_proxy_credentials(text string) string {
	lines := strings.Split(text, "\n")
	result := make([]string, 0, len(lines))
	for _, raw_line := range lines {
		trimmed := strings.TrimSpace(raw_line)
		if trimmed == "" {
			result = append(result, raw_line)
			continue
		}
		result = append(result, mask_proxy_line(raw_line))
	}
	return strings.Join(result, "\n")
}

var MaskProxyCredentials = mask_proxy_credentials

func mask_secrets(values map[string]any) {
	if runtime, ok := values["proxy_runtime"].(map[string]any); ok {
		if clearance, ok := runtime["clearance"].(map[string]any); ok {
			cookies, _ := clearance["cf_cookies"].(string)
			token, _ := clearance["cf_clearance"].(string)
			clearance["has_cf_cookies"] = cookies != ""
			clearance["has_cf_clearance"] = token != ""
			delete(clearance, "cf_cookies")
			delete(clearance, "cf_clearance")
		}
		if proxy_list, ok := runtime["proxy_list"].(string); ok && proxy_list != "" {
			runtime["proxy_list"] = mask_proxy_credentials(proxy_list)
		}
	}
	if proxy_list, ok := values["proxy_list"].(string); ok && proxy_list != "" {
		values["proxy_list"] = mask_proxy_credentials(proxy_list)
	}
	if proxies, ok := values["proxies"].([]any); ok {
		masked := make([]any, 0, len(proxies))
		for _, item := range proxies {
			if str, ok_str := item.(string); ok_str {
				masked = append(masked, mask_proxy_credentials(str))
			} else {
				masked = append(masked, item)
			}
		}
		values["proxies"] = masked
	}
}

func preserve_clearance(current any, incoming any) any {
	next, ok := incoming.(map[string]any)
	if !ok {
		return incoming
	}
	previous, _ := current.(map[string]any)
	previous_clearance, _ := previous["clearance"].(map[string]any)
	clearance, _ := next["clearance"].(map[string]any)
	if clearance == nil {
		return incoming
	}
	if clearance["has_cf_cookies"] == true && clearance["cf_cookies"] == nil {
		clearance["cf_cookies"] = previous_clearance["cf_cookies"]
	}
	if clearance["has_cf_clearance"] == true && clearance["cf_clearance"] == nil {
		clearance["cf_clearance"] = previous_clearance["cf_clearance"]
	}
	delete(clearance, "has_cf_cookies")
	delete(clearance, "has_cf_clearance")
	return next
}

func build_proxy_restore_maps(items []string) (map[string][]string, map[string][]string) {
	masked_queue := make(map[string][]string)
	host_queue := make(map[string][]string)
	for _, it := range items {
		trimmed := strings.TrimSpace(it)
		if trimmed == "" {
			continue
		}
		masked := mask_proxy_line(trimmed)
		masked_queue[masked] = append(masked_queue[masked], trimmed)
		hp := extract_host_port(trimmed)
		if hp != "" {
			host_queue[hp] = append(host_queue[hp], trimmed)
		}
	}
	return masked_queue, host_queue
}

func restore_proxy_item(item string, masked_queue map[string][]string, host_queue map[string][]string) string {
	trimmed := strings.TrimSpace(item)
	if !strings.Contains(trimmed, "[REDACTED]") {
		return item
	}
	if q, found := masked_queue[trimmed]; found && len(q) > 0 {
		orig := q[0]
		masked_queue[trimmed] = q[1:]
		hp := extract_host_port(orig)
		if hq, ok := host_queue[hp]; ok {
			for i, v := range hq {
				if v == orig {
					host_queue[hp] = append(hq[:i], hq[i+1:]...)
					break
				}
			}
		}
		return orig
	}
	hp := extract_host_port(trimmed)
	if hp != "" {
		if hq, found := host_queue[hp]; found && len(hq) > 0 {
			orig := hq[0]
			host_queue[hp] = hq[1:]
			masked := mask_proxy_line(orig)
			if q, ok := masked_queue[masked]; ok {
				for i, v := range q {
					if v == orig {
						masked_queue[masked] = append(q[:i], q[i+1:]...)
						break
					}
				}
			}
			return orig
		}
	}
	return item
}

func preserve_proxy_list(current any, incoming any) any {
	curr_str, ok_curr := current.(string)
	inc_str, ok_inc := incoming.(string)
	if !ok_curr || !ok_inc || curr_str == "" || inc_str == "" {
		return incoming
	}
	if !strings.Contains(inc_str, "[REDACTED]") {
		return incoming
	}
	curr_lines := strings.Split(curr_str, "\n")
	masked_queue, host_queue := build_proxy_restore_maps(curr_lines)
	inc_lines := strings.Split(inc_str, "\n")
	result := make([]string, 0, len(inc_lines))
	for _, l := range inc_lines {
		result = append(result, restore_proxy_item(l, masked_queue, host_queue))
	}
	return strings.Join(result, "\n")
}

func preserve_proxies_slice(current any, incoming any) any {
	inc_slice, ok_inc := incoming.([]any)
	if !ok_inc {
		return incoming
	}
	curr_slice, _ := current.([]any)
	curr_strings := make([]string, 0, len(curr_slice))
	for _, it := range curr_slice {
		if s, ok := it.(string); ok {
			curr_strings = append(curr_strings, s)
		}
	}
	masked_queue, host_queue := build_proxy_restore_maps(curr_strings)
	result := make([]any, 0, len(inc_slice))
	for _, item := range inc_slice {
		str, ok := item.(string)
		if ok && strings.Contains(str, "[REDACTED]") {
			result = append(result, restore_proxy_item(str, masked_queue, host_queue))
		} else {
			result = append(result, item)
		}
	}
	return result
}

func proxy_list_to_slice(text string) []any {
	lines := strings.Split(text, "\n")
	result := make([]any, 0, len(lines))
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func proxy_slice_to_list(slice []any) string {
	result := make([]string, 0, len(slice))
	for _, it := range slice {
		if s, ok := it.(string); ok {
			trimmed := strings.TrimSpace(s)
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
	}
	return strings.Join(result, "\n")
}

func sync_proxy_fields(raw map[string]any, values map[string]any) {
	_, has_list := values["proxy_list"]
	_, has_proxies := values["proxies"]
	if has_list && !has_proxies {
		if list_str, ok := raw["proxy_list"].(string); ok {
			raw["proxies"] = proxy_list_to_slice(list_str)
		}
	} else if has_proxies && !has_list {
		if slice, ok := raw["proxies"].([]any); ok {
			raw["proxy_list"] = proxy_slice_to_list(slice)
		}
	} else if has_list && has_proxies {
		if list_str, ok := raw["proxy_list"].(string); ok {
			raw["proxies"] = proxy_list_to_slice(list_str)
		}
	}
}

func extract_host_port(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		parts := strings.SplitN(raw, "://", 2)
		raw = parts[1]
	}
	idx1 := strings.Index(raw, ":")
	if idx1 > 0 {
		host := raw[:idx1]
		if !strings.Contains(host, "@") && !strings.Contains(host, "/") {
			rest1 := raw[idx1+1:]
			idx2 := strings.Index(rest1, ":")
			if idx2 > 0 {
				port_str := rest1[:idx2]
				if port, err := strconv.Atoi(port_str); err == nil && port > 0 && port <= 65535 {
					return host + ":" + port_str
				}
			}
		}
	}
	last_colon := strings.LastIndex(raw, ":")
	if last_colon > 0 {
		port_str := raw[last_colon+1:]
		if port, err := strconv.Atoi(port_str); err == nil && port > 0 && port <= 65535 {
			left := raw[:last_colon]
			second_colon := strings.LastIndex(left, ":")
			if second_colon > 0 {
				host := left[second_colon+1:]
				if host != "" && !strings.Contains(host, "@") && !strings.Contains(host, "/") && !strings.Contains(host, ":") {
					return host + ":" + port_str
				}
			}
		}
	}
	idx := strings.LastIndex(raw, "@")
	if idx != -1 {
		return raw[idx+1:]
	}
	parts := strings.Split(raw, ":")
	if len(parts) == 2 {
		if port, err := strconv.Atoi(parts[1]); err == nil && port > 0 && port <= 65535 {
			return raw
		}
	}
	return raw
}

func preserve_proxy_list_map(current any, incoming any) any {
	next, ok := incoming.(map[string]any)
	if !ok {
		return incoming
	}
	previous, _ := current.(map[string]any)
	if previous == nil {
		return incoming
	}
	if list, exists := next["proxy_list"]; exists {
		next["proxy_list"] = preserve_proxy_list(previous["proxy_list"], list)
	}
	return next
}
