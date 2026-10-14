// Package app is the process composition root for Yuanpu's Go center.
// It connects business modules to the HTTP host without putting business
// imports in the host or process lifecycle code.
package app

import (
	"errors"
	"net/http"

	"yuanpu.dev/server/internal/catalog"
	"yuanpu.dev/server/internal/content"
	"yuanpu.dev/server/internal/database"
	"yuanpu.dev/server/internal/httpapi"
	"yuanpu.dev/server/internal/identity"
)

func NewHandler(version string, db *database.Store, artifactRoots ...string) (http.Handler, error) {
	if db == nil {
		return nil, errors.New("database store is required")
	}
	users := identity.New(db.Pool())
	accounts := identity.NewHTTP(users)
	resources := content.NewHTTP(content.New(db.Pool()), accounts.Authenticate)
	root := ""
	if len(artifactRoots) > 0 {
		root = artifactRoots[0]
	}
	directory, err := catalog.NewHTTP(root)
	if err != nil {
		return nil, err
	}
	return httpapi.New(version, db, accounts, resources, directory), nil
}
