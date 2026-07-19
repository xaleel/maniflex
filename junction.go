package maniflex

// junction.go — explicit declaration of many-to-many join tables.
//
// Auto-detection treats a model with exactly two BelongsTo relations to distinct
// models as a join table. That shape is not exclusive to join tables — an entity
// with two foreign keys has it too — so detection silently registered a
// many-to-many between the endpoints of models that were never join tables
// (audit MS-L9). The classic case is Order{customer_id, shipping_address_id},
// which gained a Customer↔Address relation nobody declared.
//
// Two changes close it. Detection is narrowed to models that carry *nothing but*
// their two keys, which is the only shape where the guess is unambiguous; and a
// model that is a join table despite carrying columns of its own says so by
// embedding JunctionModel. Neither replaces the other: the narrow rule keeps the
// common case declaration-free, and the marker covers everything else.
//
// A junction that carries payload is not a lesser junction — it is what
// _through exists for. The point is only that its intent cannot be read off its
// shape, so it has to be stated.

import "reflect"

// JunctionModel marks a model as a many-to-many join table. Embed it alongside
// BaseModel, not instead of it — a junction is an ordinary model with an id, and
// everything that reads or writes one depends on that:
//
//	type ProductTag struct {
//	    maniflex.BaseModel
//	    maniflex.JunctionModel `mfx:"unique"`
//	    ProductID string `json:"product_id" mfx:"relation"`
//	    TagID     string `json:"tag_id"     mfx:"relation"`
//	    Position  int    `json:"position"`
//	}
//
// The model must have exactly two BelongsTo relations to distinct models;
// anything else is a registration error, since there is no pair to join.
//
// # mfx:"unique"
//
// Off by default, and deliberately so. Uniqueness on the key pair is the norm
// for a pure link table — Django, Rails and EF Core all default to it — but it
// is wrong for a junction that carries its own attributes, where the same pair
// may legitimately repeat:
//
//	Enrollment{student_id, course_id, term}       // same pair, different terms
//	Attendance{user_id, event_id, checked_in_at}  // many check-ins
//
// Nothing in the model's shape says which kind it is, which is the same reason
// detection had to be narrowed. Declaring it emits a UNIQUE index over the two
// key columns, and also lets an include collapse duplicate links: with the
// declaration a repeat is corruption, without it a repeat is data.
//
// Adding the tag to a table that already holds duplicate pairs fails the
// migration until they are cleaned up. That is why it is separate from the
// marker itself — embedding JunctionModel changes nothing about the schema, so
// declaring what a model *is* never risks a migration.
//
// # Deletes
//
// A junction's foreign keys default to ON DELETE CASCADE, so deleting either
// endpoint takes its link rows with it. A link to a row that no longer exists
// says nothing, and leaving it behind was how junction tables accumulated
// orphans (audit MS-L10). An explicit mfx:"on_delete:..." on the column wins.
type JunctionModel struct{}

var junctionModelType = reflect.TypeOf(JunctionModel{})

// junctionEmbed reports whether t embeds JunctionModel, and returns the mfx tag
// on that embed. Mirrors how mfx:"versioned" is read off the BaseModel embed.
func junctionEmbed(t reflect.Type) (tag string, found bool) {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.Anonymous && sf.Type == junctionModelType {
			return sf.Tag.Get("mfx"), true
		}
	}
	return "", false
}

// junctionSides returns the model's two BelongsTo relations when it has exactly
// two, to distinct models. Both the marker and auto-detection need this shape;
// anything else is not a join table.
func junctionSides(m *ModelMeta) (a, b RelationMeta, ok bool) {
	var bts []RelationMeta
	for _, r := range m.Relations {
		if r.Kind == BelongsTo {
			bts = append(bts, r)
		}
	}
	if len(bts) != 2 || bts[0].RelatedModel == bts[1].RelatedModel {
		return a, b, false
	}
	return bts[0], bts[1], true
}

// junctionPayloadColumns returns the columns a model carries beyond its two
// foreign keys and the framework-managed ones. A model with none of these is a
// pure link table and is safe to auto-detect; one with any of them has to
// declare itself, because the columns are exactly what an entity would have too.
func junctionPayloadColumns(m *ModelMeta, fkA, fkB string) []string {
	skip := map[string]bool{
		"id": true, "created_at": true, "updated_at": true, "deleted_at": true,
		fkA: true, fkB: true,
	}
	var out []string
	for i := range m.Fields {
		if col := m.Fields[i].Tags.DBName; !skip[col] {
			out = append(out, col)
		}
	}
	return out
}

// isJunction reports whether m should be treated as a many-to-many join table.
//
// Deliberately a pure function of the model rather than a flag set during
// resolution: AutoMigrate and validateRegistry are reached by different paths,
// and foreign-key emission must not depend on which ran first.
func isJunction(m *ModelMeta) bool {
	if m.Config.DisableAutoJunction {
		return false
	}
	a, b, ok := junctionSides(m)
	if !ok {
		return false
	}
	if m.Config.Junction {
		return true
	}
	// Auto-detected: only the unambiguous shape, nothing but the two keys.
	return len(junctionPayloadColumns(m, a.FKColumn, b.FKColumn)) == 0
}
