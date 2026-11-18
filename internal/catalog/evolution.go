package catalog

import (
	"fmt"
	"lamina/internal/datum"
)

// ColumnChange is one operation inside a single atomic schema evolution.
// Supported actions:
//   - add_column:   add a new column; non-null columns require a typed default
//   - set_default:  attach or replace the default interpretation of a column
//   - drop_default: remove a default; only nullable columns may become default-less
type ColumnChange struct {
	Action   string       `json:"action"`
	Column   string       `json:"column"`
	Type     string       `json:"type,omitempty"`
	Nullable bool         `json:"nullable,omitempty"`
	Default  *datum.Value `json:"default,omitempty"`
}

type EvolveRequest struct {
	Relation string         `json:"relation"`
	Changes  []ColumnChange `json:"changes"`
	Note     string         `json:"note,omitempty"`
}

// ValidateChangeShape checks request shape independent of the live relation.
// Shape failures are client input errors (HTTP 400).
func ValidateChangeShape(req EvolveRequest) error {
	if !ValidName(req.Relation) {
		return fmt.Errorf("invalid relation name")
	}
	if len(req.Changes) == 0 {
		return fmt.Errorf("evolution needs at least one change")
	}
	if len(req.Changes) > 64 {
		return fmt.Errorf("evolution change limit exceeded")
	}
	if len(req.Note) > 256 {
		return fmt.Errorf("evolution note exceeds 256 characters")
	}
	for _, change := range req.Changes {
		if !ValidName(change.Column) {
			return fmt.Errorf("invalid or missing change column")
		}
		switch change.Action {
		case "add_column":
			if !validColumnType(change.Type) {
				return fmt.Errorf("add_column requires a supported type")
			}
		case "set_default":
			if change.Default == nil || change.Default.Null() {
				return fmt.Errorf("set_default requires a non-null default; use drop_default")
			}
		case "drop_default":
		default:
			return fmt.Errorf("unsupported evolution action %q", change.Action)
		}
	}
	return nil
}

// ApplyChanges derives the post-evolution column list and reports semantic
// incompatibility against the current schema. Incompatibility failures are
// distinct from shape errors (HTTP 409). The returned slice is independent of
// the relation's backing array.
func ApplyChanges(r Relation, changes []ColumnChange) ([]Column, error) {
	columns := cloneColumns(r.Columns)
	indexOf := map[string]int{}
	for i, c := range columns {
		indexOf[c.Name] = i
	}
	for _, change := range changes {
		pos, exists := indexOf[change.Column]
		switch change.Action {
		case "add_column":
			if exists {
				return nil, fmt.Errorf("incompatible evolution: column %s already exists", change.Column)
			}
			if len(columns) >= 64 {
				return nil, fmt.Errorf("incompatible evolution: relation column limit 64 reached")
			}
			if change.Column == r.Primary {
				return nil, fmt.Errorf("incompatible evolution: primary column cannot be added")
			}
			column := Column{Name: change.Column, Type: change.Type, Nullable: change.Nullable, Default: nil}
			if change.Default != nil && !change.Default.Null() {
				if change.Default.Kind != column.Type {
					return nil, fmt.Errorf("incompatible evolution: default type does not match new column %s", change.Column)
				}
				column.Default = change.Default
			}
			if !column.Nullable && (column.Default == nil || column.Default.Null()) {
				return nil, fmt.Errorf("incompatible evolution: non-null column %s requires a typed default", change.Column)
			}
			if err := defaultValid(column); err != nil {
				return nil, fmt.Errorf("incompatible evolution: %w", err)
			}
			indexOf[column.Name] = len(columns)
			columns = append(columns, column)
		case "set_default":
			if !exists {
				return nil, fmt.Errorf("incompatible evolution: unknown column %s", change.Column)
			}
			column := columns[pos]
			if column.Name == r.Primary {
				return nil, fmt.Errorf("incompatible evolution: primary column cannot carry a default")
			}
			if change.Default.Kind != column.Type {
				return nil, fmt.Errorf("incompatible evolution: default type does not match column %s", change.Column)
			}
			column.Default = change.Default
			if err := defaultValid(column); err != nil {
				return nil, fmt.Errorf("incompatible evolution: %w", err)
			}
			columns[pos] = column
		case "drop_default":
			if !exists {
				return nil, fmt.Errorf("incompatible evolution: unknown column %s", change.Column)
			}
			column := columns[pos]
			if !column.Nullable {
				return nil, fmt.Errorf("incompatible evolution: dropping default of non-null column %s would strand historical rows", change.Column)
			}
			column.Default = nil
			columns[pos] = column
		}
	}
	return columns, nil
}

// PruneRevisions drops revisions that cannot bind any retained snapshot. The
// caller supplies a per-relation low-water mark (the vacuum horizon, raised to
// the oldest retained row when every version is newer). The revision effective
// at that watermark remains; strictly older revisions are superseded. Because
// columns are additive, rows still interpret correctly under the surviving
// revision. Returns the number of revisions removed.
func (c *Catalog) PruneRevisions(rowWatermark func(relation string) uint64) int {
	removed := 0
	for name, r := range c.Relations {
		r.EnsureRevisions()
		if len(r.Revisions) <= 1 {
			c.Relations[name] = r
			continue
		}
		watermark := rowWatermark(name)
		keep := 0
		for keep+1 < len(r.Revisions) && r.Revisions[keep+1].Sequence <= watermark {
			keep++
		}
		if keep == 0 {
			c.Relations[name] = r
			continue
		}
		pruned := append([]Revision(nil), r.Revisions[keep:]...)
		removed += keep
		r.Revisions = pruned
		c.Relations[name] = r
	}
	return removed
}

// RevisionCount totals retained schema revisions across all relations.
func (c *Catalog) RevisionCount() int {
	total := 0
	for _, r := range c.Relations {
		if len(r.Revisions) == 0 {
			total++
			continue
		}
		total += len(r.Revisions)
	}
	return total
}
