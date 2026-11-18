package engine

import (
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"lamina/internal/query"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "db")
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func createAndSeed(t *testing.T, db *DB) uint64 {
	t.Helper()
	rel := catalog.Relation{
		Name:    "spectra",
		Primary: "sample",
		Columns: []catalog.Column{
			{Name: "sample", Type: "int"},
			{Name: "band", Type: "text"},
		},
	}
	if _, err := db.CreateRelation(rel); err != nil {
		t.Fatalf("create relation: %v", err)
	}
	tx, err := db.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Put(tx.ID, "spectra", datum.Row{
		"sample": datum.Integer(1),
		"band":   datum.Value{Kind: "text", Text: "b1"},
	}, false); err != nil {
		t.Fatal(err)
	}
	commit, err := db.Commit(tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	return commit.Sequence
}

// relations helper unwraps the catalog listing for assertions.
func mustRelation(t *testing.T, db *DB) catalog.Relation {
	t.Helper()
	relations, _, err := db.Relations()
	if err != nil {
		t.Fatal(err)
	}
	return relations[0]
}

func TestEvolveRelationDurableAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedSeq := createAndSeed(t, db)

	// Rejected evolution must not consume a sequence.
	before, _ := db.Stats()
	badReq := catalog.EvolveRequest{Relation: "spectra", Changes: []catalog.ColumnChange{
		{Action: "add_column", Column: "tier", Type: "text", Nullable: false},
	}}
	if _, err := db.EvolveRelation(badReq); err == nil {
		t.Fatal("non-null column without default must be rejected")
	}
	after, _ := db.Stats()
	if after.Sequence != before.Sequence {
		t.Fatalf("rejected evolution advanced sequence %d -> %d", before.Sequence, after.Sequence)
	}

	// Admitted evolution with a default.
	if _, err := db.EvolveRelation(catalog.EvolveRequest{
		Relation: "spectra",
		Changes:  []catalog.ColumnChange{{Action: "add_column", Column: "tier", Type: "text", Nullable: false, Default: textVal("std")}},
	}); err != nil {
		t.Fatalf("evolve: %v", err)
	}
	head, _ := db.Stats()

	// Current read interprets the historical row through the new default.
	res, err := db.Query(query.Request{Relation: "spectra"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["tier"].Text != "std" {
		t.Fatalf("historical row not defaulted: %+v", res.Rows)
	}

	// Old snapshot keeps the original two-column interpretation via the same entry.
	old, err := db.Query(query.Request{Relation: "spectra", At: &seedSeq})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := old.Rows[0]["tier"]; present {
		t.Fatalf("old snapshot must not expose the later column: %+v", old.Rows[0])
	}

	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen purely from the journal: revisions must rebuild.
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	stats, _ := db2.Stats()
	if stats.Sequence != head.Sequence {
		t.Fatalf("sequence changed across reopen: %d != %d", stats.Sequence, head.Sequence)
	}
	rel := mustRelation(t, db2)
	if len(rel.Revisions) != 2 {
		t.Fatalf("revisions lost across reopen: %+v", rel.Revisions)
	}
	res2, err := db2.Query(query.Request{Relation: "spectra"})
	if err != nil || res2.Rows[0]["tier"].Text != "std" {
		t.Fatalf("post-reopen interpretation wrong: %+v err=%v", res2.Rows, err)
	}
}

func TestEvolveBlocksStaleTransactionWrites(t *testing.T) {
	db := openTestDB(t)
	createAndSeed(t, db)
	tx, err := db.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EvolveRelation(catalog.EvolveRequest{
		Relation: "spectra",
		Changes:  []catalog.ColumnChange{{Action: "add_column", Column: "note", Type: "text", Nullable: true}},
	}); err != nil {
		t.Fatal(err)
	}
	err = db.Put(tx.ID, "spectra", datum.Row{
		"sample": datum.Integer(2), "band": datum.Value{Kind: "text", Text: "b2"},
	}, false)
	if err == nil {
		t.Fatal("write under a schema newer than the transaction snapshot must be rejected")
	}
	if e, ok := err.(*Error); !ok || e.Code != "conflict" {
		t.Fatalf("expected conflict code, got %v", err)
	}
}

func TestVacuumPrunesRevisionsAndRetiresSnapshots(t *testing.T) {
	db := openTestDB(t)
	seedSeq := createAndSeed(t, db)
	if _, err := db.EvolveRelation(catalog.EvolveRequest{
		Relation: "spectra",
		Changes:  []catalog.ColumnChange{{Action: "add_column", Column: "tier", Type: "text", Nullable: false, Default: textVal("std")}},
	}); err != nil {
		t.Fatal(err)
	}
	stats, _ := db.Stats()
	result, err := db.Vacuum(stats.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaRevisionsPruned < 1 {
		t.Fatalf("expected schema revision pruning, got %d", result.SchemaRevisionsPruned)
	}
	// Pre-horizon snapshot is retired through the normal 410 path.
	if _, err := db.Query(query.Request{Relation: "spectra", At: &seedSeq}); err == nil {
		t.Fatal("snapshot below horizon must be rejected")
	} else if e, ok := err.(*Error); !ok || e.Code != "snapshot" {
		t.Fatalf("expected snapshot error, got %v", err)
	}
	// Current rows remain readable through the surviving revision.
	res, err := db.Query(query.Request{Relation: "spectra"})
	if err != nil || res.Rows[0]["tier"].Text != "std" {
		t.Fatalf("post-vacuum read wrong: %+v err=%v", res.Rows, err)
	}
}

func textVal(s string) *datum.Value {
	v := datum.Value{Kind: "text", Text: s}
	return &v
}
