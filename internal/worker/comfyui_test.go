package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestComfyUIEngineUploadsRunsAndDownloadsWorkflow(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "workflow_api.json")
	workflow := `{"1":{"class_type":"CLIPTextEncode","inputs":{"text":"` + PromptPlaceholder + `"}},"2":{"class_type":"LoadImage","inputs":{"image":"` + InputImagePlaceholder + `"}}}`
	if err := os.WriteFile(workflowPath, []byte(workflow), 0600); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(t.TempDir(), "input.png")
	if err := os.WriteFile(inputPath, []byte("png-data"), 0600); err != nil {
		t.Fatal(err)
	}

	var historyPolls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		jsonResponse := func(status int, body string) *http.Response {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}
		}
		switch {
		case req.URL.Path == "/upload/image":
			mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/form-data" {
				t.Fatalf("upload content-type=%q err=%v", req.Header.Get("Content-Type"), err)
			}
			reader := multipart.NewReader(req.Body, params["boundary"])
			fields := map[string]string{}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				data, _ := io.ReadAll(part)
				fields[part.FormName()] = string(data)
			}
			if fields["image"] != "png-data" || fields["type"] != "input" {
				t.Fatalf("uploaded fields=%v", fields)
			}
			return jsonResponse(http.StatusOK, `{"name":"uploaded.png","subfolder":"input","type":"input"}`), nil
		case req.URL.Path == "/prompt":
			var payload struct {
				Prompt map[string]map[string]any `json:"prompt"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Prompt["1"]["inputs"].(map[string]any)["text"] != "a red kite" {
				t.Fatalf("workflow prompt placeholder was not replaced: %#v", payload.Prompt["1"])
			}
			if payload.Prompt["2"]["inputs"].(map[string]any)["image"] != "input/uploaded.png" {
				t.Fatalf("workflow image placeholder was not replaced: %#v", payload.Prompt["2"])
			}
			return jsonResponse(http.StatusOK, `{"prompt_id":"job-1"}`), nil
		case req.URL.Path == "/history/job-1":
			if historyPolls.Add(1) == 1 {
				return jsonResponse(http.StatusOK, `{}`), nil
			}
			return jsonResponse(http.StatusOK, `{"job-1":{"outputs":{"7":{"videos":[{"filename":"render.mp4","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`), nil
		case req.URL.Path == "/view":
			query, _ := url.ParseQuery(req.URL.RawQuery)
			if query.Get("filename") != "render.mp4" || query.Get("type") != "output" {
				t.Fatalf("view query=%v", query)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader([]byte("video-data"))), Request: req}, nil
		default:
			t.Fatalf("unexpected ComfyUI request: %s %s", req.Method, req.URL)
			return nil, nil
		}
	})}
	engine := &ComfyUIEngine{BaseURL: "http://comfy.test", WorkflowPath: workflowPath, Client: client, PollInterval: time.Millisecond, Timeout: time.Second}
	if err := engine.Validate(); err != nil {
		t.Fatal(err)
	}
	output, err := engine.Generate(context.Background(), inputPath, "a red kite")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(output)
	if filepath.Ext(output) != ".mp4" {
		t.Fatalf("output path=%s", output)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "video-data" {
		t.Fatalf("video result=%q", data)
	}
	if historyPolls.Load() < 2 {
		t.Fatalf("history polls=%d; expected pending then complete", historyPolls.Load())
	}
}

func TestComfyUIEngineRejectsWorkflowWithoutPlaceholders(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "workflow_api.json")
	if err := os.WriteFile(workflowPath, []byte(`{"1":{"class_type":"LoadImage","inputs":{"image":"example.png"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	engine := &ComfyUIEngine{BaseURL: "http://comfy.test", WorkflowPath: workflowPath}
	if err := engine.Validate(); err == nil {
		t.Fatal("workflow without prompt/image placeholders was accepted")
	}
}

func TestComfyUIEngineIncludesPromptValidationDetails(t *testing.T) {
	engine := &ComfyUIEngine{
		BaseURL: "http://comfy.test",
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"prompt_outputs_failed_validation","message":"Prompt has no outputs"}}`)),
				Request:    req,
			}, nil
		})},
	}
	var response comfyPromptResponse
	err := engine.postJSON(context.Background(), "/prompt", map[string]any{"prompt": map[string]any{}}, &response)
	if err == nil || !strings.Contains(err.Error(), "prompt_outputs_failed_validation") || !strings.Contains(err.Error(), "Prompt has no outputs") {
		t.Fatalf("expected ComfyUI validation detail, got %v", err)
	}
}

func TestComfyUIEngineReportsNodeExecutionFailure(t *testing.T) {
	messages := []json.RawMessage{
		json.RawMessage(`["execution_start",{"prompt_id":"job-1"}]`),
		json.RawMessage(`["execution_error",{"prompt_id":"job-1","node_id":"307","node_type":"MiniMaxH3AudioConditioningT8","exception_message":"task input is invalid"}]`),
	}
	got := describeComfyFailure(messages)
	if !strings.Contains(got, "MiniMaxH3AudioConditioningT8") || !strings.Contains(got, "ID 307") || !strings.Contains(got, "task input is invalid") {
		t.Fatalf("execution failure detail = %q", got)
	}
}
