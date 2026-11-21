package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/protocol"
	chatgpt "github.com/basketikun/chatgpt2api/internal/upstream"
	"github.com/disintegration/imaging"
)

type AIService struct {
	accounts *AccountService
	catalog  *CatalogService
	images   *ImageService
	logs     *LogService
	proxy    *ProxyService
	config   *config.Config
	cache_mu sync.Mutex
	cache    map[string]cachedProtocol
}
type cachedProtocol struct {
	value      json.RawMessage
	expires_at time.Time
}

type ImageRequest struct {
	Prompt         string
	Model          string
	Count          int
	Size           string
	Quality        string
	ResponseFormat string
	BaseURL        string
	Stream         bool
	HistoryID      *int64
	TurnID         *int64
	Images         [][]byte
	ImageNames     []string
	ImageMIMEs     []string
	Masks          [][]byte
	MaskNames      []string
	MaskMIMEs      []string
}
type ImageResult struct {
	B64           string
	URL           string
	RevisedPrompt string
}
type ImageError struct {
	Err            error
	Status         int
	Type           string
	Code           string
	Param          any
	ConversationID string
	AccountEmail   string
}

func (e *ImageError) Error() string { return e.Err.Error() }

func NewAIService(accounts *AccountService, catalog *CatalogService, images *ImageService, logs *LogService, proxy *ProxyService, config *config.Config) *AIService {
	return &AIService{accounts: accounts, catalog: catalog, images: images, logs: logs, proxy: proxy, config: config, cache: map[string]cachedProtocol{}}
}

func (s *AIService) Text(ctx context.Context, model_name string, messages []map[string]any, effort string, search bool, delta func(string) error, call_log *CallLog) (string, error) {
	if model_name == "" {
		model_name = s.config.DefaultModel
	}
	call_log.Update(map[string]any{"model": model_name})
	backend_model := model_name
	if search {
		backend_model = s.config.DefaultModel
	}
	output := strings.Builder{}
	var err error
	account_types := map[string]bool(nil)
	if backend_model != "auto" {
		account_types, err = s.catalog.AccountTypesForModel(ctx, backend_model)
		if err != nil {
			return "", err
		}
	}
	if plans := call_log.identity.Plans; len(plans) > 0 {
		allowed := map[string]bool{}
		for _, plan := range plans {
			allowed[plan] = account_types == nil || account_types[plan]
		}
		account_types = allowed
	}
	attempted, conversation_id, email := map[string]bool{}, "", ""
	for {
		token, token_err := s.accounts.TextTokenForModel(ctx, attempted, account_types, backend_model)
		if token_err != nil {
			err = token_err
			break
		}
		attempted[token] = true
		account, account_err := s.accounts.Account(ctx, token)
		if account_err != nil {
			err = account_err
			break
		}
		email = ""
		if account.Email != nil {
			email = *account.Email
		}
		call_log.Update(map[string]any{"account_email": email})
		call_log.Add("已选择上游账号", map[string]any{"account_email": email})
		client := chatgpt.NewOpenAIBackendAPI(token, account.Proxy, s.proxy, s.config).WithAccountID(account.AccountID)
		emitted := false
		call_log.Add("开始准备上游请求", map[string]any{"model": backend_model, "thinking_effort": effort, "search": search})
		raw, clean := "", ""
		conversation_id, err = client.StartConversation(ctx, messages, backend_model, effort, nil, search, func(value string) error {
			emitted = true
			if search {
				raw += value
				next := protocol.CleanSearchText(raw)
				if !strings.HasPrefix(next, clean) {
					return nil
				}
				value = strings.TrimPrefix(next, clean)
				clean = next
				if value == "" {
					return nil
				}
			}
			output.WriteString(value)
			if delta != nil {
				return delta(value)
			}
			return nil
		})
		if err == nil {
			s.accounts.MarkTextUsed(ctx, token)
			if s.config.Account.RemoveConversation && conversation_id != "" {
				client.DeleteConversation(context.Background(), conversation_id)
			}
			break
		}
		s.accounts.RecordTextError(ctx, token, err)
		call_log.Add("上游调用失败", map[string]any{"error": err.Error(), "response_started": emitted})
		if emitted || !invalidTokenError(err) {
			break
		}
		refreshed, refresh_err := s.accounts.RefreshToken(ctx, token, true)
		if refresh_err == nil && refreshed != "" && refreshed != token && !attempted[refreshed] {
			continue
		}
		s.accounts.RemoveInvalid(ctx, token)
	}
	if err == nil {
		call_log.Update(map[string]any{"conversation_id": conversation_id})
		call_log.Add("上游 SSE 已完成", map[string]any{"conversation_id": conversation_id, "output_chars": output.Len()})
	}
	if err != nil {
		call_log.Save("调用失败", "failed", map[string]any{"error": err.Error(), "account_email": email, "conversation_id": conversation_id})
	} else {
		call_log.Save("调用完成", "success", map[string]any{"account_email": email, "conversation_id": conversation_id})
	}
	return output.String(), err
}

func (s *AIService) PrepareMessages(messages []map[string]any) []map[string]any {
	if s.config.SystemPrompt != "" {
		messages = append([]map[string]any{{"role": "system", "content": s.config.SystemPrompt}}, messages...)
	}
	if !s.config.Cache.NormalizeMessages {
		return messages
	}
	result := make([]map[string]any, 0, len(messages))
	previous := ""
	for _, message := range messages {
		if s.config.Cache.DropAssistantHistory && message["role"] == "assistant" {
			continue
		}
		data, _ := json.Marshal(message)
		signature := string(data)
		if s.config.Cache.DropAdjacentDuplicates && signature == previous {
			continue
		}
		result = append(result, message)
		previous = signature
	}
	return result
}

func (s *AIService) ProtocolCacheKey(body map[string]any, messages []map[string]any, stream bool) string {
	keys := []string{"frequency_penalty", "max_completion_tokens", "max_tokens", "metadata", "model", "presence_penalty", "reasoning_effort", "response_format", "seed", "stop", "stream_options", "temperature", "thinking_effort", "tool_choice", "tools", "top_p", "user", "reasoning"}
	payload := map[string]any{"messages": messages, "stream": stream}
	for _, key := range keys {
		if value, ok := body[key]; ok {
			payload[key] = value
		}
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *AIService) CachedProtocol(key string, stream bool, target any) bool {
	if !s.config.Cache.Enabled || s.config.Cache.TTLSeconds <= 0 || stream && !s.config.Cache.StreamCache {
		return false
	}
	s.cache_mu.Lock()
	defer s.cache_mu.Unlock()
	item, ok := s.cache[key]
	if !ok || time.Now().After(item.expires_at) {
		delete(s.cache, key)
		return false
	}
	return json.Unmarshal(item.value, target) == nil
}

func (s *AIService) StoreProtocol(key string, stream bool, value any) {
	if !s.config.Cache.Enabled || s.config.Cache.TTLSeconds <= 0 || stream && !s.config.Cache.StreamCache {
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	s.cache_mu.Lock()
	defer s.cache_mu.Unlock()
	now := time.Now()
	for current, item := range s.cache {
		if now.After(item.expires_at) {
			delete(s.cache, current)
		}
	}
	for len(s.cache) >= max(1, s.config.Cache.MaxEntries) {
		oldest := ""
		for current, item := range s.cache {
			if oldest == "" || item.expires_at.Before(s.cache[oldest].expires_at) {
				oldest = current
			}
		}
		delete(s.cache, oldest)
	}
	s.cache[key] = cachedProtocol{value: data, expires_at: now.Add(time.Duration(s.config.Cache.TTLSeconds) * time.Second)}
}

func (s *AIService) generateImage(ctx context.Context, request ImageRequest, call_log *CallLog, index int) (ImageResult, bool, error) {
	token, err := s.accounts.ImageToken(ctx, call_log.identity.Routes, call_log.identity.Plans)
	if err != nil {
		return ImageResult{}, false, newImageError(err)
	}
	success := false
	defer func() { s.accounts.ReleaseImageToken(context.Background(), token, success) }()
	account, _ := s.accounts.Account(ctx, token)
	codex := account.IsCodex && slices.Contains(call_log.identity.Routes, "codex")
	email := ""
	if account.Email != nil {
		email = *account.Email
	}
	call_log.Update(map[string]any{"account_email": email})
	call_log.Add("已选择上游账号", map[string]any{"account_email": email, "position": index})
	var data []byte
	conversation_id := ""
	if codex {
		encoded := make([]string, 0, len(request.Images))
		for _, image := range request.Images {
			encoded = append(encoded, base64.StdEncoding.EncodeToString(image))
		}
		codex_image_model := request.Model
		if codex_image_model == "" {
			codex_image_model = s.config.Image.CodexImageModel
		}
		if codex_image_model == "" {
			codex_image_model = s.config.ImageModelName
		}
		call_log.Add("请求 Codex 图片生成", map[string]any{"position": index, "model": codex_image_model})
		data, err = chatgpt.NewCodexAPI(token, chatgpt.CodexBaseURL, account.Proxy, s.proxy).WithAccountID(account.AccountID).StartImageGen(ctx, request.Prompt, s.config.Image.CodexModel, codex_image_model, encoded, defaultString(request.Size, "2048x2048"), defaultString(request.Quality, "auto"))
		if err == nil {
			call_log.Add("Codex 图片生成完成", map[string]any{"position": index})
		}
	} else {
		client := chatgpt.NewOpenAIBackendAPI(token, account.Proxy, s.proxy, s.config).WithAccountID(account.AccountID)
		call_log.Add("请求上游对话", map[string]any{"position": index, "model": s.config.DefaultModel, "image_model": request.Model})
		content := []any{map[string]any{"type": "text", "text": imagePrompt(request.Prompt, request.Size, request.Quality)}}
		for image_index, image := range request.Images {
			content = append(content, map[string]any{"type": "image", "data": image, "name": imageValue(request.ImageNames, image_index, "image.png"), "mime": imageValue(request.ImageMIMEs, image_index, "image/png")})
		}
		message := map[string]any{"role": "user", "content": content}
		if settings := imageGenerationSettings(request.Size); settings != nil {
			message["image_generation_settings"] = settings
		}
		policy_text := strings.Builder{}
		conversation_id, err = client.StartConversation(ctx, []map[string]any{message}, s.config.DefaultModel, "", []string{"picture_v2"}, false, func(value string) error {
			policy_text.WriteString(value)
			return nil
		})
		if err == nil && policy_text.Len() > 0 {
			var ids []string
			if ids, err = client.ImageIDs(ctx, conversation_id); err == nil && len(ids) == 0 {
				err = &ImageError{Err: errors.New(policy_text.String()), Status: 400, Type: "invalid_request_error", Code: "content_policy_violation"}
			}
		}
		if err == nil {
			call_log.Update(map[string]any{"conversation_id": conversation_id})
			call_log.Add("上游对话已结束", map[string]any{"conversation_id": conversation_id, "position": index})
			call_log.Add("开始轮询上游图片", map[string]any{"conversation_id": conversation_id, "position": index})
			var ids []string
			ids, err = client.PollImages(ctx, conversation_id)
			if err == nil {
				call_log.Add("获取到上游图片", map[string]any{"conversation_id": conversation_id, "image_ids": ids, "position": index})
				data, err = client.Download(ctx, conversation_id, ids[0])
			}
		}
	}
	if err != nil {
		image_err := newImageError(err)
		image_err.ConversationID, image_err.AccountEmail = conversation_id, email
		return ImageResult{}, image_err.Code != "content_policy_violation", image_err
	}
	stored, err := s.images.Save(ctx, data, request.BaseURL)
	if err != nil {
		image_err := newImageError(err)
		image_err.ConversationID, image_err.AccountEmail = conversation_id, email
		return ImageResult{}, true, image_err
	}
	result := ImageResult{B64: base64.StdEncoding.EncodeToString(data), URL: stored.URL, RevisedPrompt: request.Prompt}
	call_log.Add("图片已保存", map[string]any{"position": index, "url": stored.URL})
	success = true
	return result, false, nil
}

func (s *AIService) Images(ctx context.Context, request ImageRequest, call_log *CallLog) ([]ImageResult, error) {
	if request.Model == "" || request.Model == "auto" {
		request.Model = s.config.ImageModelName
		if request.Model == "" {
			request.Model = config.DEFAULT_IMAGE_MODEL_NAME
		}
	}
	if !s.IsImageModel(request.Model) {
		err := newImageError(fmt.Errorf("unsupported image model: %s", request.Model))
		call_log.Save("调用失败", "failed", map[string]any{"error": err.Error()})
		return nil, err
	}
	call_log.Update(map[string]any{"model": request.Model})
	if request.Count < 1 || request.Count > 4 {
		err := errors.New("n must be between 1 and 4")
		call_log.Save("调用失败", "failed", map[string]any{"error": err.Error()})
		return nil, err
	}
	if s.config.BaseURL != "" {
		request.BaseURL = s.config.BaseURL
	}
	request.Images = applyMasks(request.Images, request.Masks)
	results := make([]ImageResult, request.Count)
	errs := make(chan error, request.Count)
	var wait sync.WaitGroup
	for index := range request.Count {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			for attempt := 0; ; attempt++ {
				result, retryable, err := s.generateImage(ctx, request, call_log, index)
				if err == nil {
					results[index] = result
					return
				}
				if !retryable || attempt >= s.config.Image.AccountRetries || ctx.Err() != nil {
					errs <- err
					return
				}
				call_log.Add("更换账号重试", map[string]any{"position": index, "attempt": attempt + 1, "error": err.Error()})
			}
		}(index)
	}
	wait.Wait()
	close(errs)
	if err := <-errs; err != nil {
		detail := map[string]any{"error": err.Error()}
		var image_err *ImageError
		if errors.As(err, &image_err) {
			detail["account_email"], detail["conversation_id"] = image_err.AccountEmail, image_err.ConversationID
		}
		call_log.Save("调用失败", "failed", detail)
		return nil, err
	}
	urls := make([]string, 0, len(results))
	for _, result := range results {
		urls = append(urls, result.URL)
	}
	call_log.Save("调用完成", "success", map[string]any{"urls": urls})
	return results, nil
}

func (s *AIService) IsImageModel(model string) bool {
	if model == "" {
		return false
	}
	if slices.Contains(config.PRESET_IMAGE_MODELS, model) {
		return true
	}
	if s.config != nil {
		if s.config.ImageModelName != "" && model == s.config.ImageModelName {
			return true
		}
		if s.config.Image.CodexImageModel != "" && model == s.config.Image.CodexImageModel {
			return true
		}
	}
	return false
}

func newImageError(err error) *ImageError {
	var image_err *ImageError
	if errors.As(err, &image_err) {
		return image_err
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "图片额度") || strings.Contains(message, "no available") && strings.Contains(message, "image quota") {
		return &ImageError{Err: errors.New("no available image quota"), Status: 429, Type: "insufficient_quota", Code: "insufficient_quota"}
	}
	if strings.Contains(message, "content policy") || strings.Contains(message, "content_policy") || strings.Contains(message, "safety policy") {
		return &ImageError{Err: err, Status: 400, Type: "invalid_request_error", Code: "content_policy_violation"}
	}
	return &ImageError{Err: err, Status: 502, Type: "server_error", Code: "upstream_error"}
}

func imageValue(values []string, index int, fallback string) string {
	if index < len(values) && values[index] != "" {
		return values[index]
	}
	return fallback
}

func applyMasks(images [][]byte, masks [][]byte) [][]byte {
	if len(masks) == 0 {
		return images
	}
	result := make([][]byte, 0, len(images))
	for index, data := range images {
		source, err := imaging.Decode(bytes.NewReader(data))
		if err != nil {
			result = append(result, data)
			continue
		}
		mask_data := masks[min(index, len(masks)-1)]
		mask, err := imaging.Decode(bytes.NewReader(mask_data))
		if err != nil {
			result = append(result, data)
			continue
		}
		bounds := source.Bounds()
		resized := imaging.Resize(mask, bounds.Dx(), bounds.Dy(), imaging.Lanczos)
		alpha := image.NewAlpha(bounds)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				alpha.SetAlpha(x, y, color.Alpha{A: color.GrayModel.Convert(resized.At(x, y)).(color.Gray).Y})
			}
		}
		output := image.NewNRGBA(bounds)
		draw.DrawMask(output, bounds, source, bounds.Min, alpha, bounds.Min, draw.Src)
		buffer := bytes.NewBuffer(nil)
		if png.Encode(buffer, output) == nil {
			result = append(result, buffer.Bytes())
		} else {
			result = append(result, data)
		}
	}
	return result
}

func imagePrompt(prompt string, size string, quality string) string {
	hints := []string{}
	if quality != "" && quality != "auto" {
		hints = append(hints, "输出图片质量为 "+quality+"。")
	}
	if len(hints) == 0 {
		return prompt
	}
	return prompt + "\n\n" + strings.Join(hints, "")
}

func imageGenerationSettings(size string) map[string]any {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return nil
	}
	width, width_err := strconv.Atoi(strings.TrimSpace(parts[0]))
	height, height_err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if width_err != nil || height_err != nil || width <= 0 || height <= 0 {
		return nil
	}
	ratio := float64(width) / float64(height)
	options := []struct {
		label string
		value float64
	}{{"1:1", 1}, {"4:3", 4.0 / 3}, {"3:4", 3.0 / 4}, {"16:9", 16.0 / 9}, {"9:16", 9.0 / 16}}
	best, best_diff := "", math.MaxFloat64
	for _, option := range options {
		diff := math.Abs(math.Log(ratio / option.value))
		if diff < best_diff {
			best, best_diff = option.label, diff
		}
	}
	return map[string]any{"aspect_ratio": best, "image_count": 1}
}
func defaultString(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func NewID(prefix string) string {
	return prefix + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
}
