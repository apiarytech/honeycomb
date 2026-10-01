package sqlstore_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/apiarytech/honeycomb"
	"github.com/apiarytech/honeycomb/store/sqlstore"
	plc "github.com/apiarytech/royaljelly/iec"
	_ "modernc.org/sqlite"
)

type testMotor struct {
	Speed   plc.REAL
	Running plc.BOOL
}

func (m *testMotor) TypeName() honeycomb.DataType { return "SQLStoreTestMotor" }

func openSQLite(t *testing.T, opts sqlstore.Options) *sqlstore.Store {
	t.Helper()
	opts.AutoSetup = true
	store, err := sqlstore.Open(context.Background(), "sqlite", "file::memory:", opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestSetupIsIdempotentAndTeardownDropsTables(t *testing.T) {
	ctx := context.Background()
	store := openSQLite(t, sqlstore.Options{})

	if err := store.Setup(ctx); err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if !tableExists(t, store.DB(), sqlstore.TagsTable) {
		t.Fatal("tags table missing after Setup")
	}

	if err := store.Teardown(ctx); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for _, table := range []string{sqlstore.TagsTable, sqlstore.MigrationsTable} {
		if tableExists(t, store.DB(), table) {
			t.Errorf("table %s still exists after Teardown", table)
		}
	}

	if err := store.Setup(ctx); err != nil {
		t.Fatalf("Setup after Teardown: %v", err)
	}
}

func TestSaveLoadDeleteAndInstanceIsolation(t *testing.T) {
	ctx := context.Background()
	store := openSQLite(t, sqlstore.Options{InstanceID: "plc1"})
	other, err := sqlstore.New(ctx, store.DB(), sqlstore.Options{Dialect: mustLookup(t, "sqlite"), InstanceID: "plc2"})
	if err != nil {
		t.Fatal(err)
	}

	rec := honeycomb.StoredTag{Name: "A", DataType: honeycomb.TypeDINT, Value: []byte("1"), Retain: true}
	if err := store.SaveTags(ctx, []honeycomb.StoredTag{rec}); err != nil {
		t.Fatal(err)
	}
	rec.Value = []byte("2")
	if err := store.SaveTags(ctx, []honeycomb.StoredTag{rec, {Name: "B", DataType: honeycomb.TypeBOOL}}); err != nil {
		t.Fatal(err)
	}

	tags, err := store.LoadTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].Name != "A" || string(tags[0].Value) != "2" || !tags[0].Retain || tags[1].Value != nil {
		t.Fatalf("unexpected tags after upsert: %+v", tags)
	}
	if otherTags, _ := other.LoadTags(ctx); len(otherTags) != 0 {
		t.Fatalf("instance plc2 sees plc1's tags: %+v", otherTags)
	}

	if err := store.DeleteTags(ctx, []string{"A", "missing"}); err != nil {
		t.Fatal(err)
	}
	if tags, _ = store.LoadTags(ctx); len(tags) != 1 || tags[0].Name != "B" {
		t.Fatalf("unexpected tags after delete: %+v", tags)
	}
	if err := store.Purge(ctx); err != nil {
		t.Fatal(err)
	}
	if tags, _ = store.LoadTags(ctx); len(tags) != 0 {
		t.Fatalf("unexpected tags after purge: %+v", tags)
	}
}

// configure builds the tag configuration a PLC program would declare at power-up.
func configure(t *testing.T) *honeycomb.TagDatabase {
	t.Helper()
	honeycomb.RegisterUDT(&testMotor{})
	db := honeycomb.NewTagDatabase()
	tags := []*honeycomb.Tag{
		{Name: "Counter", TypeInfo: &honeycomb.TypeInfo{DataType: honeycomb.TypeDINT}, Value: plc.DINT(0), Retain: true},
		{Name: "Setpoint", TypeInfo: &honeycomb.TypeInfo{DataType: honeycomb.TypeLREAL}, Value: plc.LREAL(0), Retain: true},
		{Name: "Motors", TypeInfo: &honeycomb.TypeInfo{DataType: honeycomb.TypeARRAY, ElementType: "SQLStoreTestMotor"},
			Value: []*testMotor{{}, {}}, Retain: true},
		{Name: "Scratch", TypeInfo: &honeycomb.TypeInfo{DataType: honeycomb.TypeDINT}, Value: plc.DINT(0)},
	}
	for _, tag := range tags {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestPersisterPowerCycle(t *testing.T) {
	ctx := context.Background()
	store := openSQLite(t, sqlstore.Options{})
	// The Persister closes its store at shutdown; keep the in-memory database
	// alive across the simulated power cycle by handing it a non-owning Store.
	shared := func() *sqlstore.Store {
		s, err := sqlstore.New(ctx, store.DB(), sqlstore.Options{Dialect: mustLookup(t, "sqlite")})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// First run: power-up with an empty store, run, shut down.
	db := configure(t)
	p, err := db.AttachStore(shared(), honeycomb.PersistOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	must(t, db.SetTagValue("Counter", plc.DINT(41)))
	must(t, p.Flush(ctx))
	must(t, db.SetTagValueQuality("Counter", plc.DINT(42), honeycomb.QualityBad))
	must(t, db.SetTagValue("Setpoint", plc.LREAL(12.5)))
	must(t, db.SetTagValue("Motors[1].Speed", plc.REAL(1500)))
	must(t, db.SetTagValue("Scratch", plc.DINT(7)))
	if _, err := db.SetTagForceValue("Setpoint", plc.LREAL(99)); err != nil {
		t.Fatal(err)
	}
	must(t, p.Close(ctx))

	// Second run: a fresh database with the same configuration.
	db = configure(t)
	p, err = db.AttachStore(shared(), honeycomb.PersistOptions{})
	if err != nil {
		t.Fatal(err)
	}
	must(t, p.Restore(ctx))
	defer p.Close(ctx)

	expect := map[string]any{
		"Counter":         plc.DINT(42),
		"Setpoint":        plc.LREAL(12.5), // the actual value, not the force value
		"Motors[1].Speed": plc.REAL(1500),
		"Scratch":         plc.DINT(0), // not Retain, so not persisted
	}
	for name, want := range expect {
		got, err := db.GetTagValue(name)
		if err != nil || got != want {
			t.Errorf("%s = %v (%v), want %v", name, got, err, want)
		}
	}
	// Good values may be stale after a power cycle; Bad values stay Bad.
	for name, want := range map[string]honeycomb.Quality{
		"Counter":  honeycomb.QualityBad,
		"Setpoint": honeycomb.QualityUncertain,
		"Scratch":  honeycomb.QualityUnknown,
	} {
		if got, err := db.GetTagQuality(name); err != nil || got != want {
			t.Errorf("%s quality = %v (%v), want %v", name, got, err, want)
		}
	}
	if forced, _ := db.GetTagForced("Setpoint"); forced {
		t.Error("force state restored although RestoreForces is false")
	}
}

func TestBuiltInDialects(t *testing.T) {
	cases := map[string]string{
		"sqlite":      "ON CONFLICT (instance_id) DO UPDATE SET tag_name",
		"postgres":    "VALUES ($1, $2, $3",
		"cockroachdb": "UPSERT INTO t (instance_id, tag_name, tag_value) VALUES ($1, $2, $3)",
		"mysql":       "ON DUPLICATE KEY UPDATE tag_name = VALUES(tag_name)",
		"sqlserver":   "WHEN NOT MATCHED THEN INSERT",
	}
	for name, fragment := range cases {
		d := mustLookup(t, name)
		upsert := d.Upsert("t", []string{"instance_id", "tag_name", "tag_value"}, []string{"instance_id"})
		if !strings.Contains(upsert, fragment) {
			t.Errorf("%s upsert %q does not contain %q", name, upsert, fragment)
		}
		migrations, err := sqlstore.LoadMigrations(d.Migrations())
		if err != nil || len(migrations) == 0 {
			t.Errorf("%s migrations: %v (found %d)", name, err, len(migrations))
		}
	}
	for _, alias := range []string{"sqlite3", "pgx", "mssql", "MySQL", "crdb"} {
		mustLookup(t, alias)
	}
}

func mustLookup(t *testing.T, name string) sqlstore.Dialect {
	t.Helper()
	d, err := sqlstore.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
