package maniflex

import "fmt"

// resolveManyToMany is called once after all models are registered (in Handler).
// It performs two jobs:
//  1. Fills in ThroughTable/ThroughLocalFK/ThroughRemoteFK for any ManyToMany
//     stubs stored by scanFields (explicit through: tags).
//  2. Auto-detects junction models (models with exactly two BelongsTo relations
//     to distinct registered models) and bidirectionally registers ManyToMany on
//     both endpoint models — unless a relation already exists.
//
// The registry is mutated in-place; this is safe because it runs before the
// router is built (single-goroutine init path).
func resolveManyToMany(reg *Registry) error {
	models := reg.All()

	// ── Step 1: resolve explicit through: stubs ───────────────────────────────
	for _, meta := range models {
		for i := range meta.Relations {
			rel := &meta.Relations[i]
			if rel.Kind != ManyToMany || rel.ThroughModel == "" {
				continue
			}
			if rel.ThroughTable != "" {
				continue // already resolved (shouldn't happen at init, but be safe)
			}
			jMeta, ok := reg.Get(rel.ThroughModel)
			if !ok {
				return fmt.Errorf("maniflex: model %q has through:%q but %q is not registered",
					meta.Name, rel.ThroughModel, rel.ThroughModel)
			}
			localFK, remoteFK, err := junctionFKs(jMeta, meta.Name, rel.RelatedModel)
			if err != nil {
				return fmt.Errorf("maniflex: model %q through:%q: %w", meta.Name, rel.ThroughModel, err)
			}
			rel.ThroughTable = jMeta.TableName
			rel.ThroughLocalFK = localFK
			rel.ThroughRemoteFK = remoteFK
		}
	}

	// ── Step 2: register many-to-many for junction models ─────────────────────
	for _, jMeta := range models {
		if !isJunction(jMeta) {
			continue
		}
		sideA, sideB, _ := junctionSides(jMeta)

		metaA, okA := reg.Get(sideA.RelatedModel)
		metaB, okB := reg.Get(sideB.RelatedModel)
		if !okA || !okB {
			continue
		}

		// Register A→B (via junction) if not already present
		if err := addM2MIfMissing(metaA, metaB, jMeta, sideA.FKColumn, sideB.FKColumn); err != nil {
			return err
		}
		// Register B→A (via junction) if not already present
		if err := addM2MIfMissing(metaB, metaA, jMeta, sideB.FKColumn, sideA.FKColumn); err != nil {
			return err
		}
	}

	return nil
}

// addM2MIfMissing adds a ManyToMany relation from src to dst via junction,
// but only when src has no existing relation (of any kind) pointing to dst.
//
// The relation key uses pluralize() so auto-detected junctions produce the
// same key the explicit relation paths do — `people`, `categories`, `boxes`
// — instead of the naive `+"s"` form (`persons`, `categorys`, `boxs`) the
// previous implementation emitted.
func addM2MIfMissing(src, dst, junction *ModelMeta, localFK, remoteFK string) error {
	relKey := pluralize(toSnakeCase(dst.Name))
	if src.RelationByKey(relKey) != nil || src.RelationByModel(dst.Name) != nil {
		return nil // explicit relation already registered; don't override
	}
	src.Relations = append(src.Relations, RelationMeta{
		FieldName:       pluralize(dst.Name),
		DBName:          relKey,
		RelationKey:     relKey,
		RelatedModel:    dst.Name,
		ThroughTable:    junction.TableName,
		ThroughModel:    junction.Name,
		ThroughLocalFK:  localFK,
		ThroughRemoteFK: remoteFK,
		Kind:            ManyToMany,
	})
	return nil
}

// belongsToRelations returns all BelongsTo relations on m.
func belongsToRelations(m *ModelMeta) []RelationMeta {
	var out []RelationMeta
	for _, r := range m.Relations {
		if r.Kind == BelongsTo {
			out = append(out, r)
		}
	}
	return out
}

// junctionFKs inspects junction model j and returns the FK columns for
// localModel and remoteModel respectively. Returns an error if the junction
// does not have exactly one BelongsTo relation to each model.
func junctionFKs(j *ModelMeta, localModel, remoteModel string) (localFK, remoteFK string, err error) {
	for _, r := range j.Relations {
		if r.Kind != BelongsTo {
			continue
		}
		switch r.RelatedModel {
		case localModel:
			localFK = r.FKColumn
		case remoteModel:
			remoteFK = r.FKColumn
		}
	}
	if localFK == "" {
		return "", "", fmt.Errorf("junction %q has no BelongsTo relation to %q", j.Name, localModel)
	}
	if remoteFK == "" {
		return "", "", fmt.Errorf("junction %q has no BelongsTo relation to %q", j.Name, remoteModel)
	}
	return localFK, remoteFK, nil
}
