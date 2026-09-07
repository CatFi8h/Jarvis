// Package webapp serves the Telegram Mini App: the static frontend plus a small
// JSON API over the GR watcher. Requests are authenticated with the Mini App
// initData signature (HMAC over the bot token), so the backend derives the
// Telegram user id itself — the client never states who it is.
package webapp

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"time"

	"jarvis-bot/internal/gr"
)

//go:embed static
var staticFS embed.FS

const initDataMaxAge = 12 * time.Hour

// Server is the Mini App HTTP server.
type Server struct {
	addr     string
	botToken string
	gr       *gr.Watcher
}

func New(addr, botToken string, w *gr.Watcher) *Server {
	return &Server{addr: addr, botToken: botToken, gr: w}
}

// handler builds the full route table (static frontend + authenticated API).
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("GET /api/meta", s.auth(s.handleMeta))
	mux.HandleFunc("GET /api/trains", s.auth(s.handleTrains))
	mux.HandleFunc("GET /api/searches", s.auth(s.handleSearchList))
	mux.HandleFunc("POST /api/searches", s.auth(s.handleSearchCreate))
	mux.HandleFunc("DELETE /api/searches/{id}", s.auth(s.handleSearchDelete))
	mux.HandleFunc("POST /api/searches/{id}/check", s.auth(s.handleSearchCheck))
	return mux
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	srv := &http.Server{Addr: s.addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("[webapp] listening on %s", s.addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[webapp] server error: %v", err)
	}
}

// auth validates the initData passed in "Authorization: tma <initData>" and
// injects the resolved chat id into the handler.
func (s *Server) auth(next func(w http.ResponseWriter, r *http.Request, chatID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "tma "
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
			jsonError(w, http.StatusUnauthorized, "missing initData")
			return
		}
		chatID, err := validateInitData(h[len(prefix):], s.botToken, initDataMaxAge)
		if err != nil {
			jsonError(w, http.StatusUnauthorized, "auth failed: "+err.Error())
			return
		}
		next(w, r, chatID)
	}
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request, chatID string) {
	min, max := s.gr.DateRange()
	jsonOK(w, map[string]any{
		"directions": s.gr.Directions(),
		"min_date":   min,
		"max_date":   max,
	})
}

func (s *Server) handleTrains(w http.ResponseWriter, r *http.Request, chatID string) {
	dir := r.URL.Query().Get("dir")
	date := r.URL.Query().Get("date")
	route, trains, err := s.gr.TrainsFor(chatID, dir, date)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOK(w, map[string]any{"route": route, "date": date, "trains": trains})
}

func (s *Server) handleSearchList(w http.ResponseWriter, r *http.Request, chatID string) {
	jsonOK(w, map[string]any{"searches": s.gr.SearchList(chatID)})
}

func (s *Server) handleSearchCreate(w http.ResponseWriter, r *http.Request, chatID string) {
	var body struct {
		Dir      string `json:"dir"`
		Date     string `json:"date"`
		TrainNum string `json:"train_number"`
		DepTime  string `json:"dep_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	sv, err := s.gr.CreateSearch(chatID, body.Dir, body.Date, body.TrainNum, body.DepTime)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, sv)
}

func (s *Server) handleSearchDelete(w http.ResponseWriter, r *http.Request, chatID string) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.gr.DeleteSearch(chatID, id); err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	jsonOK(w, map[string]any{"deleted": id})
}

func (s *Server) handleSearchCheck(w http.ResponseWriter, r *http.Request, chatID string) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "bad id")
		return
	}
	sv, err := s.gr.CheckSearch(chatID, id)
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	jsonOK(w, sv)
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}
