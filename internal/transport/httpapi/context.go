package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

type contextKey int
const userKey contextKey = 1
func contextWithUser(r *http.Request, u sqlite.User) *http.Request { return r.WithContext(context.WithValue(r.Context(), userKey, u)) }
func userFromContext(r *http.Request) sqlite.User { u, _ := r.Context().Value(userKey).(sqlite.User); return u }
func randomID() (string, error) { b:=make([]byte,16); if _,err:=rand.Read(b);err!=nil{return "",err};return hex.EncodeToString(b),nil }
