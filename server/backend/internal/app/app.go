// Package app is the process composition root for Yuanpu's Go center.
// It connects business modules to the HTTP host without putting business
// imports in the host or process lifecycle code.
package app

import (
	"errors"
	"io/fs"
	"net/http"
	"os"

	"yuanpu.dev/server/industries"
	"yuanpu.dev/server/internal/catalog"
	"yuanpu.dev/server/internal/content"
	"yuanpu.dev/server/internal/database"
	"yuanpu.dev/server/internal/httpapi"
	"yuanpu.dev/server/internal/identity"
	"yuanpu.dev/server/internal/industry"
	"yuanpu.dev/server/internal/ingest"
	"yuanpu.dev/server/internal/store/bookstack"
)

type Options struct {
	ArtifactRoot string
	IndustryRoot string
	IndustryID   string
	BookStack    bookstack.Config
}

func NewHandler(version string, db *database.Store, options ...Options) (http.Handler, error) {
	if db == nil {
		return nil, errors.New("database store is required")
	}
	users := identity.New(db.Pool())
	accounts := identity.NewHTTP(users)
	resourceStore := content.New(db.Pool())
	resources := content.NewHTTP(resourceStore, accounts.Authenticate)
	uploads := ingest.NewHTTP(db.Pool(), resourceStore, accounts.Authenticate)
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	directory, err := catalog.NewHTTP(opts.ArtifactRoot)
	if err != nil {
		return nil, err
	}
	modules := []httpapi.RouteModule{accounts, resources, uploads, directory}
	if opts.IndustryID != "" {
		var files fs.FS = industries.Files
		if opts.IndustryRoot != "" {
			files = os.DirFS(opts.IndustryRoot)
		}
		pack, err := industry.Load(files, opts.IndustryID)
		if err != nil {
			return nil, err
		}
		modules = append(modules, industry.NewHTTP(pack, files, opts.IndustryID, accounts.Authenticate))
	}
	if opts.BookStack.URL != "" {
		adapter, err := bookstack.New(opts.BookStack)
		if err != nil {
			return nil, err
		}
		modules = append(modules, bookstack.NewHTTP(adapter, accounts.Authenticate))
	}
	return httpapi.New(version, db, modules...), nil
}
