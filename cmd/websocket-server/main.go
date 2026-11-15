// Command websocket-server runs the device and admin WebSocket relay.
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
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	wsserver "github.com/eclipseorc/wanxiang-server/internal/transport/websocket"
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

	store := sqlite.NewStore(db)
	if err := store.ResetWatch(); err != nil {
		return err
	}

	hub := wsserver.NewHub(store)
	server := &http.Server{
		Addr:              envfile.String("WS_ADDR", ":8081"),
		Handler:           wsserver.NewHandler(hub).ServeMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pollerDone := make(chan struct{})
	go func() {
		hub.RunControlPoller(ctx.Done())
		close(pollerDone)
	}()

	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("WebSocket relay listening on %s", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			<-pollerDone
			return err
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	<-pollerDone
	return nil
}
