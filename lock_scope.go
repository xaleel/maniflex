package maniflex

import (
	"fmt"
	"strings"
)

// LockScopeSpec records one mfx:"lock_scope:ModelName" directive resolved at
// registration time. The DB step acquires a FOR UPDATE lock on the referenced
// row before executing a create, preventing write-skew races on shared
// resources (pharmacy stock, seat inventory, etc.).
//
// Requires an active transaction on the request — register
// maniflex.WithTransaction(nil) on the Service step for the model.
type LockScopeSpec struct {
	// DBName is the DB column carrying the referenced row's ID.
	DBName string
	// Model is the registered model name to lock.
	Model string
}

// collectLockScopes gathers every lock_scope field on the model into
// m.LockScopes. Referenced model names are not validated here (the target
// model may not be registered yet); validateLockScopes in Handler() catches
// typos once the full registry is available.
func (m *ModelMeta) collectLockScopes() {
	for _, f := range m.Fields {
		if f.Tags.LockScope == "" {
			continue
		}
		m.LockScopes = append(m.LockScopes, LockScopeSpec{
			DBName: f.Tags.DBName,
			Model:  f.Tags.LockScope,
		})
	}
}

// validateLockScopes checks that every lock_scope directive across all
// registered models references a model that actually exists. Called once in
// Handler() after resolveManyToMany, so all models are present.
func validateLockScopes(reg *Registry) error {
	for _, model := range reg.All() {
		for _, ls := range model.LockScopes {
			if _, ok := reg.Get(ls.Model); !ok {
				return fmt.Errorf(
					"maniflex: model %q lock_scope on field %q references unknown model %q",
					model.Name, ls.DBName, ls.Model)
			}
		}
	}
	return nil
}

// collectRelationIssues reports the two problems a convention-inferred BelongsTo
// relation can have. Both used to be warnings; they differ in how certain the
// mistake is, so 10.1 treats them differently.
//
// A relation on a field whose name does not end in "ID" is always an error. The
// target model is derived by stripping that suffix, so without one the framework
// infers the target from the whole field name — which is almost never a real
// model, leaving a relation that resolves against nothing. There is no reading of
// that configuration under which the author got what they asked for, and the
// warning had promised to reject it for several releases.
//
// A relation whose target is simply not registered is reported only under
// Config.Strict, because it has a legitimate reading: the field may be a plain
// foreign id that wants no relation tag at all, and the FK column is created and
// usable either way.
func collectRelationIssues(reg *Registry, strict bool, issues *issueList) {
	for _, model := range reg.All() {
		for i := range model.Relations {
			checkInferredRelation(reg, model, &model.Relations[i], strict, issues)
		}
	}
}

// checkInferredRelation applies collectRelationIssues' two rules to one relation.
func checkInferredRelation(reg *Registry, model *ModelMeta, rel *RelationMeta,
	strict bool, issues *issueList,
) {
	if !rel.Convention || rel.Kind != BelongsTo {
		return
	}
	if !strings.HasSuffix(rel.FieldName, "ID") {
		issues.add("relation",
			"%s.%s is tagged mfx:\"relation\" but its name does not end in \"ID\", so the "+
				"target model was inferred from the whole field name as %q — write "+
				"mfx:\"relation:Target\" to name the target explicitly",
			model.Name, rel.FieldName, rel.RelatedModel)
	}
	if _, ok := reg.Get(rel.RelatedModel); ok {
		return
	}
	if strict {
		issues.addStrict("relation",
			"%s.%s is tagged mfx:\"relation\" but its target model %q is not registered — "+
				"register it, or remove the tag if this is a plain foreign id",
			model.Name, rel.FieldName, rel.RelatedModel)
	}
}
