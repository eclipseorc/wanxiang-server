package main

import (
	"log"
	"net/http"
	"os"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/eclipseorc/wanxiang-server/internal/transport/httpapi"
)
func main(){path:=env("DB_PATH","./data/wanxiang.db");db,e:=sqlite.Open(path);if e!=nil{log.Fatal(e)};defer db.Close();if e=sqlite.InitSchema(db);e!=nil{log.Fatal(e)};st:=sqlite.NewStore(db);svc:=logic.New(st);if e=svc.EnsureAdmin(env("ADMIN_USERNAME","admin"),env("ADMIN_PASSWORD",""));e!=nil{log.Fatal("bootstrap admin: ",e)};addr:=env("HTTP_ADDR",":8080");log.Printf("HTTP management API listening on %s",addr);log.Fatal(http.ListenAndServe(addr,httpapi.New(svc).Handler()))}
func env(k,d string)string{if v:=os.Getenv(k);v!=""{return v};return d}
