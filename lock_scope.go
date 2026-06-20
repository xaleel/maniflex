package maniflex

import (
	"fmt"
	"log/slog"
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

// warnDanglingRelations logs a warning for every convention-inferred BelongsTo
// relation — a "<Name>ID" field auto-promoted to a relation — whose target model
// was never registered. This is almost always a microservice storing a foreign
// id by design (e.g. UserID when users live in another service); tagging the
// field mfx:"norelation" silences the inference. It only warns: a missing target
// is not fatal, the FK column is still created and usable.
func warnDanglingRelations(reg *Registry, logger *slog.Logger) {
	for _, model := range reg.All() {
		for i := range model.Relations {
			rel := &model.Relations[i]
			if !rel.Convention || rel.Kind != BelongsTo {
				continue
			}
			if _, ok := reg.Get(rel.RelatedModel); ok {
				continue
			}
			logger.Warn("[maniflex] convention relation targets an unregistered model — "+
				"tag the field mfx:\"norelation\" if it is a plain foreign id, not a relation",
				slog.String("model", model.Name),
				slog.String("field", rel.FieldName),
				slog.String("target_model", rel.RelatedModel))
		}
	}
}
