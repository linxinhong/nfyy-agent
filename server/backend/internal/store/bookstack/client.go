// Package bookstack is the replaceable content and permission adapter for a
// privately deployed BookStack instance. It never exposes the service token to
// callers or follows redirects to another host.
package bookstack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalid     = errors.New("invalid BookStack configuration or request")
	ErrDenied      = errors.New("BookStack denied the request")
	ErrNotFound    = errors.New("BookStack item not found")
	ErrUnavailable = errors.New("BookStack unavailable")
)

type Config struct {
	URL         string
	TokenID     string
	TokenSecret string
	AllowHTTP   bool // Only for an isolated internal network or local development.
}

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

type Page struct {
	ID        int64  `json:"id"`
	BookID    int64  `json:"book_id"`
	ChapterID *int64 `json:"chapter_id"`
	Name      string `json:"name"`
	Markdown  string `json:"markdown"`
	HTML      string `json:"html"`
	CreatedBy struct {
		ID int64 `json:"id"`
	} `json:"created_by"`
}

type PageInput struct {
	BookID    int64  `json:"book_id,omitempty"`
	ChapterID int64  `json:"chapter_id,omitempty"`
	Name      string `json:"name"`
	Markdown  string `json:"markdown"`
}

type RolePermission struct {
	RoleID int64 `json:"role_id"`
	View   bool  `json:"view"`
	Create bool  `json:"create"`
	Update bool  `json:"update"`
	Delete bool  `json:"delete"`
}

type FallbackPermission struct {
	Inheriting bool `json:"inheriting"`
	View       bool `json:"view"`
	Create     bool `json:"create"`
	Update     bool `json:"update"`
	Delete     bool `json:"delete"`
}

type Permissions struct {
	RolePermissions     []RolePermission   `json:"role_permissions"`
	FallbackPermissions FallbackPermission `json:"fallback_permissions"`
}

func New(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || base == nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		(base.Scheme != "https" && !(cfg.AllowHTTP && base.Scheme == "http")) ||
		strings.TrimSpace(cfg.TokenID) == "" || strings.TrimSpace(cfg.TokenSecret) == "" ||
		strings.ContainsAny(cfg.TokenID, ":\r\n") || strings.ContainsAny(cfg.TokenSecret, "\r\n") {
		return nil, ErrInvalid
	}
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
	return &Client{base: base, token: "Token " + cfg.TokenID + ":" + cfg.TokenSecret,
		http: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}}, nil
}

func (c *Client) request(ctx context.Context, method, path string, input, output any) error {
	if c == nil {
		return ErrUnavailable
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return ErrInvalid
		}
		body = bytes.NewReader(encoded)
	}
	u := *c.base
	u.Path = c.base.Path + "/api/" + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return ErrInvalid
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrDenied
	case http.StatusNotFound:
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ErrUnavailable
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(output); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (c *Client) GetPage(ctx context.Context, id int64) (Page, error) {
	if id < 1 {
		return Page{}, ErrInvalid
	}
	var page Page
	err := c.request(ctx, http.MethodGet, "pages/"+strconv.FormatInt(id, 10), nil, &page)
	return page, err
}

func (c *Client) CreatePage(ctx context.Context, input PageInput) (Page, error) {
	if (input.BookID < 1) == (input.ChapterID < 1) || strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Markdown) == "" {
		return Page{}, ErrInvalid
	}
	var page Page
	err := c.request(ctx, http.MethodPost, "pages", input, &page)
	return page, err
}

func (c *Client) UpdatePage(ctx context.Context, id int64, input PageInput) (Page, error) {
	if id < 1 || strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Markdown) == "" {
		return Page{}, ErrInvalid
	}
	// BookStack accepts a partial page update. A parent is intentionally omitted
	// so an update cannot silently move a page to another permission boundary.
	input.BookID, input.ChapterID = 0, 0
	var page Page
	err := c.request(ctx, http.MethodPut, "pages/"+strconv.FormatInt(id, 10), input, &page)
	return page, err
}

func permissionPath(contentType string, id int64) (string, error) {
	if id < 1 || (contentType != "page" && contentType != "book" && contentType != "chapter" && contentType != "bookshelf") {
		return "", ErrInvalid
	}
	return fmt.Sprintf("content-permissions/%s/%d", contentType, id), nil
}

func (c *Client) GetPermissions(ctx context.Context, contentType string, id int64) (Permissions, error) {
	path, err := permissionPath(contentType, id)
	if err != nil {
		return Permissions{}, err
	}
	var permissions Permissions
	err = c.request(ctx, http.MethodGet, path, nil, &permissions)
	return permissions, err
}

func (c *Client) SetPermissions(ctx context.Context, contentType string, id int64, permissions Permissions) error {
	path, err := permissionPath(contentType, id)
	if err != nil || permissions.FallbackPermissions.Inheriting || permissions.FallbackPermissions.View ||
		permissions.FallbackPermissions.Create || permissions.FallbackPermissions.Update || permissions.FallbackPermissions.Delete {
		return ErrInvalid
	}
	seen := make(map[int64]struct{}, len(permissions.RolePermissions))
	if permissions.RolePermissions == nil {
		permissions.RolePermissions = []RolePermission{}
	}
	for _, role := range permissions.RolePermissions {
		if role.RoleID < 1 || role.Delete {
			return ErrInvalid
		}
		if _, exists := seen[role.RoleID]; exists {
			return ErrInvalid
		}
		seen[role.RoleID] = struct{}{}
	}
	// Fail closed: every content item has an explicit deny-all fallback. The
	// caller must compute all permitted roles; this PUT replaces prior overrides.
	return c.request(ctx, http.MethodPut, path, permissions, nil)
}

func (c *Client) Ping(ctx context.Context) error {
	var system map[string]any
	return c.request(ctx, http.MethodGet, "system", nil, &system)
}
