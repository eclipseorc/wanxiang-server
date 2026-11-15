// Command http-server runs the management API and serves the static console.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/envfile"
	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/eclipseorc/wanxiang-server/internal/transport/httpapi"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if err := envfile.Load(".env"); err != nil {
		return err
	}

	db, err := sqlite.Open(envfile.String("DB_PATH", "./data/wanxiang.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	if err := sqlite.InitSchema(db); err != nil {
		return err
	}

	service := logic.New(sqlite.NewStore(db))
	if err := service.EnsureAdmin(
		envfile.String("ADMIN_USERNAME", "admin"),
		envfile.String("ADMIN_PASSWORD", ""),
	); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", httpapi.New(service).Handler())
	mux.Handle("/", http.FileServer(http.Dir(envfile.String("WEB_DIR", "./web"))))

	server := &http.Server{
		Addr:              envfile.String("HTTP_ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("HTTP API and management UI listening on %s", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
