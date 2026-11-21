package service

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/repository"
	chatgpt "github.com/basketikun/chatgpt2api/internal/upstream"
)

var default_models = []string{"auto", "gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-4", "gpt-3.5-turbo"}

type CatalogService struct {
	accounts   *AccountService
	proxy      *ProxyService
	config     *config.Config
	mu         sync.Mutex
	expires_at time.Time
	signature  string
	models     map[string]map[string]bool
}

func NewCatalogService(accounts *AccountService, proxy *ProxyService, config *config.Config) *CatalogService {
	return &CatalogService{accounts: accounts, proxy: proxy, config: config, models: map[string]map[string]bool{}}
}

func (s *CatalogService) List(ctx context.Context, refresh bool) ([]string, error) {
	counts, err := s.accounts.repo.AccountTypeCounts(ctx)
	if err != nil {
		return nil, err
	}
	types := make([]string, 0, len(counts))
	total := int64(0)
	for account_type, count := range counts {
		types = append(types, account_type)
		total += count
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, account_type := range types {
		parts = append(parts, account_type+":"+strconv.FormatInt(counts[account_type], 10))
	}
	signature := strings.Join(parts, ",")
	s.mu.Lock()
	defer s.mu.Unlock()
	if refresh || signature != s.signature || time.Now().After(s.expires_at) {
		accounts, err := s.accounts.repo.ActiveAccounts(ctx)
		if err != nil {
			return nil, err
		}
		groups := map[string][]repository.Account{}
		for _, account := range accounts {
			groups[account.Type] = append(groups[account.Type], account)
		}
		next := map[string]map[string]bool{}
		for account_type, items := range groups {
			models, ok := s.fetch(ctx, items)
			if ok {
				next[account_type] = models
			} else if previous := s.models[account_type]; previous != nil {
				next[account_type] = previous
			}
		}
		s.models, s.signature, s.expires_at = next, signature, time.Now().Add(300*time.Second)
	}
	union := map[string]bool{}
	for _, models := range s.models {
		for model := range models {
			union[model] = true
		}
	}
	if len(union) == 0 {
		for _, model := range default_models {
			union[model] = true
		}
	}
	for _, model := range config.PRESET_IMAGE_MODELS {
		union[model] = true
	}
	if s.config != nil {
		if s.config.ImageModelName != "" {
			union[s.config.ImageModelName] = true
		}
		if s.config.Image.CodexImageModel != "" {
			union[s.config.Image.CodexImageModel] = true
		}
	}
	result := make([]string, 0, len(union))
	for model := range union {
		result = append(result, model)
	}
	sort.Strings(result)
	return result, nil
}

func (s *CatalogService) AccountTypesForModel(ctx context.Context, model string) (map[string]bool, error) {
	if _, err := s.List(ctx, false); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string]bool{}
	for account_type, models := range s.models {
		if models[model] {
			result[account_type] = true
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func (s *CatalogService) fetch(ctx context.Context, accounts []repository.Account) (map[string]bool, bool) {
	attempted := map[string]bool{}
	for _, account := range accounts {
		token, err := s.accounts.RefreshToken(ctx, account.AccessToken, false)
		if err != nil || attempted[token] {
			continue
		}
		attempted[token] = true
		current, err := s.accounts.Account(ctx, token)
		if err != nil {
			continue
		}
		items, err := chatgpt.NewOpenAIBackendAPI(token, current.Proxy, s.proxy, s.config).WithAccountID(current.AccountID).ListModels(ctx)
		if err != nil {
			continue
		}
		models := map[string]bool{}
		for _, item := range items {
			id, _ := item["slug"].(string)
			if id == "" {
				id, _ = item["id"].(string)
			}
			if id != "" {
				models[id] = true
			}
		}
		return models, true
	}
	return nil, false
}
