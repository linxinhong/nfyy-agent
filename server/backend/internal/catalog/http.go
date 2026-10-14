// Package catalog owns the public skill directory and controlled capability
// artifact publication. Installation and signature verification remain client
// responsibilities.
package catalog

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"yuanpu.dev/server/internal/httpkit"
)

type Item struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Publisher   string   `json:"publisher"`
	Source      string   `json:"source"`
	Components  []string `json:"components"`
	Permissions []string `json:"permissions"`
}

var items = []Item{
	{
		ID: "builtin.python.echo", Name: "yuanpu-echo-mcp", DisplayName: "Python 示例能力", Version: "0.1.0",
		Description: "进程外运行的自包含 Python 能力包，用于验证安全安装、调用与更新链路。",
		Publisher:   "yuanpu", Source: "artifact:/v1/capability-packages/builtin.python.echo/manifest",
		Components: []string{"connector"}, Permissions: []string{"notifications", "background"},
	},
	{
		ID: "works.earendil.dynamic-workflows", Name: "@quintinshaw/pi-dynamic-workflows", DisplayName: "工作流编排", Version: "3.12.0",
		Description: "组合专家角色与多步骤工作流，自动拆解和执行复杂任务。",
		Publisher:   "quintinshaw", Source: "npm:@quintinshaw/pi-dynamic-workflows@3.12.0",
		Components: []string{"agent", "workflow", "extension"}, Permissions: []string{"scripts", "filesystem", "background"},
	},
	{
		ID: "community.hermes-memory", Name: "pi-hermes-memory", DisplayName: "长期记忆", Version: "0.9.9",
		Description: "为对话提供跨会话的长期记忆、知识沉淀和检索能力。",
		Publisher:   "community", Source: "npm:pi-hermes-memory@0.9.9",
		Components: []string{"skill", "extension"}, Permissions: []string{"filesystem", "background"},
	},
}

type HTTP struct{ artifactRoot string }

func NewHTTP(artifactRoot string) (*HTTP, error) {
	if artifactRoot == "" {
		return &HTTP{}, nil
	}
	absolute, err := filepath.Abs(artifactRoot)
	if err != nil {
		return nil, err
	}
	return &HTTP{artifactRoot: absolute}, nil
}

func (h *HTTP) Mount(router chi.Router) {
	router.Get("/v1/catalog/search", h.search)
	router.Get("/v1/catalog/items/{id}", h.detail)
	router.Get("/v1/capability-packages/*", h.capabilityPackage)
}

func (h *HTTP) publishedItems() []Item {
	result := append([]Item(nil), items...)
	manifest, _, err := h.readManifest()
	if err == nil && manifest.Version != "" {
		result[0].Version = manifest.Version
	}
	return result
}

func (h *HTTP) search(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	result := make([]Item, 0, len(items))
	for _, item := range h.publishedItems() {
		if query == "" || matches(item, query) {
			result = append(result, item)
		}
	}
	httpkit.WriteJSON(w, http.StatusOK, struct {
		SchemaVersion int    `json:"schemaVersion"`
		Items         []Item `json:"items"`
	}{1, result})
}

func matches(item Item, query string) bool {
	values := []string{item.ID, item.Name, item.DisplayName, item.Description, item.Publisher}
	values = append(values, item.Components...)
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func (h *HTTP) detail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	for _, item := range h.publishedItems() {
		if item.ID == id {
			httpkit.WriteJSON(w, http.StatusOK, item)
			return
		}
	}
	httpkit.WriteError(w, r, http.StatusNotFound, "CATALOG_ITEM_NOT_FOUND", "Skill not found")
}

type manifest struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Artifacts []struct {
		URL string `json:"url"`
	} `json:"artifacts"`
}

func (h *HTTP) openFile(name string) (*os.File, error) {
	if h.artifactRoot == "" {
		return nil, os.ErrNotExist
	}
	root, err := os.OpenRoot(h.artifactRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil || !actual.Mode().IsRegular() {
		file.Close()
		return nil, os.ErrNotExist
	}
	return file, nil
}

func (h *HTTP) readManifest() (manifest, json.RawMessage, error) {
	var value manifest
	file, err := h.openFile("manifest.json")
	if err != nil {
		return value, nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(body) > 1<<20 || json.Unmarshal(body, &value) != nil || value.ID != "builtin.python.echo" {
		return manifest{}, nil, errors.New("invalid published manifest")
	}
	return value, body, nil
}

var archiveName = regexp.MustCompile(`^YuanpuEchoMcp-(?:darwin|linux|win32)-(?:arm64|x64)\.tar\.gz$`)

func (h *HTTP) capabilityPackage(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1/capability-packages/"
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	parts := strings.Split(escaped, "/")
	if len(parts) < 2 || len(parts) > 3 || (len(parts) == 3 && parts[1] != "artifacts") {
		httpkit.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "Endpoint not found")
		return
	}
	packageID, err := url.PathUnescape(parts[0])
	if err != nil || strings.ContainsAny(packageID, `/\`) {
		httpkit.WriteError(w, r, http.StatusBadRequest, "INVALID_ARTIFACT_PATH", "Invalid capability package path")
		return
	}
	if packageID != "builtin.python.echo" {
		httpkit.WriteError(w, r, http.StatusNotFound, "ARTIFACT_NOT_PUBLISHED", "Capability artifact not published")
		return
	}
	value, raw, err := h.readManifest()
	if err != nil {
		httpkit.WriteError(w, r, http.StatusNotFound, "ARTIFACT_NOT_PUBLISHED", "Capability artifact not published")
		return
	}
	if len(parts) == 2 && parts[1] == "manifest" {
		httpkit.WriteJSON(w, http.StatusOK, raw)
		return
	}
	if len(parts) != 3 {
		httpkit.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "Endpoint not found")
		return
	}
	filename, err := url.PathUnescape(parts[2])
	if err != nil || filename != filepath.Base(filename) || strings.ContainsAny(filename, `/\`) {
		httpkit.WriteError(w, r, http.StatusBadRequest, "INVALID_ARTIFACT_PATH", "Invalid artifact filename")
		return
	}
	if !archiveName.MatchString(filename) || !manifestReferences(value, filename) {
		httpkit.WriteError(w, r, http.StatusNotFound, "ARTIFACT_NOT_PUBLISHED", "Artifact is not published")
		return
	}
	file, err := h.openFile(filename)
	if err != nil {
		httpkit.WriteError(w, r, http.StatusNotFound, "ARTIFACT_NOT_PUBLISHED", "Artifact is not published")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		httpkit.WriteError(w, r, http.StatusNotFound, "ARTIFACT_NOT_PUBLISHED", "Artifact is not published")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func manifestReferences(value manifest, filename string) bool {
	for _, artifact := range value.Artifacts {
		parsed, err := url.Parse(artifact.URL)
		if err == nil && filepath.Base(parsed.Path) == filename {
			return true
		}
	}
	return false
}
