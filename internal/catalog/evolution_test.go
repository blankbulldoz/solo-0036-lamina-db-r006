package catalog

import (
	"lamina/internal/datum"
	"testing"
)

func intDefault(n int64) *datum.Value   { v := datum.Integer(n); return &v }
func textDefault(s string) *datum.Value { v := datum.Value{Kind: "text", Text: s}; return &v }

func baseRelation() Relation {
	r := Relation{
		Name:    "spectra",
		Primary: "sample",
		Columns: []Column{
			{Name: "sample", Type: "int"},
			{Name: "band", Type: "text"},
		},
		Created: 1,
	}
	r.EnsureRevisions()
	return r
}

func TestApplyChangesCompatible(t *testing.T) {
	r := baseRelation()
	columns, err := ApplyChanges(r, []ColumnChange{
		{Action: "add_column", Column: "note", Type: "text", Nullable: true},
		{Action: "add_column", Column: "tier", Type: "text", Nullable: false, Default: textDefault("std")},
	})
	if err != nil {
		t.Fatalf("compatible evolution rejected: %v", err)
	}
	if len(columns) != 4 {
		t.Fatalf("expected 4 columns, got %d", len(columns))
	}
	r.ApplyRevision(2, "evolve", "", columns)
	if got := len(r.Revisions); got != 2 {
		t.Fatalf("expected 2 revisions, got %d", got)
	}
	schema, ok := r.SchemaAt(1)
	if !ok || len(schema.Columns) != 2 {
		t.Fatalf("old snapshot must bind the original schema, got %+v ok=%v", schema.Columns, ok)
	}
	schema, ok = r.SchemaAt(2)
	if !ok || len(schema.Columns) != 4 {
		t.Fatalf("new snapshot must bind the evolved schema")
	}
}

func TestApplyChangesIncompatible(t *testing.T) {
	r := baseRelation()
	cases := []struct {
		name   string
		change ColumnChange
	}{
		{"duplicate column", ColumnChange{Action: "add_column", Column: "band", Type: "text", Nullable: true}},
		{"non-null missing default flag", ColumnChange{Action: "add_column", Column: "tier", Type: "text", Nullable: false}},
		{"non-null null default", ColumnChange{Action: "add_column", Column: "tier", Type: "text", Nullable: false, Default: &datum.Value{Kind: "null"}}},
		{"default wrong type", ColumnChange{Action: "add_column", Column: "tier", Type: "text", Nullable: false, Default: intDefault(3)}},
		{"primary default", ColumnChange{Action: "set_default", Column: "sample", Default: intDefault(1)}},
		{"set default unknown", ColumnChange{Action: "set_default", Column: "ghost", Default: intDefault(1)}},
		{"drop default non-null", ColumnChange{Action: "drop_default", Column: "band"}},
	}
	// mark band non-null (it is by default) and give it a default so the
	// drop_default case is meaningful.
	for i := range r.Columns {
		if r.Columns[i].Name == "band" {
			r.Columns[i].Default = textDefault("x")
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ApplyChanges(r, []ColumnChange{tc.change}); err == nil {
				t.Fatalf("expected incompatibility for %s", tc.name)
			}
		})
	}
}

func TestNormalizeAndInterpretDefaults(t *testing.T) {
	r := baseRelation()
	columns, err := ApplyChanges(r, []ColumnChange{
		{Action: "add_column", Column: "tier", Type: "text", Nullable: false, Default: textDefault("std")},
		{Action: "add_column", Column: "note", Type: "text", Nullable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.ApplyRevision(2, "evolve", "", columns)

	// A row written under the old schema has no tier/note keys.
	oldRow := datum.Row{"sample": datum.Integer(1), "band": datum.Value{Kind: "text", Text: "b1"}}
	current, ok := r.SchemaAt(2)
	if !ok {
		t.Fatal("schema at 2 missing")
	}
	projected, err := current.Interpret(oldRow)
	if err != nil {
		t.Fatalf("interpret old row: %v", err)
	}
	if projected["tier"].Text != "std" || !projected["note"].Null() {
		t.Fatalf("default backfill wrong: %+v", projected)
	}

	// New writes inherit the default when omitted, but keep explicit values.
	normalized, err := current.Normalize(datum.Row{"sample": datum.Integer(2), "band": datum.Value{Kind: "text", Text: "b2"}})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if normalized["tier"].Text != "std" {
		t.Fatalf("omitted column did not inherit default: %+v", normalized)
	}
	normalized, err = current.Normalize(datum.Row{
		"sample": datum.Integer(3), "band": datum.Value{Kind: "text", Text: "b3"},
		"tier": datum.Value{Kind: "text", Text: "gold"},
	})
	if err != nil || normalized["tier"].Text != "gold" {
		t.Fatalf("explicit value must win over default: %v %+v", err, normalized)
	}
}

func TestPruneRevisions(t *testing.T) {
	r := baseRelation()
	c2, _ := ApplyChanges(r, []ColumnChange{{Action: "add_column", Column: "a", Type: "int", Nullable: true}})
	r.ApplyRevision(3, "evolve", "", c2)
	c3, _ := ApplyChanges(r, []ColumnChange{{Action: "add_column", Column: "b", Type: "int", Nullable: true}})
	r.ApplyRevision(5, "evolve", "", c3)
	c := New()
	c.Relations[r.Name] = r

	// Watermark 4 keeps the revision effective at/under 4 (seq 3) and drops seq 1.
	removed := c.PruneRevisions(func(string) uint64 { return 4 })
	if removed != 1 {
		t.Fatalf("expected 1 pruned revision, got %d", removed)
	}
	pruned := c.Relations[r.Name]
	if pruned.Revisions[0].Sequence != 3 || len(pruned.Revisions) != 2 {
		t.Fatalf("unexpected retained revisions: %+v", pruned.Revisions)
	}
	// A snapshot inside retention but older than the oldest retained revision
	// falls back to the oldest retained revision rather than failing.
	schema, ok := pruned.SchemaAt(2)
	if !ok || len(schema.Columns) != 3 {
		t.Fatalf("expected additive fallback schema, got ok=%v cols=%d", ok, len(schema.Columns))
	}
	if _, ok := pruned.SchemaAt(0); ok {
		t.Fatal("snapshot before creation must not bind any schema")
	}
}
