package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Task struct {
	ID          string `json:"task_id"`
	Prompt      string `json:"prompt"`
	InputFileID string `json:"input_file_id"`
}
type Engine interface {
	Generate(context.Context, string, string) (string, error)
}
type Config struct {
	CloudURL, Token, WorkerID, ComfyURL, WorkflowPath, OutputDir string
	PollInterval                                                 time.Duration
}
type Agent struct {
	cfg    Config
	engine Engine
	client *http.Client
}

func New(cfg Config, engine Engine) *Agent {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = "./outputs"
	}
	return &Agent{cfg: cfg, engine: engine, client: &http.Client{Timeout: 15 * time.Minute}}
}
func (a *Agent) Run(ctx context.Context) error {
	if a.cfg.CloudURL == "" || a.cfg.Token == "" || a.cfg.WorkerID == "" {
		return errors.New("CLOUD_URL, WORKER_TOKEN and WORKER_ID are required")
	}
	if a.engine == nil {
		return errors.New("inference engine is required")
	}
	wsURL, err := websocketURL(a.cfg.CloudURL)
	if err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = a.connect(ctx, wsURL)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(a.cfg.PollInterval):
			}
		}
	}
}
func websocketURL(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return "", e
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported cloud URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/worker"
	return u.String(), nil
}
func (a *Agent) connect(ctx context.Context, endpoint string) error {
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.cfg.Token)
	c, _, err := d.DialContext(ctx, endpoint, h)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.WriteJSON(map[string]any{"type": "REGISTER", "worker_id": a.cfg.WorkerID, "worker_version": "0.1.0", "timestamp": time.Now().UTC()})
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		_ = c.SetReadDeadline(time.Now().Add(75 * time.Second))
		var msg struct {
			Type string `json:"type"`
			Task Task   `json:"task"`
		}
		if err := c.ReadJSON(&msg); err != nil {
			return err
		}
		switch msg.Type {
		case "TASK_ASSIGN":
			if err := a.runTask(ctx, c, msg.Task); err != nil {
				return err
			}
		case "PING":
			_ = c.WriteJSON(map[string]string{"type": "PONG"})
		case "REGISTER_ACK", "HEARTBEAT_ACK":
		}
	}
}
func (a *Agent) runTask(ctx context.Context, c *websocket.Conn, t Task) error {
	_ = c.WriteJSON(map[string]any{"type": "TASK_ACK", "task_id": t.ID})
	_ = c.WriteJSON(map[string]any{"type": "TASK_STARTED", "task_id": t.ID, "timestamp": time.Now().UTC()})
	input, err := a.download(ctx, t.InputFileID, t.ID)
	if err != nil {
		return a.fail(c, t.ID, err)
	}
	defer os.Remove(input)
	output, err := a.engine.Generate(ctx, input, t.Prompt)
	if err != nil {
		return a.fail(c, t.ID, err)
	}
	defer os.Remove(output)
	fileID, err := a.upload(ctx, output)
	if err != nil {
		return a.fail(c, t.ID, err)
	}
	return c.WriteJSON(map[string]any{"type": "TASK_SUCCESS", "task_id": t.ID, "output_file_id": fileID, "timestamp": time.Now().UTC()})
}
func (a *Agent) fail(c *websocket.Conn, id string, e error) error {
	return c.WriteJSON(map[string]any{"type": "TASK_FAILED", "task_id": id, "error": e.Error(), "timestamp": time.Now().UTC()})
}
func (a *Agent) download(ctx context.Context, id string, taskID ...string) (string, error) {
	if !safeID(id) {
		return "", errors.New("invalid input file id")
	}
	endpoint := strings.TrimRight(a.cfg.CloudURL, "/") + "/api/v1/worker/files/" + url.PathEscape(id)
	if len(taskID) > 0 {
		endpoint += "?task_id=" + url.QueryEscape(taskID[0])
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if e != nil {
		return "", e
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, e := a.client.Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download input: %s", resp.Status)
	}
	if e = os.MkdirAll(a.cfg.OutputDir, 0750); e != nil {
		return "", e
	}
	ext := imageExtension(resp.Header.Get("Content-Type"))
	f, e := os.CreateTemp(a.cfg.OutputDir, "input-*"+ext)
	if e != nil {
		return "", e
	}
	name := f.Name()
	limited := io.LimitReader(resp.Body, 20<<20+1)
	written, e := io.Copy(f, limited)
	if e != nil {
		f.Close()
		os.Remove(name)
		return "", e
	}
	if written > 20<<20 {
		f.Close()
		os.Remove(name)
		return "", errors.New("input file exceeds 20 MiB")
	}
	if e = f.Close(); e != nil {
		os.Remove(name)
		return "", e
	}
	return name, nil
}

func imageExtension(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	switch strings.ToLower(mediaType) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/tiff":
		return ".tif"
	default:
		return ""
	}
}
func (a *Agent) upload(ctx context.Context, path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return "", e
	}
	if info.Size() <= 0 || info.Size() > 500<<20 {
		return "", errors.New("output file must be between 1 byte and 500 MiB")
	}
	reader, writer := io.Pipe()
	mw := multipart.NewWriter(writer)
	requestDone := make(chan error, 1)
	go func() {
		part, err := mw.CreateFormFile("file", filepath.Base(path))
		if err == nil {
			var copied int64
			copied, err = io.Copy(part, io.LimitReader(f, 500<<20+1))
			if err == nil && copied > 500<<20 {
				err = errors.New("output file exceeds 500 MiB")
			}
		}
		if closeErr := mw.Close(); err == nil {
			err = closeErr
		}
		_ = writer.CloseWithError(err)
		requestDone <- err
	}()
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.CloudURL, "/")+"/api/v1/worker/results", reader)
	if e != nil {
		_ = reader.CloseWithError(e)
		<-requestDone
		return "", e
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	resp, e := a.client.Do(req)
	if e != nil {
		_ = reader.CloseWithError(e)
		<-requestDone
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		_ = reader.CloseWithError(errors.New("cloud rejected result upload"))
		<-requestDone
		return "", fmt.Errorf("upload output: %s", resp.Status)
	}
	var v struct {
		FileID string `json:"file_id"`
	}
	if e = json.NewDecoder(resp.Body).Decode(&v); e != nil {
		return "", e
	}
	if e = <-requestDone; e != nil {
		return "", e
	}
	return v.FileID, nil
}
func safeID(s string) bool { return s != "" && filepath.Base(s) == s && !strings.ContainsAny(s, "/\\") }

type MockEngine struct{}

func (MockEngine) Generate(ctx context.Context, input, prompt string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(time.Second):
	}
	src, e := os.Open(input)
	if e != nil {
		return "", e
	}
	defer src.Close()
	out, e := os.CreateTemp("", "t2va-mock-*.mp4")
	if e != nil {
		return "", e
	}
	if _, e = io.Copy(out, src); e != nil {
		out.Close()
		os.Remove(out.Name())
		return "", e
	}
	if e = out.Close(); e != nil {
		return "", e
	}
	return out.Name(), nil
}
