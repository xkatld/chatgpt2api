package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	netproxy "golang.org/x/net/proxy"
)

type Clearance struct {
	Cookies   map[string]string
	UserAgent string
	ExpiresAt time.Time
}

type ProxyService struct {
	config     *config.Config
	mu         sync.RWMutex
	clearance  map[string]Clearance
	client_mu  sync.Mutex
	clients    map[string]*http.Client
	tls_client map[string]tls_client.HttpClient
}

func NewProxyService(config *config.Config) *ProxyService {
	return &ProxyService{config: config, clearance: map[string]Clearance{}, clients: map[string]*http.Client{}, tls_client: map[string]tls_client.HttpClient{}}
}

func (s *ProxyService) Client(account_proxy string, resource bool, upstream bool) *http.Client {
	proxy_url, _ := s.proxy(account_proxy, resource, upstream)
	s.client_mu.Lock()
	defer s.client_mu.Unlock()
	if client := s.clients[proxy_url]; client != nil {
		return client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy_url != "" {
		parsed, err := url.Parse(normalize_proxy_url(proxy_url))
		if err == nil && (parsed.Scheme == "socks5" || parsed.Scheme == "socks5h") {
			var auth *netproxy.Auth
			if parsed.User != nil {
				password, _ := parsed.User.Password()
				auth = &netproxy.Auth{User: parsed.User.Username(), Password: password}
			}
			if dialer, err := netproxy.SOCKS5("tcp", parsed.Host, auth, netproxy.Direct); err == nil {
				transport.DialContext = func(_ context.Context, network string, address string) (net.Conn, error) {
					return dialer.Dial(network, address)
				}
			}
		} else if err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	client := &http.Client{Transport: transport, Timeout: 300 * time.Second}
	s.clients[proxy_url] = client
	return client
}

func (s *ProxyService) TLSClient(account_proxy string, resource bool, timeout_seconds ...int) (tls_client.HttpClient, error) {
	proxy_url, _ := s.proxy(account_proxy, resource, true)
	timeout := 300
	if len(timeout_seconds) > 0 {
		timeout = timeout_seconds[0]
	}
	key := proxy_url + "|" + strconv.Itoa(timeout)
	s.client_mu.Lock()
	defer s.client_mu.Unlock()
	if client := s.tls_client[key]; client != nil {
		return client, nil
	}
	options := []tls_client.HttpClientOption{tls_client.WithTimeoutSeconds(timeout), tls_client.WithClientProfile(profiles.Chrome_146), tls_client.WithRandomTLSExtensionOrder()}
	if proxy_url != "" {
		options = append(options, tls_client.WithProxyUrl(normalize_proxy_url(proxy_url)))
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err == nil {
		s.tls_client[key] = client
	}
	return client, err
}

func (s *ProxyService) UpstreamClearance(ctx context.Context, target_url string) (string, map[string]string) {
	if !s.config.ProxyRuntime.Enabled || s.config.ProxyRuntime.Clearance.Mode == "none" {
		return "", nil
	}
	bundle := s.CachedClearance("chatgpt.com")
	if time.Now().After(bundle.ExpiresAt) {
		bundle, _ = s.RefreshClearance(ctx, target_url)
	}
	return bundle.UserAgent, bundle.Cookies
}

func (s *ProxyService) RefreshClearance(ctx context.Context, target_url string) (Clearance, error) {
	settings := s.config.ProxyRuntime.Clearance
	if !s.config.ProxyRuntime.Enabled || settings.Mode == "none" {
		return Clearance{}, errors.New("clearance is disabled")
	}
	parsed, err := url.Parse(target_url)
	if err != nil {
		return Clearance{}, err
	}
	if settings.Mode == "manual" {
		cookies := parse_cookies(settings.CFCookies)
		if settings.CFClearance != "" {
			cookies["cf_clearance"] = settings.CFClearance
		}
		bundle := Clearance{Cookies: cookies, UserAgent: settings.UserAgent, ExpiresAt: time.Now().Add(time.Duration(settings.RefreshInterval) * time.Second)}
		s.mu.Lock()
		s.clearance[parsed.Hostname()] = bundle
		s.mu.Unlock()
		return bundle, nil
	}
	payload, _ := json.Marshal(map[string]any{"cmd": "request.get", "url": target_url, "maxTimeout": settings.TimeoutSeconds * 1000, "proxy": map[string]string{"url": s.config.ProxyRuntime.ProxyURL}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.FlareSolverrURL+"/v1", strings.NewReader(string(payload)))
	if err != nil {
		return Clearance{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := s.Client("", false, false).Do(req)
	if err != nil {
		return Clearance{}, err
	}
	defer response.Body.Close()
	var result struct {
		Status   string `json:"status"`
		Solution struct {
			UserAgent string `json:"userAgent"`
			Cookies   []struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Domain string `json:"domain"`
			} `json:"cookies"`
		} `json:"solution"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return Clearance{}, err
	}
	if result.Status != "ok" {
		return Clearance{}, errors.New("clearance refresh returned no bundle")
	}
	cookies := map[string]string{}
	for _, item := range result.Solution.Cookies {
		if item.Domain == "" || parsed.Hostname() == strings.TrimPrefix(item.Domain, ".") || strings.HasSuffix(parsed.Hostname(), item.Domain) {
			cookies[item.Name] = item.Value
		}
	}
	bundle := Clearance{Cookies: cookies, UserAgent: result.Solution.UserAgent, ExpiresAt: time.Now().Add(time.Duration(settings.RefreshInterval) * time.Second)}
	s.mu.Lock()
	s.clearance[parsed.Hostname()] = bundle
	s.mu.Unlock()
	return bundle, nil
}

func (s *ProxyService) CachedClearance(host string) Clearance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clearance[host]
}

func (s *ProxyService) Status() map[string]any {
	s.mu.RLock()
	hosts := make([]string, 0, len(s.clearance))
	for host := range s.clearance {
		hosts = append(hosts, host)
	}
	s.mu.RUnlock()
	runtime := s.config.ProxyRuntime
	proxy_url, proxy_source := s.proxy("", false, true)
	return map[string]any{"enabled": runtime.Enabled, "egress_mode": runtime.EgressMode, "proxy_source": proxy_source, "has_proxy": proxy_url != "", "clearance_enabled": runtime.Enabled && runtime.Clearance.Mode != "none", "clearance_mode": runtime.Clearance.Mode, "has_clearance_bundle": len(hosts) > 0, "cached_clearance_hosts": hosts}
}

func (s *ProxyService) resolve_redacted_proxy(target string) string {
	lines := strings.Split(target, "\n")
	selected := ""
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, ";") {
			selected = trimmed
			break
		}
	}
	if selected == "" {
		selected = strings.TrimSpace(target)
	}
	if !strings.Contains(selected, "[REDACTED]") {
		return selected
	}
	var candidates []string
	if s.config.ProxyRuntime.ProxyURL != "" {
		candidates = append(candidates, s.config.ProxyRuntime.ProxyURL)
	}
	if s.config.ProxyRuntime.ProxyList != "" {
		for _, line := range strings.Split(s.config.ProxyRuntime.ProxyList, "\n") {
			if t := strings.TrimSpace(line); t != "" {
				candidates = append(candidates, t)
			}
		}
	}
	for _, it := range s.config.Proxies {
		if t := strings.TrimSpace(it); t != "" {
			candidates = append(candidates, t)
		}
	}
	if s.config.ProxyList != "" {
		for _, line := range strings.Split(s.config.ProxyList, "\n") {
			if t := strings.TrimSpace(line); t != "" {
				candidates = append(candidates, t)
			}
		}
	}
	if s.config.Proxy != "" {
		candidates = append(candidates, s.config.Proxy)
	}
	for _, cand := range candidates {
		if redact_proxy_credentials(cand) == selected {
			return cand
		}
	}
	target_hp := extract_host_port(selected)
	if target_hp != "" {
		for _, cand := range candidates {
			if strings.EqualFold(extract_host_port(cand), target_hp) {
				return cand
			}
		}
	}
	return selected
}

func (s *ProxyService) Test(ctx context.Context, proxy_url string) map[string]any {
	started := time.Now()
	proxy_source := "input"
	proxy_url = strings.TrimSpace(proxy_url)
	if proxy_url == "" {
		proxy_url, proxy_source = s.proxy("", false, true)
	} else {
		if strings.Contains(proxy_url, "[REDACTED]") {
			proxy_url = s.resolve_redacted_proxy(proxy_url)
		}
		if list := parse_proxy_list(proxy_url); len(list) > 0 {
			proxy_url = list[0]
		} else {
			proxy_url = normalize_proxy_url(proxy_url)
		}
	}
	client, err := s.TLSClient(proxy_url, false)
	if err != nil {
		return map[string]any{"ok": false, "status": 0, "latency_ms": time.Since(started).Milliseconds(), "error": redact_proxy_credentials(err.Error()), "proxy_source": proxy_source, "has_proxy": proxy_url != "", "proxy_url": redact_proxy_credentials(proxy_url)}
	}
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, "https://chatgpt.com/api/auth/csrf", nil)
	if err != nil {
		return map[string]any{"ok": false, "status": 0, "latency_ms": time.Since(started).Milliseconds(), "error": redact_proxy_credentials(err.Error()), "proxy_source": proxy_source, "has_proxy": proxy_url != "", "proxy_url": redact_proxy_credentials(proxy_url)}
	}
	response, err := client.Do(req)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return map[string]any{"ok": false, "status": 0, "latency_ms": latency, "error": redact_proxy_credentials(err.Error()), "proxy_source": proxy_source, "has_proxy": proxy_url != "", "proxy_url": redact_proxy_credentials(proxy_url)}
	}
	defer response.Body.Close()
	return map[string]any{"ok": response.StatusCode < 500, "status": response.StatusCode, "latency_ms": latency, "error": nil, "proxy_source": proxy_source, "has_proxy": proxy_url != "", "proxy_url": redact_proxy_credentials(proxy_url)}
}

func (s *ProxyService) proxy(account_proxy string, resource bool, upstream bool) (string, string) {
	if account_proxy != "" {
		if list := parse_proxy_list(account_proxy); len(list) > 0 {
			return list[0], "account"
		}
		return normalize_proxy_url(account_proxy), "account"
	}
	if upstream && s.config.ProxyRuntime.Enabled {
		if resource && s.config.ProxyRuntime.ResourceProxyURL != "" {
			return normalize_proxy_url(s.config.ProxyRuntime.ResourceProxyURL), "runtime_resource"
		}
		if s.config.ProxyRuntime.EgressMode == "proxy_list" || (s.config.ProxyRuntime.ProxyList != "" && s.config.ProxyRuntime.EgressMode != "single_proxy" && s.config.ProxyRuntime.EgressMode != "direct") {
			proxies := parse_proxy_list(s.config.ProxyRuntime.ProxyList)
			if len(proxies) > 0 {
				return proxies[random_index(len(proxies))], "runtime_list"
			}
		}
		if s.config.ProxyRuntime.ProxyURL != "" {
			return normalize_proxy_url(s.config.ProxyRuntime.ProxyURL), "runtime"
		}
		if s.config.ProxyRuntime.ProxyList != "" {
			proxies := parse_proxy_list(s.config.ProxyRuntime.ProxyList)
			if len(proxies) > 0 {
				return proxies[random_index(len(proxies))], "runtime_list"
			}
		}
	}
	all_proxies := s.available_proxies()
	if len(all_proxies) > 0 {
		return all_proxies[random_index(len(all_proxies))], "list"
	}
	if s.config.Proxy != "" {
		return normalize_proxy_url(s.config.Proxy), "global"
	}
	return "", "direct"
}

func (s *ProxyService) available_proxies() []string {
	var candidates []string
	if len(s.config.Proxies) > 0 {
		for _, item := range s.config.Proxies {
			candidates = append(candidates, parse_proxy_list(item)...)
		}
	}
	if s.config.ProxyList != "" {
		candidates = append(candidates, parse_proxy_list(s.config.ProxyList)...)
	}
	seen := make(map[string]bool)
	var result []string
	for _, item := range candidates {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func parse_proxy_list(text string) []string {
	lines := strings.Split(text, "\n")
	result := make([]string, 0, len(lines))
	seen := make(map[string]bool)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		candidate := normalize_proxy_candidate(line)
		if candidate == "" {
			continue
		}
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Host == "" {
			continue
		}
		normalized := normalize_proxy_url(candidate)
		if !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	return result
}

var ParseProxyList = parse_proxy_list

func normalize_proxy_candidate(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	scheme := "http"
	rest := line
	if strings.Contains(line, "://") {
		parts := strings.SplitN(line, "://", 2)
		scheme = parts[0]
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
					user_pass := rest1[idx2+1:]
					user, pass, ok := strings.Cut(user_pass, ":")
					if ok {
						u := &url.URL{Scheme: scheme, User: url.UserPassword(user, pass), Host: net.JoinHostPort(host, port_str)}
						return u.String()
					}
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
					user_pass := left[:second_colon]
					user, pass, ok := strings.Cut(user_pass, ":")
					if ok {
						u := &url.URL{Scheme: scheme, User: url.UserPassword(user, pass), Host: net.JoinHostPort(host, port_str)}
						return u.String()
					}
				}
			}
		}
	}
	idx := strings.LastIndex(rest, "@")
	if idx != -1 {
		user_info := rest[:idx]
		host_port := rest[idx+1:]
		if strings.Contains(user_info, ":") {
			user, pass, _ := strings.Cut(user_info, ":")
			u := &url.URL{Scheme: scheme, User: url.UserPassword(user, pass), Host: host_port}
			return u.String()
		}
		u := &url.URL{Scheme: scheme, User: url.User(user_info), Host: host_port}
		return u.String()
	}
	parts := strings.Split(rest, ":")
	if len(parts) == 2 {
		if port, err := strconv.Atoi(parts[1]); err == nil && port > 0 && port <= 65535 {
			return scheme + "://" + rest
		}
	}
	return scheme + "://" + rest
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

func random_index(limit int) int {
	if limit <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}

func redact_proxy_credentials(value string) string {
	line := strings.TrimSpace(value)
	if line == "" {
		return value
	}
	if strings.Contains(line, "[REDACTED]") {
		return line
	}
	scheme := ""
	rest := line
	if strings.Contains(line, "://") {
		parts := strings.SplitN(line, "://", 2)
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
	return value
}

func normalize_proxy_url(value string) string {
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	if strings.HasPrefix(value, "socks://") {
		return "socks5h://" + strings.TrimPrefix(value, "socks://")
	}
	if strings.HasPrefix(value, "socks5://") {
		return "socks5h://" + strings.TrimPrefix(value, "socks5://")
	}
	return value
}

func parse_cookies(value string) map[string]string {
	result := map[string]string{}
	for _, item := range strings.Split(value, ";") {
		name, content, ok := strings.Cut(item, "=")
		if ok {
			result[name] = content
		}
	}
	return result
}
