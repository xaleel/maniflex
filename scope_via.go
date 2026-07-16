package maniflex

// scope_via.go — scoping a model through the parent that carries the column (R2).
//
// db.ForceFilter maps a field to a value: "an Order is yours if order.org_id is
// yours". That covers a model that carries a column to scope by, and says
// nothing about one that does not. The child tables — a DamagedItem, a CartLine,
// a StocktakeEntry — carry only a foreign key, and their scope lives on the
// other end of it: "a DamagedItem is yours if its Item is yours". The two ways
// out of that are to denormalise owner_id onto every child (a schema change, and
// a permanent sync burden on precisely the tables whose scoping is easiest to
// get wrong) or to drop those models out of declarative scoping and hand-write
// the predicate — which is the status quo the whole mechanism exists to replace.
//
// Neither is necessary, because the predicate engine already does this. A
// FilterExpr with IsNested set carries the relation's table and foreign key, the
// query builder emits the LEFT JOIN, and the write path's pre-flight read (P0-1)
// passes the same filters to FindByID, which joins them too. So the gap was never
// the engine: it was that ForceFilter's constructor could only spell an equality
// on a column of the model itself. ViaFilter spells the other one.

import "fmt"

// relationKindName renders a RelationKind for an error message. RelationKind is
// an int with no String method, so a %s of one is the number.
func relationKindName(k RelationKind) string {
	switch k {
	case BelongsTo:
		return "BelongsTo"
	case HasMany:
		return "HasMany"
	case ManyToMany:
		return "ManyToMany"
	}
	return fmt.Sprintf("RelationKind(%d)", int(k))
}

// ViaFilter builds a forced filter that scopes this request's model through one
// of its BelongsTo parents — the model has no column to scope by, so the scope is
// applied to the column its parent carries.
//
// relationKey names the relation, in the same vocabulary a nested ?filter= uses
// (?filter=author.status:neq:banned → "author"). parentField names the column on
// the parent model, by JSON or DB name. The result is AND-ed into the query like
// any other filter, and marked Forced, so it constrains updates and deletes as
// well as reads.
//
// db.ForceFilterVia is the shipped wrapper and is what most callers want; this is
// exported for middleware that needs the FilterExpr itself.
//
// It reports an error rather than returning a filter that would not do what it
// says: an unregistered relation, a HasMany (there is no foreign key on this row
// to join by), or an encrypted parent column (the stored value is ciphertext, so
// an equality against the plaintext scope value matches nothing). The caller must
// fail the request on an error — a scope that cannot be built is not a scope, and
// skipping it would leave the request unscoped rather than refused.
func (c *ServerContext) ViaFilter(relationKey, parentField string, value any) (*FilterExpr, error) {
	// An Action and the global search both run on a synthetic sentinel ModelMeta
	// — non-nil, named for the route, carrying no fields and no relations. Left to
	// the lookup below, that produces "model __action_POST_/orders/{id}/refund has
	// no relation \"item\"" and advice to go and tag a relation, which is a hunt
	// for a problem that is not there. Say the real one.
	if c.Operation == OpAction || c.Operation == OpSearch {
		return nil, fmt.Errorf(
			"maniflex: ViaFilter scopes a model through its parent, and %s runs on a synthetic "+
				"model with no relations to scope through — it belongs on the DB step, which runs "+
				"per model. Build the FilterExpr by hand (IsNested, RelationKey, RelationTable, "+
				"RelationFK, NestedField, Forced) and pass it to ctx.SetActionScope",
			c.Operation)
	}
	if c.Model == nil {
		return nil, fmt.Errorf(
			"maniflex: ViaFilter needs the request's model on the context, and there is none")
	}
	if c.reg == nil {
		return nil, fmt.Errorf("maniflex: registry not available on this ServerContext")
	}

	rel := c.Model.RelationByKey(relationKey)
	if rel == nil {
		return nil, fmt.Errorf(
			"maniflex: model %s has no relation %q to scope through — a relation is declared with "+
				"mfx:\"relation\" on the foreign key field (since v0.1.3 a bare <Name>ID column is "+
				"not one), and its key is the field name with ID stripped, e.g. ItemID → %q",
			c.Model.Name, relationKey, "item")
	}
	if rel.Kind != BelongsTo {
		return nil, fmt.Errorf(
			"maniflex: relation %q on model %s is %s, and only a BelongsTo can carry a scope — the "+
				"join needs a foreign key on this row pointing at the one row that owns it, and a "+
				"%s has no single owning row to point at",
			relationKey, c.Model.Name, relationKindName(rel.Kind), relationKindName(rel.Kind))
	}

	parent, ok := c.reg.Get(rel.RelatedModel)
	if !ok {
		return nil, fmt.Errorf(
			"maniflex: relation %q on model %s targets model %q, which is not registered",
			relationKey, c.Model.Name, rel.RelatedModel)
	}

	pf := parent.FieldByJSONName(parentField)
	if pf == nil {
		pf = parent.FieldByDBName(parentField)
	}
	if pf == nil {
		return nil, fmt.Errorf(
			"maniflex: field %q not found on model %s (the parent of %s.%s)",
			parentField, parent.Name, c.Model.Name, relationKey)
	}
	if pf.Tags.Encrypted {
		return nil, fmt.Errorf(
			"maniflex: field %q on model %s is encrypted, so it cannot carry a scope — the column "+
				"holds ciphertext and the comparison would be against the plaintext value, matching "+
				"no row and hiding the whole table rather than scoping it",
			parentField, parent.Name)
	}
	// Filterable is deliberately not required. It is the client's permission to
	// filter on a field, and this filter is not the client's — a scope column is
	// usually one they must never be able to filter by.

	return &FilterExpr{
		Field:         relationKey + "." + pf.Tags.DBName,
		Operator:      OpEq,
		Value:         value,
		IsNested:      true,
		RelationKey:   relationKey,
		RelationModel: rel.RelatedModel,
		RelationTable: parent.TableName,
		RelationFK:    rel.FKColumn,
		NestedField:   pf.Tags.DBName,
		Forced:        true,
	}, nil
}
