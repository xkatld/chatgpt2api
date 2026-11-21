package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/repository"
)

const MAX_ACTIVE_TASKS = 10

var ErrTooManyTasks = errors.New("进行中的任务太多了，请等一部分完成后再提交")

type TaskService struct {
	repo     *repository.Repository
	ai       *AIService
	config   *config.Config
}

func NewTaskService(repo *repository.Repository, ai *AIService, config *config.Config) *TaskService {
	return &TaskService{repo: repo, ai: ai, config: config}
}

func (s *TaskService) List(ctx context.Context, owner_id string, ids []int64, types []string) ([]repository.Task, []int64, error) {
	if err := s.CleanupExpired(ctx); err != nil {
		return nil, nil, err
	}
	items, err := s.repo.Tasks(ctx, owner_id, ids, types)
	if err != nil {
		return nil, nil, err
	}
	found := map[int64]bool{}
	for _, item := range items {
		found[item.ID] = true
	}
	missing := []int64{}
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	return items, missing, nil
}

func (s *TaskService) SubmitImage(ctx context.Context, identity Identity, request ImageRequest, history_id *int64, turn_id *int64, kind string) (repository.Task, error) {
	if request.Model == "" || request.Model == "auto" {
		request.Model = s.config.ImageModelName
		if request.Model == "" {
			request.Model = config.DEFAULT_IMAGE_MODEL_NAME
		}
	}
	if !s.ai.IsImageModel(request.Model) {
		return repository.Task{}, fmt.Errorf("unsupported image model: %s", request.Model)
	}
	if request.Count <= 0 {
		request.Count = 1
	}
	if request.Quality == "" {
		request.Quality = "auto"
	}
	if request.Size == "" {
		request.Size = "1024x1024"
	}
	active, err := s.repo.CountActiveTasks(ctx, identity.ID)
	if err != nil {
		return repository.Task{}, err
	}
	if active >= MAX_ACTIVE_TASKS {
		return repository.Task{}, ErrTooManyTasks
	}
	if s.config.BaseURL != "" {
		request.BaseURL = s.config.BaseURL
	}
	task_title := title(request.Prompt)
	if task_title == "" {
		task_title = "图像任务"
	}
	if history_id != nil {
		if history, err := s.repo.Task(ctx, identity.ID, *history_id); err == nil {
			task_title = history.Title
		}
	}
	now := time.Now().UTC()
	item := repository.Task{OwnerID: identity.ID, HistoryID: history_id, TurnID: turn_id, Title: task_title, Type: kind, Status: "queued", Prompt: request.Prompt, Model: request.Model, Size: request.Size, Quality: request.Quality, OutputCount: request.Count, BaseURL: request.BaseURL, CreatedAt: now, UpdatedAt: now}
	item, err = s.repo.SaveTask(ctx, item)
	if err != nil {
		return item, err
	}
	if item.HistoryID == nil {
		item.HistoryID = &item.ID
	}
	if item.TurnID == nil {
		item.TurnID = &item.ID
	}
	item, err = s.repo.SaveTask(ctx, item)
	if err == nil {
		go s.runImage(identity, item, request)
	}
	return item, err
}

func (s *TaskService) runImage(identity Identity, task repository.Task, request ImageRequest) {
	if request.Model == "" || request.Model == "auto" {
		request.Model = task.Model
	}
	if request.Count <= 0 {
		request.Count = task.OutputCount
	}
	if request.Quality == "" {
		request.Quality = task.Quality
	}
	if request.Size == "" {
		request.Size = task.Size
	}
	ctx := context.Background()
	now := time.Now().UTC()
	task.Status, task.Progress, task.StartedAt, task.UpdatedAt = "running", "", &now, now
	s.repo.SaveTask(ctx, task)
	started := time.Now()
	call_log := s.ai.logs.NewCallLog(identity, map[string]string{"image_generation": "/api/tasks/image-generation", "image_edit": "/api/tasks/image-edit"}[task.Type], task.Model, task.Type+" ", task.Prompt, started)
	results, err := s.ai.Images(ctx, request, call_log)
	ended := time.Now().UTC()
	duration := int(time.Since(started).Milliseconds())
	task.EndedAt, task.UpdatedAt, task.DurationMS = &ended, ended, &duration
	if err != nil {
		var image_err *ImageError
		if errors.As(err, &image_err) {
			task.ConversationID = image_err.ConversationID
			task.AccountEmail = image_err.AccountEmail
		}
		task.Status, task.Error, task.Progress = "error", err.Error(), ""
		s.repo.SaveTask(ctx, task)
		return
	}
	output := make([]repository.TaskResult, 0, len(results))
	for index, result := range results {
		output = append(output, repository.TaskResult{TaskID: task.ID, Position: index, Type: "image", Name: "image-" + strconv.Itoa(index+1) + ".png", URL: result.URL, RevisedPrompt: result.RevisedPrompt, ContentType: "image/png"})
	}
	s.repo.ReplaceTaskResults(ctx, task.ID, output)
	task.InputTokens, task.OutputTokens, task.TotalTokens = imageTaskUsage(request, results)
	task.Status, task.Progress = "success", ""
	s.repo.SaveTask(ctx, task)
}

func (s *TaskService) CleanupExpired(ctx context.Context) error {
	return s.repo.DeleteFinishedTasksBefore(ctx, time.Now().AddDate(0, 0, -s.config.Image.RetentionDays))
}
func title(value string) string {
	runes := []rune(value)
	if len(runes) > 24 {
		return string(runes[:24])
	}
	return value
}
