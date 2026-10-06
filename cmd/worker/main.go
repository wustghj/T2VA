package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"t2va/internal/worker"
)

func main() {
	var engine worker.Engine
	switch strings.ToLower(strings.TrimSpace(os.Getenv("INFERENCE_MODE"))) {
	case "", "mock":
		engine = worker.MockEngine{}
	case "comfyui":
		comfy := &worker.ComfyUIEngine{BaseURL: os.Getenv("COMFYUI_URL"), WorkflowPath: os.Getenv("COMFYUI_WORKFLOW")}
		if err := comfy.Validate(); err != nil {
			log.Fatal(err)
		}
		engine = comfy
	default:
		log.Fatal("INFERENCE_MODE must be mock or comfyui")
	}
	agent := worker.New(worker.Config{CloudURL: os.Getenv("CLOUD_URL"), Token: os.Getenv("WORKER_TOKEN"), WorkerID: os.Getenv("WORKER_ID"), ComfyURL: os.Getenv("COMFYUI_URL")}, engine)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
