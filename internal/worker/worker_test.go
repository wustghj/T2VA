package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWebsocketURLUsesCloudPathPrefix(t *testing.T) {
	got, err := websocketURL("https://example.test/root")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://example.test/root/ws/worker" {
		t.Fatalf("unexpected endpoint %q", got)
	}
}
func TestSafeIDRejectsPathTraversal(t *testing.T) {
	for _, id := range []string{"../secret", "a/b", "a\\b", ""} {
		if safeID(id) {
			t.Errorf("accepted unsafe id %q", id)
		}
	}
	if !safeID("image-id") {
		t.Fatal("rejected safe id")
	}
}
func TestMockEngineProducesOutput(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.png")
	if err := os.WriteFile(input, []byte("test image"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := (MockEngine{}).Generate(context.Background(), input, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "test image" {
		t.Fatalf("output=%q", got)
	}
}
func TestDownloadRejectsOversizedInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 20<<20+1))
	}))
	defer server.Close()
	a := New(Config{CloudURL: server.URL, OutputDir: t.TempDir()}, MockEngine{})
	if _, err := a.download(context.Background(), "image-id"); err == nil {
		t.Fatal("expected oversized input to be rejected")
	}
}
