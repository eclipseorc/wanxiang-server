package main

import (
	"log"
	"net/http"
	"os"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/eclipseorc/wanxiang-server/internal/transport/httpapi"
)

func main() {
	db, err := sqlite.Open(env("DB_PATH", "./data/wanxiang.db"))
	if err != nil { log.Fatal(err) }
	defer db.Close()
	if err = sqlite.InitSchema(db); err != nil { log.Fatal(err) }
	service := logic.New(sqlite.NewStore(db))
	if err = service.EnsureAdmin(env("ADMIN_USERNAME", "admin"), env("ADMIN_PASSWORD", "")); err != nil { log.Fatal("bootstrap admin: ", err) }

	mux := http.NewServeMux()
	mux.Handle("/api/", httpapi.New(service).Handler())
	mux.Handle("/", http.FileServer(http.Dir(env("WEB_DIR", "./web"))))
	addr := env("HTTP_ADDR", ":8080")
	log.Printf("HTTP API and management UI listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func env(key, fallback string) string { if v := os.Getenv(key); v != "" { return v }; return fallback }
