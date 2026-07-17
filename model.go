package maniflex

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const TABLE_NAME_PREFIX = ""

// ─── Embeddable helpers ───────────────────────────────────────────────────────

// BaseModel provides the standard id / created_at / updated_at columns.
// Embed it in every model struct you register, else registering model fails
// `CreatedAt` and `UpdatedAt` are auto-injected.
// If edited here, make sure the names are edited in the `injectTimestamp` function
type BaseModel struct {
	ID        string    `json:"id"         db:"id"`
	CreatedAt time.Time `json:"created_at" db:"created_at" mfx:"readonly,sortable"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at" mfx:"readonly,sortable"`

	// ── framework-internal carriers (typed-models migration, Phase 1) ──
	// Unexported, so they are never DB columns (collectFields skips them) and
	// never serialized as struct fields. Populated only by the framework, via
	// the promoted *BaseModel methods below. Inert until later phases read them.
	present  map[string]struct{} // JSON keys present in the incoming write body (PATCH semantics)
	extra    map[string]any      // computed fields, locale companions, _through, ad-hoc response keys
	selectFn map[string]struct{} // when non-nil, response includes only these JSON names (+ id)
}

// recordMeta is the framework-internal contract every registered model satisfies
// through its embedded *BaseModel. The pipeline reaches it by asserting on the
// erased record pointer — no reflection, no cross-package unexported access:
//
//	if rm, ok := any(recordPtr).(recordMeta); ok { rm.mfxSetPresent(keys) }
type recordMeta interface {
	mfxSetPresent(keys map[string]struct{})
	mfxPresent() map[string]struct{}
	mfxExtra() map[string]any // lazily allocated
	mfxSetSelect(keys map[string]struct{})
	mfxSelect() map[string]struct{}
}

var _ recordMeta = (*BaseModel)(nil)

func (b *BaseModel) mfxSetPresent(keys map[string]struct{}) { b.present = keys }
func (b *BaseModel) mfxPresent() map[string]struct{}        { return b.present }

// mfxExtra returns the extra map, allocating it on first use so callers can
// write into it unconditionally.
func (b *BaseModel) mfxExtra() map[string]any {
	if b.extra == nil {
		b.extra = make(map[string]any)
	}
	return b.extra
}

func (b *BaseModel) mfxSetSelect(keys map[string]struct{}) { b.selectFn = keys }
func (b *BaseModel) mfxSelect() map[string]struct{}        { return b.selectFn }

// mfxExtraPeek returns the extra map without allocating it. Used by the Phase-3
// bridge (ExtraColumns) on the read path where allocating an empty map per
// record would be wasteful.
func (b *BaseModel) mfxExtraPeek() map[string]any { return b.extra }

// WithDeletedAt enables deleted_at-style soft deletion.
type WithDeletedAt struct {
	DeletedAt *time.Time `json:"deleted_at,omitempty" db:"deleted_at" mfx:"readonly,filterable"`
}

// WithIsDeleted enables is_deleted boolean-style soft deletion.
type WithIsDeleted struct {
	IsDeleted bool `json:"is_deleted" db:"is_deleted" mfx:"readonly,filterable,default:false"`
}

// SoftDeletable is satisfied by models that declare their own soft-delete config.
type SoftDeletable interface {
	SoftDeleteConfig() SoftDeleteConfig
}

func (WithDeletedAt) SoftDeleteConfig() SoftDeleteConfig {
	return SoftDeleteConfig{Enabled: true, Field: "deleted_at", FieldType: SoftDeleteTimestamp}
}
func (WithIsDeleted) SoftDeleteConfig() SoftDeleteConfig {
	return SoftDeleteConfig{Enabled: true, Field: "is_deleted", FieldType: SoftDeleteBool}
}

// ─── Relation ─────────────────────────────────────────────────────────────────

// RelationKind describes the direction of a relationship.
type RelationKind int

const (
	// BelongsTo: this model holds the FK. e.g. Post.UserID → User
	BelongsTo RelationKind = iota
	// HasMany: the related model holds the FK. e.g. User → []Post
	HasMany
	// ManyToMany: a junction table connects two models. e.g. Product ↔ Tag via ProductTag.
	ManyToMany
)

// RelationMeta describes one relationship on a model.
type RelationMeta struct {
	// FieldName is the Go FK field name for BelongsTo (e.g. "ManagerID")
	// or the slice field name for HasMany (e.g. "Posts").
	FieldName string

	// DBName is the snake_case version of FieldName.
	DBName string

	// FKColumn is the DB column carrying the foreign key.
	//   BelongsTo → column on THIS table (e.g. "manager_id")
	//   HasMany   → column on the RELATED table (e.g. "team_id")
	FKColumn string

	// RelationKey is the short key used in ?include= and nested ?filter=.
	//   Explicit: snake_case of the Relation tag value, e.g. "manager"
	//   Convention: snake_case of trimmed field name, e.g. "user"
	RelationKey string

	// CompanionField is the Go struct field name of the companion placeholder
	// (e.g. "Manager" for a ManagerID FK). Empty for convention-style or HasMany.
	CompanionField string

	// RelatedModel is the target model's struct name, e.g. "User".
	RelatedModel string

	// Convention is true when this BelongsTo was inferred from a "<Name>ID" field
	// name rather than an explicit mfx:"relation:X" tag. Used to warn (not error)
	// when such an inferred relation targets a model that was never registered —
	// the common microservice case of storing a foreign id (e.g. UserID) by
	// design, which mfx:"norelation" silences.
	Convention bool

	Kind     RelationKind
	OnDelete OnDeleteAction

	// ManyToMany-only fields. All three are populated by resolveManyToMany.
	ThroughTable    string // junction table, e.g. "product_tags"
	ThroughLocalFK  string // FK on junction pointing to THIS model, e.g. "product_id"
	ThroughRemoteFK string // FK on junction pointing to the related model, e.g. "tag_id"
	ThroughModel    string // junction model name, e.g. "ProductTag"
}

// ─── Field ────────────────────────────────────────────────────────────────────

// FieldMeta describes one scalar (non-relation) DB column field.
type FieldMeta struct {
	Name  string       // Go struct field name
	Type  reflect.Type // Go type
	Tags  FieldTags    // parsed mfx/json/db tags
	Index []int        // reflect index path (supports embedded structs)
}

// ─── Model ────────────────────────────────────────────────────────────────────

// IndexSpec describes a database index to create during AutoMigrate.
type IndexSpec struct {
	// Name is the unique index identifier, e.g. "idx_invoice_history_record_id".
	Name string
	// Columns are the column expressions to index, e.g. ["record_id", "version DESC"].
	// Each entry is emitted verbatim inside the ON table(...) clause.
	Columns []string
	// Unique adds the UNIQUE keyword.
	Unique bool
}

// SchedAction is the action a scheduled time-driven transition applies to a
// row once its timestamp column falls in the past (8.6).
type SchedAction uint8

const (
	// SchedSoftDelete soft-deletes the row (model must be soft-deletable).
	SchedSoftDelete SchedAction = iota
	// SchedHardDelete physically deletes the row.
	SchedHardDelete
	// SchedSetField writes a fixed value into a sibling column.
	SchedSetField
)

// ScheduledSpec is one resolved scheduled field of a model (8.6).
type ScheduledSpec struct {
	Column  string      // DB column of the *time.Time field that drives it
	Action  SchedAction // resolved action
	Field   string      // target DB column          (SchedSetField only)
	From    string      // guard value                (SchedSetField only)
	HasFrom bool        // whether From was specified  (SchedSetField only)
	To      string      // value to write             (SchedSetField only)
}

// ModelMeta holds all reflection-derived and user-supplied metadata for a
// registered model. Built once at registration; treated as read-only afterwards.
//
// Fields and Relations are indexed by name to back the accessors below, so the
// one mutation they do not tolerate is renaming an entry in place after it has
// been looked up once — the index keys off DBName/JSONName/RelationKey/
// RelatedModel and would keep resolving the old name. Appending is fine (that
// is how resolveManyToMany adds its relations when the router is built).
type ModelMeta struct {
	Name       string
	GoType     reflect.Type
	TableName  string
	Fields     []FieldMeta    // scalar DB column fields
	Relations  []RelationMeta // FK and slice-based relations
	SoftDelete SoftDeleteConfig
	Config     ModelConfig
	Indices    []IndexSpec // extra DB indexes created during AutoMigrate

	// LockWhen aggregates all `lock_when:field=value` directives across the
	// model's fields. Populated at registration via collectLockWhen. The
	// default validate step checks these on Update/Delete: if any condition
	// matches the currently-stored record, the request is rejected with 422
	// RECORD_LOCKED. Empty slice means the model is never locked.
	LockWhen []LockCondition

	// LockScopes aggregates all `lock_scope:ModelName` directives across the
	// model's fields. Populated at registration via collectLockScopes. The DB
	// step acquires a FOR UPDATE lock on each referenced row before executing a
	// create. Requires an active transaction. Empty slice means no auto-locking.
	LockScopes []LockScopeSpec

	// Computed holds per-model virtual fields registered via
	// Server.AddComputedField. They are materialised in the Response step
	// after toJSONMap and cannot be filtered or sorted — read-only output
	// only. Mutated under mu; readers should snapshot the slice header under
	// the read lock before iterating to avoid racing with AddComputedField.
	Computed []ComputedField
	mu       sync.RWMutex

	// Adapter overrides the global Config.DB for this model. nil means use
	// the global. Copied from ModelConfig.Adapter at registration.
	Adapter DBAdapter

	// CursorField is the resolved DB column for keyset (cursor) pagination, or
	// "" when the model only supports offset pagination. Resolved at registration
	// from ModelConfig.CursorField or an mfx:"cursor_field:..." field tag.
	CursorField string

	// SearchFields are the DB column names of every mfx:"searchable" field, in
	// declaration order. Non-empty enables full-text search (?q=) on the model:
	// AutoMigrate provisions the driver's native FTS index (Postgres tsvector
	// column + GIN, SQLite FTS5 shadow table) over these columns. Resolved at
	// registration by collectSearchFields, which also rejects non-string fields.
	SearchFields []string

	scheduled []ScheduledSpec // resolved mfx:"scheduled" fields (8.6)

	// idx indexes Fields and Relations by the names the accessors below look
	// them up by. It is built on first lookup rather than at registration so
	// that a ModelMeta assembled by hand — the history model, a test fixture —
	// is indexed on the same terms as a scanned one.
	idx atomic.Pointer[modelIndex]
}

// modelIndex resolves a lookup name to a position in Fields or Relations. It
// holds positions rather than pointers because both slices are appended to
// after they are first indexed (resolveManyToMany adds relations when the
// router is built), and an append that reallocates the backing array would
// leave a stored pointer addressing the old one. A position stays valid.
type modelIndex struct {
	// The slice lengths this index was built from. index() compares them
	// against the live lengths and rebuilds when either has grown, so an
	// appender does not have to know the index exists.
	nFields, nRelations int

	fieldByDB   map[string]int
	fieldByJSON map[string]int
	relByKey    map[string]int
	relByModel  map[string]int
}

// index returns the lookup index, building it if it is missing or has fallen
// behind an append. Two callers racing here build equal indexes from the same
// slices, so whichever store lands last is the one either would have produced.
func (m *ModelMeta) index() *modelIndex {
	ix := m.idx.Load()
	if ix != nil && ix.nFields == len(m.Fields) && ix.nRelations == len(m.Relations) {
		return ix
	}
	return m.buildIndex()
}

func (m *ModelMeta) buildIndex() *modelIndex {
	ix := &modelIndex{
		nFields:     len(m.Fields),
		nRelations:  len(m.Relations),
		fieldByDB:   make(map[string]int, len(m.Fields)),
		fieldByJSON: make(map[string]int, len(m.Fields)),
		relByKey:    make(map[string]int, len(m.Relations)),
		relByModel:  make(map[string]int, len(m.Relations)),
	}
	for i := range m.Fields {
		indexFirst(ix.fieldByDB, m.Fields[i].Tags.DBName, i)
		indexFirst(ix.fieldByJSON, m.Fields[i].Tags.JSONName, i)
	}
	for i := range m.Relations {
		indexFirst(ix.relByKey, m.Relations[i].RelationKey, i)
		indexFirst(ix.relByModel, m.Relations[i].RelatedModel, i)
	}
	m.idx.Store(ix)
	return ix
}

// indexFirst records pos under name only when name is new, so a duplicate name
// resolves to the first entry declaring it — what a linear scan returns. A
// model can carry two fields with the same DB column (an embed shadowing a
// BaseModel one), and the rest of the framework is built around first-wins.
func indexFirst(m map[string]int, name string, pos int) {
	if _, seen := m[name]; !seen {
		m[name] = pos
	}
}

// ResolveAdapter returns the adapter to use for this model: the per-model
// override if set, otherwise the supplied global. Both may be nil; callers
// must check.
func (m *ModelMeta) ResolveAdapter(global DBAdapter) DBAdapter {
	if m.Adapter != nil {
		return m.Adapter
	}
	return global
}

// Scheduled returns the resolved scheduled-transition specs for the model.
// A nil/empty slice means there is nothing for a scheduled.Runner to sweep.
func (m *ModelMeta) Scheduled() []ScheduledSpec { return m.scheduled }

// HasScheduled reports whether the model declares any valid scheduled field.
func (m *ModelMeta) HasScheduled() bool { return len(m.scheduled) > 0 }

// rejectHiddenRequired refuses a field tagged both hidden and required. The two
// directives contradict: hidden keeps the client from sending the field (it
// implies readonly, so the write path strips it), required insists the client
// does. Nothing can satisfy that pair, and the runtime symptom actively
// misleads — the NOT NULL column rejects every insert with "<field> is
// required", including the requests that supplied it.
//
// writeonly is the directive for a value the client must supply but must never
// read back — a password — so the error names it.
// rejectPresignedWithoutFile refuses mfx:"upload:presigned" on a field that is
// not mfx:"file".
//
// upload:presigned says how a file field's bytes get to storage. A field that
// stores no file has no bytes and mounts no upload route, so the directive would
// simply not apply — and a protective-looking directive that quietly does nothing
// is the exact shape v0.2.3 stopped tolerating for a misspelt option. The tag
// parses, so the unknown-option check cannot catch this one; it is caught here.
func (m *ModelMeta) rejectPresignedWithoutFile() error {
	for _, f := range m.Fields {
		if f.Tags.PresignedUpload && !f.Tags.File {
			return fmt.Errorf(
				"maniflex: model %q field %q is mfx:\"upload:presigned\" but not mfx:\"file\" — "+
					"upload:presigned chooses how a file field's bytes reach storage, so on a "+
					"field that stores no file it does nothing. Add file, or drop upload:presigned",
				m.Name, f.Name)
		}
	}
	return nil
}

// fileKeysType is the reflect.Type of FileKeys, the multi-key file column.
var fileKeysType = reflect.TypeOf(FileKeys(nil))

// isFileFieldType reports whether t is a Go type an mfx:"file" field may have:
// a string (one storage key) or a FileKeys (many).
func isFileFieldType(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.String || t == fileKeysType
}

// rejectBadFileFieldType refuses mfx:"file" on a field that is neither a string
// nor a FileKeys.
//
// This closes a silent hole rather than a cosmetic one. A bare []string field
// fails loudly at AutoMigrate ("no SQL column mapping"), which is fine — but the
// documented way around that error is to wrap it in a named SQLTyper, and such a
// field then migrates cleanly, parses mfx:"file" cleanly, and is skipped by every
// single file path: the existence/max_size/accept check, file_acl signing,
// auto_delete GC and hard-delete cleanup each assert .(string) and fall through
// with no else. So the field looks protected and enforces nothing — strictly
// worse than the loud error it was routed around. FileKeys exists so that shape
// has a supported form; anything else is refused here.
func (m *ModelMeta) rejectBadFileFieldType() error {
	for _, f := range m.Fields {
		if !f.Tags.File || isFileFieldType(f.Type) {
			continue
		}
		return fmt.Errorf(
			"maniflex: model %q field %q is mfx:\"file\" but its Go type is %s — a file "+
				"field stores a storage key (string), or maniflex.FileKeys for many. Every "+
				"file rule (existence, max_size, accept, file_acl, auto_delete) is keyed on "+
				"that, so on any other type they would all be silently skipped",
			m.Name, f.Name, f.Type)
	}
	return nil
}

// rejectBadMaxCount refuses a malformed mfx:"max_count:N", and max_count on a
// field it cannot bound.
//
// The parser marks a malformed value -1 rather than swallowing it the way min:
// and max: swallow theirs, because max_count is protective: mfx:"max_count:1O"
// would otherwise widen the cap from 10 to the default 100 in silence. On a
// single-key file field the option bounds nothing, and on a non-file field it
// means nothing at all.
func (m *ModelMeta) rejectBadMaxCount() error {
	for _, f := range m.Fields {
		if f.Tags.MaxCount == 0 {
			continue // unset — DefaultMaxFileCount applies
		}
		if f.Tags.MaxCount < 0 {
			return fmt.Errorf(
				"maniflex: model %q field %q has a malformed mfx:\"max_count:\" value — "+
					"it takes a positive whole number, e.g. mfx:\"max_count:20\"",
				m.Name, f.Name)
		}
		if f.Type != fileKeysType {
			return fmt.Errorf(
				"maniflex: model %q field %q is mfx:\"max_count\" but its Go type is %s — "+
					"max_count bounds how many keys a maniflex.FileKeys field holds, so "+
					"here it would bound nothing",
				m.Name, f.Name, f.Type)
		}
	}
	return nil
}

func (m *ModelMeta) rejectHiddenRequired() error {
	for _, f := range m.Fields {
		if f.Tags.Hidden && f.Tags.Required && !f.Tags.WriteOnly {
			return fmt.Errorf(
				"maniflex: model %q field %q is both mfx:\"hidden\" and mfx:\"required\" — "+
					"hidden means the client cannot send the field, so a required check on it "+
					"can never pass. Use mfx:\"writeonly,required\" for a value the client must "+
					"supply but never reads back, or drop required if the server populates it",
				m.Name, f.Name)
		}
	}
	return nil
}

func (m *ModelMeta) FieldByDBName(name string) *FieldMeta {
	if i, ok := m.index().fieldByDB[name]; ok {
		return &m.Fields[i]
	}
	return nil
}

func (m *ModelMeta) FieldByJSONName(name string) *FieldMeta {
	if i, ok := m.index().fieldByJSON[name]; ok {
		return &m.Fields[i]
	}
	return nil
}

func (m *ModelMeta) RelationByKey(key string) *RelationMeta {
	if i, ok := m.index().relByKey[key]; ok {
		return &m.Relations[i]
	}
	return nil
}

func (m *ModelMeta) RelationByModel(name string) *RelationMeta {
	if i, ok := m.index().relByModel[name]; ok {
		return &m.Relations[i]
	}
	return nil
}

// IsFileList reports whether this field is an mfx:"file" column holding many
// storage keys (maniflex.FileKeys) rather than one.
//
// The single-key shape is what mounts an attachment route (GET /{model}/{id}/
// {field} streams one object) and what multipart can populate (one file per
// part). A list does neither, so the paths that assume one key ask this first.
func (f FieldMeta) IsFileList() bool {
	return f.Tags.File && f.Type == fileKeysType
}

// FileFields returns all fields with the File tag set.
func (m *ModelMeta) FileFields() []FieldMeta {
	var out []FieldMeta
	for _, f := range m.Fields {
		if f.Tags.File {
			out = append(out, f)
		}
	}
	return out
}

// HasFileFields reports whether the model has any mfx:"file" fields.
func (m *ModelMeta) HasFileFields() bool {
	for _, f := range m.Fields {
		if f.Tags.File {
			return true
		}
	}
	return false
}

// EncryptedFields returns all fields marked mfx:"encrypted".
func (m *ModelMeta) EncryptedFields() []FieldMeta {
	var out []FieldMeta
	for _, f := range m.Fields {
		if f.Tags.Encrypted {
			out = append(out, f)
		}
	}
	return out
}

// HasEncryptedFields reports whether the model has any mfx:"encrypted" fields.
func (m *ModelMeta) HasEncryptedFields() bool {
	for _, f := range m.Fields {
		if f.Tags.Encrypted {
			return true
		}
	}
	return false
}

func (m *ModelMeta) FilterableDBNames() map[string]bool {
	out := make(map[string]bool)
	for _, f := range m.Fields {
		if f.Tags.Filterable {
			out[f.Tags.DBName] = true
		}
	}
	return out
}

// ─── ScanModel ────────────────────────────────────────────────────────────────

// ScanModel inspects a struct value (or pointer) and returns a populated ModelMeta.
func ScanModel(v any, cfg ModelConfig) (*ModelMeta, error) {
	t := reflect.TypeOf(v)
	if t == nil {
		return nil, fmt.Errorf("maniflex: model cannot be nil")
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("maniflex: model must be a struct, got %s", t.Kind())
	}

	base := reflect.TypeOf(BaseModel{})
	for i := range base.NumField() {
		field := base.Field(i)
		if _, ok := t.FieldByName(field.Name); !ok {
			return nil, fmt.Errorf("maniflex: model must embed BaseModel")
		}
	}

	tableName := cfg.TableName
	if tableName == "" {
		tableName = tableNameFromModelName(t.Name())
	}

	meta := &ModelMeta{
		Name:      t.Name(),
		GoType:    t,
		TableName: TABLE_NAME_PREFIX + tableName,
		Config:    cfg,
		Indices:   append([]IndexSpec(nil), cfg.Indices...), // copy user-declared indices
		Adapter:   cfg.Adapter,
	}

	if cfg.SoftDelete.Enabled {
		meta.SoftDelete = cfg.SoftDelete
	} else if sd, ok := reflect.New(t).Interface().(SoftDeletable); ok {
		meta.SoftDelete = sd.SoftDeleteConfig()
	}

	// Detect versioned / versioned:diff_only on the embedded BaseModel field.
	if !cfg.Versioned {
		baseType := reflect.TypeOf(BaseModel{})
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if sf.Anonymous && sf.Type == baseType {
				ct := sf.Tag.Get("mfx")
				if ct == "versioned" {
					meta.Config.Versioned = true
				} else if ct == "versioned:diff_only" {
					meta.Config.Versioned = true
					meta.Config.VersionedDiffOnly = true
				}
				break
			}
		}
	}

	// Detect cursor_field on the embedded BaseModel field, e.g.
	// `maniflex.BaseModel `mfx:"cursor_field:created_at"``. collectFields recurses
	// into the embed without parsing its own tag, so resolve it here into
	// ModelConfig.CursorField (an explicit config value wins).
	if meta.Config.CursorField == "" {
		baseType := reflect.TypeOf(BaseModel{})
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if sf.Anonymous && sf.Type == baseType {
				if cf := parseFieldTags(sf).CursorField; cf != "" {
					meta.Config.CursorField = cf
				}
				break
			}
		}
	}

	if err := scanFields(t, meta, nil); err != nil {
		return nil, err
	}

	if meta.FieldByDBName("id") == nil {
		return nil, fmt.Errorf(
			"maniflex: model %q has no field with db column \"id\" (embed maniflex.BaseModel)", meta.Name)
	}

	// Singleton models auto-provision their single row from column defaults, so
	// a required field (no DB default, NOT NULL) would make the lazy insert
	// fail with an opaque 500 on the first request. Reject it at registration.
	if meta.Config.Singleton {
		for _, f := range meta.Fields {
			if f.Tags.Required {
				return nil, fmt.Errorf(
					"maniflex: singleton model %q field %q cannot be mfx:\"required\" — "+
						"the singleton row is auto-provisioned with column defaults", meta.Name, f.Name)
			}
		}
	}

	// hidden and required cannot both hold: hidden means the client may not send
	// the field, required means it must. The write path strips it and the NOT NULL
	// column then rejects the insert, so every create fails with "<field> is
	// required" — including the ones that did send it. Same shape as the singleton
	// case above: an unsatisfiable requirement, caught here rather than at runtime.
	if err := meta.rejectHiddenRequired(); err != nil {
		return nil, err
	}

	// upload:presigned configures how a file field's bytes arrive, so on a field
	// that stores no file it configures nothing — and would do so in silence,
	// which is the failure mode the unknown-option error exists to end.
	if err := meta.rejectPresignedWithoutFile(); err != nil {
		return nil, err
	}

	// Every file rule is keyed on the column being a storage key, so a file field
	// of any other type has them all silently skipped rather than applied.
	if err := meta.rejectBadFileFieldType(); err != nil {
		return nil, err
	}

	// max_count is protective, so a malformed or misplaced one must not pass as
	// a wider cap than the author wrote.
	if err := meta.rejectBadMaxCount(); err != nil {
		return nil, err
	}

	// Aggregate `lock_when:field=value` directives across the model's fields.
	// We resolve the referenced JSON name now so a typo (`lock_when:satus=…`)
	// is caught at registration rather than silently never matching.
	if err := meta.collectLockWhen(); err != nil {
		return nil, err
	}

	// Aggregate `lock_scope:ModelName` directives. The referenced model name is
	// not validated here — validateLockScopes in Handler() checks it once all
	// models are registered.
	meta.collectLockScopes()

	// Resolve the keyset (cursor) pagination field from ModelConfig.CursorField
	// or an mfx:"cursor_field:..." tag. A typo is caught here rather than
	// surfacing as a 500 on the first ?cursor= request.
	if err := meta.collectCursorField(); err != nil {
		return nil, err
	}

	// Resolve mfx:"searchable" fields into SearchFields and validate them, so a
	// non-text searchable field or an invalid SearchLanguage is caught at
	// registration rather than at the first ?q= request / AutoMigrate.
	if err := meta.collectSearchFields(); err != nil {
		return nil, err
	}

	// GlobalSearchable (the built-in /search endpoint opt-in) is meaningless
	// without searchable fields to fan out over — catch the misconfiguration at
	// registration rather than silently never returning the model from /search.
	if meta.Config.GlobalSearchable && len(meta.SearchFields) == 0 {
		return nil, fmt.Errorf(
			"maniflex: model %q sets GlobalSearchable but declares no mfx:\"searchable\" fields",
			meta.Name)
	}

	// Append one IndexSpec per mfx:"index" field so AutoMigrate creates the
	// index. Runs before buildScheduled so a scheduled column that also carries
	// mfx:"index" is deduplicated by the scheduled auto-index pass.
	meta.buildIndices()

	// Resolve mfx:"scheduled" fields once at scan time so the runner does no
	// reflection per tick. Invalid config is warned-and-dropped (non-strict
	// mode; §10.1 will make it panic instead).
	meta.buildScheduled()

	return meta, nil
}

var timePtrType = reflect.TypeOf((*time.Time)(nil))

// buildScheduled resolves and validates every mfx:"scheduled" field on the
// model, populating meta.scheduled and appending one auto-IndexSpec per
// surviving scheduled column. See "No implicit action" in the 8.6 plan.
func (m *ModelMeta) buildScheduled() {
	// Strict mode (§10.1) is not yet wired; 8.6 always warns-and-ignores.
	const strict = false

	var specs []ScheduledSpec
	for _, f := range m.Fields {
		if !f.Tags.Scheduled {
			continue
		}
		if spec, ok := resolveSchedSpec(m, f, strict); ok {
			specs = append(specs, spec)
		}
	}
	m.scheduled = specs

	// Check 9 — warn when two scheduled specs target the same field= and the
	// later one omits from= (a chained transition's later step needs a guard).
	seenTarget := make(map[string]bool)
	for _, s := range specs {
		if s.Action != SchedSetField {
			continue
		}
		if seenTarget[s.Field] && !s.HasFrom {
			slog.Default().Warn(
				"maniflex: two scheduled fields target the same column and the later one omits from= — chained transitions may double-apply",
				slog.String("model", m.Name), slog.String("field", s.Field))
		}
		seenTarget[s.Field] = true
	}

	// Auto-index every surviving scheduled column for sweep efficiency, unless
	// the user already declared an index on that column.
	for _, s := range specs {
		if m.hasIndexOn(s.Column) {
			continue
		}
		m.Indices = append(m.Indices, IndexSpec{
			Name:    "idx_" + m.TableName + "_" + s.Column,
			Columns: []string{s.Column},
		})
	}
}

// buildIndices appends one IndexSpec per mfx:"index" field, so AutoMigrate
// creates a plain (non-unique) index on that column (§10.4). A column already
// covered by an index is skipped to avoid a redundant duplicate: a user-declared
// ModelConfig.Indices entry or a scheduled auto-index (both via hasIndexOn), or
// the column's own mfx:"unique" constraint, which the database indexes
// implicitly. The index is named idx_<table>_<column>, matching the scheduled
// auto-index convention.
func (m *ModelMeta) buildIndices() {
	for _, f := range m.Fields {
		if !f.Tags.Index || f.Tags.Unique {
			continue
		}
		if m.hasIndexOn(f.Tags.DBName) {
			continue
		}
		m.Indices = append(m.Indices, IndexSpec{
			Name:    "idx_" + m.TableName + "_" + f.Tags.DBName,
			Columns: []string{f.Tags.DBName},
		})
	}
}

// hasIndexOn reports whether an index covering exactly the single column col
// is already declared on the model.
func (m *ModelMeta) hasIndexOn(col string) bool {
	for _, idx := range m.Indices {
		if len(idx.Columns) != 1 {
			continue
		}
		// Columns may carry a direction hint ("col DESC"); compare the bare name.
		name := idx.Columns[0]
		if sp := strings.SplitN(name, " ", 2); len(sp) > 0 {
			name = sp[0]
		}
		if name == col {
			return true
		}
	}
	return false
}

// reportSchedIssue records an invalid-scheduled-config problem. In strict mode
// (§10.1) it panics like every other fatal invalid-tag case; in the non-strict
// mode 8.6 ships it logs a warning and the caller drops the field.
func reportSchedIssue(strict bool, model, field, msg string) {
	full := fmt.Sprintf("maniflex: model %q field %q: %s", model, field, msg)
	if strict {
		panic(full)
	}
	slog.Default().Warn("maniflex: invalid scheduled tag — field ignored",
		slog.String("model", model),
		slog.String("field", field),
		slog.String("problem", msg))
}

// resolveSchedSpec validates one scheduled field and returns its resolved spec.
// On any invalid-config problem it reports the issue and returns ok=false so
// the caller drops the field from meta.scheduled.
func resolveSchedSpec(m *ModelMeta, f FieldMeta, strict bool) (ScheduledSpec, bool) {
	t := f.Tags
	report := func(msg string) { reportSchedIssue(strict, m.Name, f.Name, msg) }

	// 1. The driving field must be *time.Time so "unset" is distinguishable.
	if f.Type != timePtrType {
		report("mfx:\"scheduled\" requires a *time.Time field")
		return ScheduledSpec{}, false
	}
	// 2. Unrecognised option.
	if t.SchedBadOpt != "" {
		report(fmt.Sprintf("unrecognised scheduled option %q", t.SchedBadOpt))
		return ScheduledSpec{}, false
	}
	// 3. Action count — soft-delete / hard-delete / field= are exclusive.
	n := 0
	if t.SchedSoft {
		n++
	}
	if t.SchedHard {
		n++
	}
	if t.SchedField != "" {
		n++
	}
	if n == 0 {
		report("mfx:\"scheduled\" declares no action; expected one of soft-delete, hard-delete, field=...")
		return ScheduledSpec{}, false
	}
	if n > 1 {
		report("mfx:\"scheduled\" declares conflicting actions; exactly one of soft-delete, hard-delete, field= is allowed")
		return ScheduledSpec{}, false
	}
	// 5. from=/to= without field=.
	if t.SchedField == "" && (t.SchedHasFrom || t.SchedHasTo) {
		report("scheduled from=/to= options require field=")
		return ScheduledSpec{}, false
	}

	switch {
	case t.SchedSoft:
		// 6. Model must be soft-deletable.
		if !m.SoftDelete.Enabled {
			report("scheduled;soft-delete requires a soft-deletable model (embed maniflex.WithDeletedAt or use ;hard-delete)")
			return ScheduledSpec{}, false
		}
		return ScheduledSpec{Column: t.DBName, Action: SchedSoftDelete}, true

	case t.SchedHard:
		return ScheduledSpec{Column: t.DBName, Action: SchedHardDelete}, true

	default: // set-field
		// 4. field= requires to=.
		if !t.SchedHasTo {
			report(fmt.Sprintf("scheduled;field=%s requires to=", t.SchedField))
			return ScheduledSpec{}, false
		}
		// 7. Target column must exist as a scalar field on the model.
		target := m.FieldByDBName(t.SchedField)
		if target == nil {
			report(fmt.Sprintf("scheduled;field=%s names a column that does not exist on the model", t.SchedField))
			return ScheduledSpec{}, false
		}
		// 8. enum membership — catches typos at boot.
		if len(target.Tags.Enum) > 0 {
			if !schedInEnum(target.Tags.Enum, t.SchedTo) {
				report(fmt.Sprintf("scheduled to=%q is not a member of %s's enum", t.SchedTo, t.SchedField))
				return ScheduledSpec{}, false
			}
			if t.SchedHasFrom && !schedInEnum(target.Tags.Enum, t.SchedFrom) {
				report(fmt.Sprintf("scheduled from=%q is not a member of %s's enum", t.SchedFrom, t.SchedField))
				return ScheduledSpec{}, false
			}
		}
		return ScheduledSpec{
			Column:  t.DBName,
			Action:  SchedSetField,
			Field:   t.SchedField,
			From:    t.SchedFrom,
			HasFrom: t.SchedHasFrom,
			To:      t.SchedTo,
		}, true
	}
}

func schedInEnum(enum []string, v string) bool {
	for _, e := range enum {
		if e == v {
			return true
		}
	}
	return false
}

// ─── Two-pass field scanner ───────────────────────────────────────────────────

// rawField is a collected struct field before categorisation.
type rawField struct {
	sf    reflect.StructField
	index []int
	tags  FieldTags
}

// scanFields performs a two-pass walk of struct t:
//
//  1. Collect every exported (non-ignored) field into one of four buckets:
//     explicitFKs, conventionFKs, companions, hasManySlices, scalars.
//  2. Build RelationMeta and FieldMeta entries from the collected buckets.
func scanFields(t reflect.Type, meta *ModelMeta, indexPath []int) error {
	var (
		explicitFKs   []rawField          // fields with mfx:"relation:X" tag
		conventionFKs []rawField          // fields ending in "ID" (no explicit relation tag)
		companions    map[string]rawField // Go field name → non-builtin struct field
		hasManySlices []rawField          // slice-of-struct fields
		m2mSlices     []rawField          // slice fields with mfx:"through:X"
		scalars       []rawField          // everything else
	)
	companions = make(map[string]rawField)

	// ── Pass 1: categorise ───────────────────────────────────────────────────
	if err := collectFields(t, indexPath, meta, &explicitFKs, &conventionFKs,
		companions, &hasManySlices, &m2mSlices, &scalars); err != nil {
		return err
	}

	// ── Pass 2: build relations ──────────────────────────────────────────────

	// Explicit FKs (relation:X tag) — companion field is mandatory
	companionsClaimed := make(map[string]bool)
	for _, fk := range explicitFKs {
		relName := fk.tags.Relation // e.g. "Manager"
		companion, ok := companions[relName]
		if !ok {
			return fmt.Errorf(
				"maniflex: model %q field %q has mfx:\"relation:%s\" but no companion field %q of the target struct type",
				meta.Name, fk.sf.Name, relName, relName,
			)
		}
		relatedModel := companion.sf.Type.Name()
		if companion.sf.Type.Kind() == reflect.Ptr {
			relatedModel = companion.sf.Type.Elem().Name()
		}
		if relatedModel == "" {
			return fmt.Errorf(
				"maniflex: companion field %q on model %q must be a named struct type", relName, meta.Name)
		}

		relKey := toSnakeCase(relName)
		meta.Relations = append(meta.Relations, RelationMeta{
			FieldName:      fk.sf.Name,
			DBName:         fk.tags.DBName,
			FKColumn:       fk.tags.DBName,
			RelationKey:    relKey,
			CompanionField: relName,
			RelatedModel:   relatedModel,
			Kind:           BelongsTo,
			OnDelete:       fk.tags.OnDelete,
		})
		// The FK column is a real DB column — add it as a scalar field too
		meta.Fields = append(meta.Fields, FieldMeta{
			Name: fk.sf.Name, Type: fk.sf.Type, Tags: fk.tags, Index: fk.index,
		})
		companionsClaimed[relName] = true
	}

	// Convention FKs (UserID → User) — companion is optional
	for _, fk := range conventionFKs {
		// e.g. "AuthorID" → relModelName = "Author", relKey = "author"
		relModelName := strings.TrimSuffix(fk.sf.Name, "ID")
		relKey := toSnakeCase(relModelName)

		// If a companion exists use its concrete type name (e.g. field "Author Employee"
		// means the FK actually references Employee, not Author).
		relatedModel := relModelName
		if comp, ok := companions[relModelName]; ok {
			if comp.sf.Type.Kind() == reflect.Ptr {
				relatedModel = comp.sf.Type.Elem().Name()
			} else {
				relatedModel = comp.sf.Type.Name()
			}
			companionsClaimed[relModelName] = true
		}

		meta.Relations = append(meta.Relations, RelationMeta{
			FieldName:      fk.sf.Name,
			DBName:         fk.tags.DBName,
			FKColumn:       fk.tags.DBName,
			RelationKey:    relKey,
			CompanionField: relModelName,
			RelatedModel:   relatedModel,
			Convention:     true,
			Kind:           BelongsTo,
			OnDelete:       fk.tags.OnDelete,
		})
		meta.Fields = append(meta.Fields, FieldMeta{
			Name: fk.sf.Name, Type: fk.sf.Type, Tags: fk.tags, Index: fk.index,
		})
		companionsClaimed[relModelName] = true
	}

	// HasMany slices
	for _, hm := range hasManySlices {
		ft := hm.sf.Type
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		fkCol := toSnakeCase(meta.Name) + "_id"
		relKey := hm.tags.DBName
		meta.Relations = append(meta.Relations, RelationMeta{
			FieldName:    hm.sf.Name,
			DBName:       hm.tags.DBName,
			FKColumn:     fkCol,
			RelationKey:  relKey,
			RelatedModel: ft.Name(),
			Kind:         HasMany,
		})
		// HasMany slices are NOT DB columns on this table — do not add to Fields
	}

	// ManyToMany stubs — through: tag provides the junction model name.
	// ThroughTable/ThroughLocalFK/ThroughRemoteFK are filled in by resolveManyToMany
	// after all models are registered.
	for _, m2m := range m2mSlices {
		ft := m2m.sf.Type
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		meta.Relations = append(meta.Relations, RelationMeta{
			FieldName:    m2m.sf.Name,
			DBName:       m2m.tags.DBName,
			RelationKey:  m2m.tags.DBName,
			RelatedModel: ft.Name(),
			ThroughModel: m2m.tags.Through,
			Kind:         ManyToMany,
		})
	}

	// Companions that were NOT claimed by any FK are unexpected; warn but skip
	// (they might be legitimate embedded structs the user wants ignored)
	// — no error, just silently skip unclaimed companions

	// Scalar fields
	for _, s := range scalars {
		meta.Fields = append(meta.Fields, FieldMeta{
			Name: s.sf.Name, Type: s.sf.Type, Tags: s.tags, Index: s.index,
		})
	}

	return nil
}

// collectFields recursively walks struct type t, expanding anonymous embeds,
// and drops each field into the appropriate bucket.
func collectFields(
	t reflect.Type,
	indexPath []int,
	meta *ModelMeta,
	explicitFKs *[]rawField,
	conventionFKs *[]rawField,
	companions map[string]rawField,
	hasManySlices *[]rawField,
	m2mSlices *[]rawField,
	scalars *[]rawField,
) error {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)

		// Skip unexported fields (PkgPath is non-empty only for them), e.g.
		// BaseModel's framework-internal carriers. They are never DB columns and
		// cannot be set via reflection anyway. Exported embeds keep PkgPath=="".
		if sf.PkgPath != "" && !sf.Anonymous {
			continue
		}

		idx := append(append([]int{}, indexPath...), i)

		// Expand anonymous (embedded) structs recursively
		if sf.Anonymous {
			ft := sf.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if err := collectFields(ft, idx, meta,
					explicitFKs, conventionFKs, companions, hasManySlices, m2mSlices, scalars); err != nil {
					return err
				}
			}
			continue
		}

		tags := parseFieldTags(sf)
		if tags.Ignore {
			continue
		}
		if tags.LocaleModeConflict {
			return fmt.Errorf(
				"maniflex: model %q field %q has conflicting locale mode tags — only one of split, resolve, dynamic is allowed",
				meta.Name, sf.Name)
		}
		// Checked here rather than over meta.Fields so that every field is
		// covered — HasMany and many-to-many slices become Relations and never
		// reach meta.Fields — while the anonymous embed, which the branch above
		// recursed into and skipped, is not. That matters: BaseModel carries
		// mfx:"versioned"/"cursor_field:", which ScanModel reads directly and
		// this parser has no case for.
		if len(tags.UnknownOpts) > 0 {
			return unknownOptError(meta.Name, sf.Name, tags.UnknownOpts)
		}

		// Determine the element type (unwrap ptr/slice)
		ft := sf.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		isSlice := ft.Kind() == reflect.Slice
		elemType := ft
		if isSlice {
			elemType = ft.Elem()
			if elemType.Kind() == reflect.Ptr {
				elemType = elemType.Elem()
			}
		}

		rf := rawField{sf: sf, index: idx, tags: tags}

		switch {
		// ── Explicit relation tag: mfx:"relation:X" ───────────────────────────
		case tags.Relation != "":
			*explicitFKs = append(*explicitFKs, rf)

		// ── ManyToMany: []SomeStruct with mfx:"through:JunctionModel" ─────────
		case isSlice && elemType.Kind() == reflect.Struct && !isBuiltinStruct(elemType) && tags.Through != "":
			*m2mSlices = append(*m2mSlices, rf)

		// ── HasMany: []SomeStruct (non-builtin, no relation tag) ──────────────
		case isSlice && elemType.Kind() == reflect.Struct && !isBuiltinStruct(elemType):
			*hasManySlices = append(*hasManySlices, rf)

		// ── Companion placeholder: SomeStruct field (non-builtin, not a slice) ─
		// These are never DB columns; they carry the type of a related model.
		case !isSlice && ft.Kind() == reflect.Struct && !isBuiltinStruct(ft):
			companions[sf.Name] = rf

		// ── Inferred relation: bare mfx:"relation" on a scalar FK. The target
		// model is the field name with a trailing "ID" stripped (AuthorID →
		// Author). Relations are NO LONGER inferred from the "ID" suffix alone —
		// a field must opt in with mfx:"relation" (or mfx:"relation:Target").
		case tags.RelationInfer && !isSlice:
			*conventionFKs = append(*conventionFKs, rf)

		// ── Everything else is a scalar DB column ─────────────────────────────
		default:
			*scalars = append(*scalars, rf)
		}
	}
	return nil
}

var sqlTyperIfaceModel = reflect.TypeOf((*SQLTyper)(nil)).Elem()

// isBuiltinStruct returns true for struct types that should be treated as
// scalar DB columns rather than relation companions. This covers time.Time,
// anonymous structs, and any type implementing SQLTyper (e.g. money.Amount).
func isBuiltinStruct(t reflect.Type) bool {
	if t == reflect.TypeOf(time.Time{}) || t.PkgPath() == "" {
		return true
	}
	return t.Implements(sqlTyperIfaceModel) || reflect.PointerTo(t).Implements(sqlTyperIfaceModel)
}
