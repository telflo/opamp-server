// Package migrations holds the opamp-server's SQL migrations, embedded in
// the binary so an air-gapped install ships nothing beside it.
// `opamp-server migrate` applies them with goose; `serve` refuses to start
// unless the database is at the newest one.
//
// Each file is named NNNNN_<what>.sql, with goose's `-- +goose Up` and
// `-- +goose Down` sections, and runs in a transaction. Every migration must
// keep working with the previous server release (expand, then contract),
// because old servers keep serving while it goes in.
//
// There are two exceptions, each an edit in place before any production
// install. First, 00009's renumbering: 00009_imports.sql was deleted and
// 00010 renumbered to 00009 and edited in place, so a database migrated
// before the edit (at version 10) is refused by `serve` and must be dropped
// and bootstrapped again, never rolled back. The reverse fails too: a build
// from before the edit can't run on a database made after it (its
// `migrate up` fails: `relation "collector_instances" already exists`);
// that database must be dropped and bootstrapped again too. Second, the
// edits of migrations 7 and 9 that keep the groups' Kubernetes columns out:
// a development database migrated before them is dropped and bootstrapped
// again likewise. Expand, then contract stays the rule for every migration
// from here on.
package migrations

import "embed"

// FS holds every migration, at its root.
//
//go:embed *.sql
var FS embed.FS
