// Package migrations embeds the SQL schema migrations so the binary and the
// tests apply exactly the same files.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
