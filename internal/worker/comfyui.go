package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	PromptPlaceholder     = "{{T2VA_PROMPT}}"
	InputImagePlaceholder = "{{T2VA_INPUT_IMAGE}}"
	maxComfyOutputSize    = 500 << 20
)

// ComfyUIEngine runs an API-format ComfyUI workflow. Put PromptPlaceholder
// in the workflow's text input and InputImagePlaceholder in its image input.
type ComfyUIEngine struct {
	BaseURL, WorkflowPath string
	Client                *http.Client
	PollInterval          time.Duration
	Timeout               time.Duration
}

type comfyUploadedImage struct {
	Name      string `json:"name"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

type comfyPromptResponse struct {
	PromptID string `json:"prompt_id"`
}

type comfyHistoryEntry struct {
	Outputs map[string]map[string]json.RawMessage `json:"outputs"`
	Status  struct {
		StatusString string            `json:"status_str"`
		Completed    bool              `json:"completed"`
		Messages     []json.RawMessage `json:"messages"`
	} `json:"status"`
}

type comfyOutputFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

func (e *ComfyUIEngine) Validate() error {
	if _, err := e.endpoint("/"); err != nil {
		return err
	}
	if strings.TrimSpace(e.WorkflowPath) == "" {
		return errors.New("COMFYUI_WORKFLOW must point to an API-format workflow JSON file")
	}
	_, prompts, images, err := e.loadWorkflow("", "")
	if err != nil {
		return err
	}
	if prompts == 0 || images == 0 {
		return fmt.Errorf("workflow must contain %q and %q placeholders", PromptPlaceholder, InputImagePlaceholder)
	}
	return nil
}

func (e *ComfyUIEngine) Generate(ctx context.Context, input, prompt string) (string, error) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workflow, prompts, images, err := e.loadWorkflow("", "")
	if err != nil {
		return "", err
	}
	if prompts == 0 || images == 0 {
		return "", fmt.Errorf("workflow must contain %q and %q placeholders", PromptPlaceholder, InputImagePlaceholder)
	}
	uploaded, err := e.uploadInput(ctx, input)
	if err != nil {
		return "", err
	}
	imageName := uploaded.Name
	if uploaded.Subfolder != "" {
		imageName = path.Join(uploaded.Subfolder, uploaded.Name)
	}
	workflow, _, _, err = e.loadWorkflow(prompt, imageName)
	if err != nil {
		return "", err
	}

	var queued comfyPromptResponse
	if err := e.postJSON(ctx, "/prompt", map[string]any{"prompt": workflow}, &queued); err != nil {
		return "", fmt.Errorf("submit ComfyUI workflow: %w", err)
	}
	if queued.PromptID == "" {
		return "", errors.New("ComfyUI returned no prompt_id")
	}
	return e.waitForVideo(ctx, queued.PromptID)
}

func (e *ComfyUIEngine) endpoint(route string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(e.BaseURL))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("COMFYUI_URL must be an http(s) URL without credentials or query parameters")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(route, "/")
	return base.String(), nil
}

func (e *ComfyUIEngine) httpClient() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{Timeout: 15 * time.Minute}
}

func (e *ComfyUIEngine) loadWorkflow(prompt, imageName string) (map[string]any, int, int, error) {
	data, err := os.ReadFile(e.WorkflowPath)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read COMFYUI_WORKFLOW: %w", err)
	}
	var workflow map[string]any
	if err := json.Unmarshal(data, &workflow); err != nil || len(workflow) == 0 {
		return nil, 0, 0, errors.New("COMFYUI_WORKFLOW must be a non-empty ComfyUI API-format JSON object")
	}
	updated, prompts, images := replaceComfyPlaceholders(workflow, prompt, imageName)
	result, ok := updated.(map[string]any)
	if !ok {
		return nil, 0, 0, errors.New("invalid ComfyUI workflow object")
	}
	return result, prompts, images, nil
}

func replaceComfyPlaceholders(value any, prompt, imageName string) (any, int, int) {
	switch item := value.(type) {
	case map[string]any:
		prompts, images := 0, 0
		for key, child := range item {
			replaced, p, i := replaceComfyPlaceholders(child, prompt, imageName)
			item[key] = replaced
			prompts += p
			images += i
		}
		return item, prompts, images
	case []any:
		prompts, images := 0, 0
		for index, child := range item {
			replaced, p, i := replaceComfyPlaceholders(child, prompt, imageName)
			item[index] = replaced
			prompts += p
			images += i
		}
		return item, prompts, images
	case string:
		prompts := strings.Count(item, PromptPlaceholder)
		images := strings.Count(item, InputImagePlaceholder)
		item = strings.ReplaceAll(item, PromptPlaceholder, prompt)
		item = strings.ReplaceAll(item, InputImagePlaceholder, imageName)
		return item, prompts, images
	default:
		return value, 0, 0
	}
}

func (e *ComfyUIEngine) uploadInput(ctx context.Context, input string) (comfyUploadedImage, error) {
	file, err := os.Open(input)
	if err != nil {
		return comfyUploadedImage{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return comfyUploadedImage{}, err
	}
	if info.Size() <= 0 || info.Size() > 20<<20 {
		return comfyUploadedImage{}, errors.New("ComfyUI input image must be between 1 byte and 20 MiB")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", filepath.Base(input))
	if err != nil {
		return comfyUploadedImage{}, err
	}
	written, err := io.Copy(part, io.LimitReader(file, 20<<20+1))
	if err != nil {
		return comfyUploadedImage{}, err
	}
	if written > 20<<20 {
		return comfyUploadedImage{}, errors.New("ComfyUI input image exceeds 20 MiB")
	}
	if err := writer.WriteField("type", "input"); err != nil {
		return comfyUploadedImage{}, err
	}
	if err := writer.WriteField("overwrite", "true"); err != nil {
		return comfyUploadedImage{}, err
	}
	if err := writer.Close(); err != nil {
		return comfyUploadedImage{}, err
	}
	endpoint, err := e.endpoint("/upload/image")
	if err != nil {
		return comfyUploadedImage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return comfyUploadedImage{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return comfyUploadedImage{}, fmt.Errorf("upload input to ComfyUI: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return comfyUploadedImage{}, fmt.Errorf("ComfyUI image upload returned HTTP %d", resp.StatusCode)
	}
	var uploaded comfyUploadedImage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&uploaded); err != nil {
		return comfyUploadedImage{}, errors.New("ComfyUI returned an invalid image upload response")
	}
	if uploaded.Name == "" {
		return comfyUploadedImage{}, errors.New("ComfyUI image upload response has no filename")
	}
	return uploaded, nil
}

func (e *ComfyUIEngine) postJSON(ctx context.Context, route string, payload any, target any) error {
	endpoint, err := e.endpoint(route)
	if err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			return fmt.Errorf("ComfyUI returned HTTP %d (could not read error response)", resp.StatusCode)
		}
		message := strings.TrimSpace(string(detail))
		if message == "" {
			return fmt.Errorf("ComfyUI returned HTTP %d with an empty error response", resp.StatusCode)
		}
		return fmt.Errorf("ComfyUI returned HTTP %d: %s", resp.StatusCode, message)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target); err != nil {
		return errors.New("ComfyUI returned an invalid JSON response")
	}
	return nil
}

func (e *ComfyUIEngine) waitForVideo(ctx context.Context, promptID string) (string, error) {
	interval := e.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		entry, found, err := e.getHistory(ctx, promptID)
		if err != nil {
			return "", err
		}
		if found {
			if strings.EqualFold(entry.Status.StatusString, "error") {
				return "", fmt.Errorf("ComfyUI workflow execution failed: %s", describeComfyFailure(entry.Status.Messages))
			}
			if output := firstVideoOutput(entry.Outputs); output != nil {
				return e.downloadOutput(ctx, *output)
			}
			if entry.Status.Completed {
				return "", errors.New("ComfyUI workflow completed without a supported video output")
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for ComfyUI result: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func describeComfyFailure(messages []json.RawMessage) string {
	for _, raw := range messages {
		var event []json.RawMessage
		if err := json.Unmarshal(raw, &event); err != nil || len(event) < 2 {
			continue
		}
		var eventType string
		if err := json.Unmarshal(event[0], &eventType); err != nil || eventType != "execution_error" {
			continue
		}
		var detail struct {
			NodeID           string `json:"node_id"`
			NodeType         string `json:"node_type"`
			ExceptionMessage string `json:"exception_message"`
		}
		if err := json.Unmarshal(event[1], &detail); err != nil {
			continue
		}
		message := strings.TrimSpace(detail.ExceptionMessage)
		if detail.NodeType != "" && detail.NodeID != "" {
			return fmt.Sprintf("node %s (ID %s): %s", detail.NodeType, detail.NodeID, message)
		}
		if detail.NodeType != "" {
			return fmt.Sprintf("node %s: %s", detail.NodeType, message)
		}
		if message != "" {
			return message
		}
	}
	return "see the GPU computer's ComfyUI console for the failing node details"
}

func (e *ComfyUIEngine) getHistory(ctx context.Context, promptID string) (comfyHistoryEntry, bool, error) {
	endpoint, err := e.endpoint("/history/" + url.PathEscape(promptID))
	if err != nil {
		return comfyHistoryEntry{}, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return comfyHistoryEntry{}, false, err
	}
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return comfyHistoryEntry{}, false, fmt.Errorf("read ComfyUI history: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return comfyHistoryEntry{}, false, fmt.Errorf("ComfyUI history returned HTTP %d", resp.StatusCode)
	}
	var history map[string]comfyHistoryEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&history); err != nil {
		return comfyHistoryEntry{}, false, errors.New("ComfyUI returned invalid history JSON")
	}
	entry, found := history[promptID]
	return entry, found, nil
}

func firstVideoOutput(outputs map[string]map[string]json.RawMessage) *comfyOutputFile {
	nodeIDs := make([]string, 0, len(outputs))
	for id := range outputs {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	for _, nodeID := range nodeIDs {
		node := outputs[nodeID]
		for _, key := range []string{"videos", "gifs", "images", "files"} {
			raw, ok := node[key]
			if !ok {
				continue
			}
			var files []comfyOutputFile
			if err := json.Unmarshal(raw, &files); err != nil {
				continue
			}
			for _, file := range files {
				if isVideoFile(file.Filename) {
					if file.Type == "" {
						file.Type = "output"
					}
					return &file
				}
			}
		}
	}
	return nil
}

func isVideoFile(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mp4", ".webm", ".mov", ".mkv", ".avi", ".gif":
		return true
	default:
		return false
	}
}

func (e *ComfyUIEngine) downloadOutput(ctx context.Context, file comfyOutputFile) (string, error) {
	endpoint, err := e.endpoint("/view")
	if err != nil {
		return "", err
	}
	query := url.Values{
		"filename":  {file.Filename},
		"subfolder": {file.Subfolder},
		"type":      {file.Type},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("download ComfyUI video: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ComfyUI video download returned HTTP %d", resp.StatusCode)
	}
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if !isVideoFile(file.Filename) {
		ext = ".mp4"
	}
	out, err := os.CreateTemp("", "t2va-comfy-*"+ext)
	if err != nil {
		return "", err
	}
	name := out.Name()
	written, copyErr := io.Copy(out, io.LimitReader(resp.Body, maxComfyOutputSize+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || written > maxComfyOutputSize {
		_ = os.Remove(name)
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return "", errors.New("ComfyUI output exceeds 500 MiB")
	}
	return name, nil
}
