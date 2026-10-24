// Package os provides the AgentOS HTTP server and route handlers.
package os

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/linxinhong/xingding-agent-runtime/pkg/os/playground"
)

func (os *AgentOS) registerBaseRoutes(auth func(http.Handler) http.Handler) {
	os.mux.Handle("GET /info", auth(http.HandlerFunc(os.handleInfo)))
	os.mux.Handle("GET /health", auth(http.HandlerFunc(os.handleHealth)))
	os.mux.Handle("GET /config", auth(http.HandlerFunc(os.handleConfig)))
	os.mux.Handle("GET /models", auth(http.HandlerFunc(os.handleModels)))

	// Playground UI at root and /playground.
	pg := playground.Handler()
	os.mux.Handle("GET /playground", auth(pg))
	os.mux.Handle("GET /playground/", auth(pg))
	os.mux.Handle("GET /{$}", auth(pg))
}

func (os *AgentOS) handleInfo(w http.ResponseWriter, r *http.Request) {
	response := map[string]any{
		"name":    "xingding-agent-runtime AgentOS",
		"version": "0.1.0",
	}
	if os.cfg.Project != nil {
		response["project"] = os.cfg.Project
	}
	respondJSON(w, http.StatusOK, response)
}

func (os *AgentOS) handleHealth(w http.ResponseWriter, r *http.Request) {
	if os.draining.Load() {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	status := "ok"
	if os.cfg.DB != nil {
		if err := os.cfg.DB.Ping(r.Context()); err != nil {
			status = "db_error: " + err.Error()
			respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": status})
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (os *AgentOS) handleConfig(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]any{
		"agents":    summarizeEntities(os.cfg.Agents),
		"teams":     summarizeEntities(os.cfg.Teams),
		"workflows": summarizeEntities(os.cfg.Workflows),
	})
}

func (os *AgentOS) handleModels(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]any{
		"models": os.listModels(),
	})
}

func (os *AgentOS) listModels() []map[string]any {
	if len(os.cfg.Models) == 0 {
		return []map[string]any{}
	}
	type item struct {
		id, name, provider string
	}
	items := make([]item, 0, len(os.cfg.Models))
	for id, m := range os.cfg.Models {
		items = append(items, item{id: id, name: m.ID(), provider: m.Provider()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	out := make([]map[string]any, len(items))
	for i, it := range items {
		out[i] = map[string]any{
			"id":       it.id,
			"name":     it.name,
			"provider": it.provider,
		}
	}
	return out
}

type entity interface {
	Name() string
}

type summaryItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func summarizeEntities[T entity](items map[string]T) []summaryItem {
	out := make([]summaryItem, 0, len(items))
	for id, item := range items {
		out = append(out, summaryItem{ID: id, Name: item.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func respondJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
