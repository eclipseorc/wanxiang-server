package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	wsserver "github.com/eclipseorc/wanxiang-server/internal/transport/websocket"
)
func main(){db,e:=sqlite.Open(env("DB_PATH","./data/wanxiang.db"));if e!=nil{log.Fatal(e)};defer db.Close();if e=sqlite.InitSchema(db);e!=nil{log.Fatal(e)};hub:=wsserver.NewHub(sqlite.NewStore(db));stop:=make(chan struct{});go hub.RunControlPoller(stop);sig:=make(chan os.Signal,1);signal.Notify(sig,syscall.SIGINT,syscall.SIGTERM);go func(){<-sig;close(stop);os.Exit(0)}();addr:=env("WS_ADDR",":8081");log.Printf("WebSocket relay listening on %s",addr);log.Fatal(http.ListenAndServe(addr,wsserver.NewHandler(hub).ServeMux()))}
func env(k,d string)string{if v:=os.Getenv(k);v!=""{return v};return d}
