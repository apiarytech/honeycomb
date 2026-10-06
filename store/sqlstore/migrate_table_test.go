package sqlstore_test

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/apiarytech/honeycomb/store/sqlstore"
)

// Two schemas in one database keep their versions in their own tables.
func TestSetupTableKeepsVersionsApart(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "two.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	d, _ := sqlstore.Lookup("sqlite")
	if err := sqlstore.Setup(ctx, db, d); err != nil {
		t.Fatal(err)
	}
	od := migrationsOnly{Dialect: d, fs: fstest.MapFS{
		"0001_other.up.sql":   {Data: []byte("CREATE TABLE other_things (id INTEGER PRIMARY KEY)")},
		"0001_other.down.sql": {Data: []byte("DROP TABLE other_things")},
	}}
	if err := sqlstore.SetupTable(ctx, db, od, "other_schema_migrations"); err != nil {
		t.Fatalf("version 1 of another schema beside honeycomb's: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO other_things (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.TeardownTable(ctx, db, od, "other_schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "SELECT 1 FROM "+sqlstore.MigrationsTable); err != nil {
		t.Fatalf("honeycomb's versions went with the other schema's: %v", err)
	}
}

type migrationsOnly struct {
	sqlstore.Dialect
	fs fs.FS
}

func (m migrationsOnly) Migrations() fs.FS { return m.fs }
