package store

import "embed"

// Migrations contains the embedded SQL migration files for goose.
// The files are in internal/store/migrations/*.sql.
//
//go:embed migrations/*.sql
var Migrations embed.FS
