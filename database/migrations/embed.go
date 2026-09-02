// Package migrations exposes the ordered PostgreSQL schema migrations.
package migrations

import "embed"

// Files contains all PostgreSQL migrations.
//
//go:embed *.sql
var Files embed.FS
