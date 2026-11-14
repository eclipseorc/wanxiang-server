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
func main(){db,err:=sqlite.Open(env("DB_PATH","./data/wanxiang.db"));if err!=nil{log.Fatal(err)};defer db.Close();if err=sqlite.InitSchema(db);err!=nil{log.Fatal(err)};st:=sqlite.NewStore(db);if err=st.ResetWatch();err!=nil{log.Fatal("reset stale watch session: ",err)};hub:=wsserver.NewHub(st);stop:=make(chan struct{});go hub.RunControlPoller(stop);sig:=make(chan os.Signal,1);signal.Notify(sig,syscall.SIGINT,syscall.SIGTERM);go func(){<-sig;close(stop);os.Exit(0)}();addr:=env("WS_ADDR",":8081");log.Printf("WebSocket relay listening on %s",addr);log.Fatal(http.ListenAndServe(addr,wsserver.NewHandler(hub).ServeMux()))}
func env(k,d string)string{if v:=os.Getenv(k);v!=""{return v};return d}
