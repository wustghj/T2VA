package main

import (
	"context"
	"database/sql"
	"log"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"t2va/internal/api"
	"t2va/internal/identity"
	"t2va/internal/task"
	"time"
)

func main() {
	addr := env("ADDR", ":8080")
	dbPath := env("DATABASE_PATH", "./data/t2va.db")
	if err := os.MkdirAll("./data", 0750); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := task.Migrate(ctx, db); err != nil {
		log.Fatal(err)
	}
	repo, err := task.NewRepository(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	if err := repo.RequeueAllRunning(ctx); err != nil {
		log.Fatal(err)
	}
	identityService, err := identity.NewService(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	if err := identityService.CleanupExpiredSessions(ctx); err != nil {
		log.Printf("could not clean expired sessions: %v", err)
	}
	storage, err := api.NewStorage(env("STORAGE_PATH", "./data/files"))
	if err != nil {
		log.Fatal(err)
	}
	h := api.New(repo, storage, os.Getenv("WORKER_TOKEN"), os.Getenv("USER_API_KEY"))
	if os.Getenv("USER_API_KEY") != "" {
		log.Print("USER_API_KEY is deprecated and ignored; browser API access uses sessions")
	}
	cookieSecure := !strings.EqualFold(strings.TrimSpace(os.Getenv("COOKIE_SECURE")), "false")
	h.SetIdentity(identityService, cookieSecure)
	fileRepo := api.NewFileRepository(db)
	h.SetFileRepository(fileRepo)
	dispatcher := api.NewDispatcher(repo)
	dispatcher.SetFileRepository(fileRepo)
	dispatcher.SetAllowedOrigin(env("WORKER_ALLOWED_ORIGIN", ""))
	h.SetDispatcher(dispatcher)
	mux := http.NewServeMux()
	mux.Handle("/", h)
	mux.HandleFunc("/ws/worker", func(w http.ResponseWriter, r *http.Request) { dispatcher.Serve(w, r, os.Getenv("WORKER_TOKEN")) })
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	stop, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-stop.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("T2VA cloud listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
