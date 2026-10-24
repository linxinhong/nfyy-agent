// Package catalog is the skill and capability directory plus the controlled
// distribution of the signed Python capability package (ported from the
// MindLink center). The gateway merges released Skill Registry packages of
// the caller's Workspace into Search, so clients see one directory.
//
// Why it is written this way: installers verify manifest signatures, sizes
// and SHA-256 on the client; the server's job is to publish only what the
// signed manifest lists and nothing else from the artifact directory.
//
// Invariants and pitfalls:
//   - Artifact reads go through os.Root and must be regular files: symlinks,
//     traversal and files the manifest does not list are never served.
//   - Archive names must match archivePattern AND be referenced by the
//     manifest; either check alone is not enough.
//   - Only builtin.python.echo is published from the artifact root.
package catalog

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PythonEchoID is the only package served from the artifact root.
const PythonEchoID = "builtin.python.echo"

// SchemaVersion is the directory response version.
const SchemaVersion = 1

var (
	ErrNotFound     = errors.New("catalog entry not found")
	ErrNotPublished = errors.New("capability artifact not published")
	ErrInvalidPath  = errors.New("invalid capability artifact path")
)

// Item is one directory entry.
type Item struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Publisher   string   `json:"publisher"`
	Source      string   `json:"source"`
	Components  []string `json:"components"`
	Permissions []string `json:"permissions"`
}

var builtinItems = []Item{
	{
		ID: PythonEchoID, Name: "yuanpu-echo-mcp", DisplayName: "Python 示例能力", Version: "0.1.0",
		Description: "进程外运行的自包含 Python 能力包，用于验证安全安装、调用与更新链路。",
		Publisher:   "yuanpu", Source: "artifact:/enterprise/v1/capability-packages/" + PythonEchoID + "/manifest",
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

var archivePattern = regexp.MustCompile(`^YuanpuEchoMcp-(?:darwin|linux|win32)-(?:arm64|x64)\.tar\.gz$`)

const maxManifestBytes = 1 << 20

// Directory serves the built-in entries and the artifact root.
type Directory struct {
	artifactRoot string
}

// New returns a directory; an empty artifactRoot keeps artifacts unpublished.
func New(artifactRoot string) (*Directory, error) {
	if artifactRoot == "" {
		return &Directory{}, nil
	}
	abs, err := filepath.Abs(artifactRoot)
	if err != nil {
		return nil, err
	}
	return &Directory{artifactRoot: abs}, nil
}

// RegistryItem describes a released Skill Registry package as an entry.
func RegistryItem(name, version, workspaceID string) Item {
	return Item{
		ID: "registry." + name, Name: name, DisplayName: name, Version: version,
		Description: "企业 Skill Registry 已发布技能", Publisher: workspaceID,
		Source:     "registry:" + name + "@" + version,
		Components: []string{"skill"}, Permissions: []string{},
	}
}

func (d *Directory) items(extra []Item) []Item {
	out := append([]Item(nil), builtinItems...)
	if m, _, err := d.manifest(); err == nil && m.Version != "" {
		out[0].Version = m.Version
	}
	return append(out, extra...)
}

func matches(item Item, query string) bool {
	values := append([]string{item.ID, item.Name, item.DisplayName, item.Description, item.Publisher}, item.Components...)
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), query) {
			return true
		}
	}
	return false
}

// Search filters built-in and extra entries by a case-insensitive query.
func (d *Directory) Search(query string, extra []Item) []Item {
	query = strings.ToLower(strings.TrimSpace(query))
	out := []Item{}
	for _, item := range d.items(extra) {
		if query == "" || matches(item, query) {
			out = append(out, item)
		}
	}
	return out
}

// Item returns one entry by id.
func (d *Directory) Item(id string, extra []Item) (Item, error) {
	for _, item := range d.items(extra) {
		if item.ID == id {
			return item, nil
		}
	}
	return Item{}, ErrNotFound
}

type manifestShape struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Artifacts []struct {
		URL string `json:"url"`
	} `json:"artifacts"`
}

func (d *Directory) open(name string) (*os.File, error) {
	if d.artifactRoot == "" {
		return nil, os.ErrNotExist
	}
	root, err := os.OpenRoot(d.artifactRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

func (d *Directory) manifest() (manifestShape, []byte, error) {
	var m manifestShape
	f, err := d.open("manifest.json")
	if err != nil {
		return m, nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil || len(body) > maxManifestBytes || json.Unmarshal(body, &m) != nil || m.ID != PythonEchoID {
		return manifestShape{}, nil, ErrNotPublished
	}
	return m, body, nil
}

// Manifest returns the signed manifest bytes exactly as published.
func (d *Directory) Manifest(packageID string) ([]byte, error) {
	if packageID != PythonEchoID {
		return nil, ErrNotPublished
	}
	_, raw, err := d.manifest()
	if err != nil {
		return nil, ErrNotPublished
	}
	return raw, nil
}

// OpenArtifact opens a manifest-referenced archive; the caller closes it.
func (d *Directory) OpenArtifact(packageID, filename string) (*os.File, error) {
	if filename == "" || filename != filepath.Base(filename) || strings.ContainsAny(filename, `/\`) || filename == ".." {
		return nil, ErrInvalidPath
	}
	if packageID != PythonEchoID {
		return nil, ErrNotPublished
	}
	m, _, err := d.manifest()
	if err != nil || !archivePattern.MatchString(filename) || !references(m, filename) {
		return nil, ErrNotPublished
	}
	f, err := d.open(filename)
	if err != nil {
		return nil, ErrNotPublished
	}
	return f, nil
}

func references(m manifestShape, filename string) bool {
	for _, a := range m.Artifacts {
		parsed, err := url.Parse(a.URL)
		if err == nil && filepath.Base(parsed.Path) == filename {
			return true
		}
	}
	return false
}
