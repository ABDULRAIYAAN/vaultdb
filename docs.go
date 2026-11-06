// Package vaultdb holds the project's own docs and source, embedded so the
// browser build's docs assistant can answer questions about VaultDB.
package vaultdb

import "embed"

// Docs is the README plus the engine's source code.
//
//go:embed README.md internal/*/*.go
var Docs embed.FS
