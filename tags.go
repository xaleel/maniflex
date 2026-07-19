package maniflex

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// OnDeleteAction is a referential action applied to FK columns when the
// referenced row is deleted.
type OnDeleteAction string

const (
	OnDeleteNoAction OnDeleteAction = ""         // default — no constraint clause emitted
	OnDeleteCascade  OnDeleteAction = "cascade"  // DELETE parent → DELETE children
	OnDeleteSetNull  OnDeleteAction = "setNull"  // DELETE parent → SET fk = NULL
	OnDeleteRestrict OnDeleteAction = "restrict" // DELETE parent → ERROR if children exist
)

// uploadPresigned and uploadStream are the values mfx:"upload:" takes. It is a
// value-carrying option rather than a bare "presigned" so the tag says what it
// configures, and so each upload strategy has its own spelling.
//
//   - presigned: mint a one-shot URL; bytes go client→storage, never touching
//     the app process.
//   - stream: multipart through the app, but piped straight to storage as it
//     arrives rather than buffered to disk first.
const (
	uploadPresigned = "presigned"
	uploadStream    = "stream"
)

// FieldTags holds every directive that can appear in a `mfx:"..."` struct tag,
// plus the derived JSON and DB column names.
type FieldTags struct {
	// mfx tag directives
	Required   bool     // must be present in create requests
	Readonly   bool     // stripped from all write operations
	Immutable  bool     // settable on create, rejected on update
	Filterable bool     // may be used in ?filter= queries
	Sortable   bool     // may be used in ?sort= queries
	Hidden     bool     // excluded from all API responses; implies Readonly unless WriteOnly
	WriteOnly  bool     // accepted on write, excluded from responses (e.g. password)
	Unique     bool     // hint to DB adapter to add UNIQUE constraint
	Index      bool     // mfx:"index" → CREATE INDEX on the column in AutoMigrate
	Searchable bool     // included in full-text search (if supported)
	Enum       []string // allowed values, e.g. mfx:"enum:draft|published"
	Min        *float64 // numeric minimum
	Max        *float64 // numeric maximum
	Default    string   // automatically cast to corresponding type (if possible)

	// Relation options — set via mfx:"relation:RelationName;onDelete:cascade"
	// Relation names the companion struct field that carries the target model type.
	// When set this FK is an explicit relation; when empty the legacy ID-suffix
	// convention is used instead.
	Relation string
	OnDelete OnDeleteAction

	// RelationInfer is set by a bare mfx:"relation" tag. It marks a scalar FK field
	// as a BelongsTo whose target is inferred from the field name (the "ID" suffix
	// is stripped, e.g. AuthorID → Author). Use mfx:"relation:Target" instead when
	// the field name doesn't match the target model.
	RelationInfer bool

	// Deprecated: NoRelation (mfx:"norelation") is a no-op. Relations are no longer
	// inferred from a field's "ID" suffix — tag the field mfx:"relation" (or
	// mfx:"relation:Target") to opt IN instead. The tag is still parsed so existing
	// models compile.
	NoRelation bool

	// Through is set on []SliceFields to declare a many-to-many relation via a
	// junction model. Value is the junction model struct name, e.g. "ProductTag".
	// mfx:"through:ProductTag"
	Through string

	// Encryption options — set via mfx:"encrypted" or mfx:"encrypted,key:patient-pii"
	// Encrypted marks the field for transparent AES-256-GCM encryption at rest.
	// Values are stored as "enc:<base64(envelope)>" and decrypted on read.
	// Filtering and sorting on encrypted fields is not supported.
	// If Unique is also set, a companion {field}_hmac column enforces uniqueness
	// without exposing the plaintext.
	Encrypted     bool
	EncryptionKey string // key name passed to KeyProvider; defaults to "default"

	// File upload options — set via mfx:"file,max_size:10MB,accept:image/*"
	// File marks this field as a file upload field. The DB column stores the
	// storage key (string). When true, multipart form-data is automatically
	// accepted for create/update operations on models containing this field.
	File bool
	// MaxSize is the maximum allowed file size in bytes. Parsed from
	// mfx:"max_size:10MB". Zero means no per-field limit. On a FileKeys field
	// it bounds each key's object, not their total.
	MaxSize int64
	// MaxCount bounds how many keys a maniflex.FileKeys field accepts. Parsed
	// from mfx:"max_count:20". Zero means DefaultMaxFileCount — every key is
	// Stat'd against storage, so an uncapped array is one request costing N
	// round-trips. Meaningless on a single-key (string) file field, which is a
	// registration error rather than a silently ignored option.
	MaxCount int
	// Accept is a list of allowed MIME type patterns, e.g. ["image/*", "application/pdf"].
	// Parsed from mfx:"accept:image/*|application/pdf".
	Accept []string
	// AutoDelete controls whether the file is automatically deleted from storage
	// when the record is hard-deleted or the field value is replaced by an update.
	// Default: true when File is set. Set to false with mfx:"auto_delete:false".
	AutoDelete bool
	// PresignedUpload mounts POST /{model}/{field}/upload-url for this field, so a
	// client uploads its bytes straight to storage and then sends back only the
	// key. Parsed from mfx:"upload:presigned". Requires File.
	//
	// The default (multipart through the app) materialises the whole body in the
	// server process before the handler runs, so a 60 MB video costs 60 MB of
	// server memory and two hops. This is the way out of both, and the field's
	// max_size and accept rules still bind: they are pinned into the signature
	// where the backend can, and re-checked against the stored object when the
	// record references the key.
	PresignedUpload bool
	// StreamUpload pipes a multipart upload straight to storage as it arrives off
	// the socket, instead of buffering the whole body to the app server's disk
	// first. Parsed from mfx:"upload:stream". Requires File, and is mutually
	// exclusive with PresignedUpload (a field has one upload strategy).
	//
	// The default multipart path materialises the whole request before the
	// handler runs — a 5 GB upload lands on the app server's disk in full, then is
	// copied to storage. Streaming removes that landing: bytes are written to the
	// backend as they are read. The trade-off is that the object is stored before
	// the record's own validation runs (accept is still checked from the sniffed
	// head first, and max_size mid-stream), so a request that later fails leaves
	// an object that the same non-2xx cleanup as every other stored file removes.
	// For the very largest uploads prefer PresignedUpload, which never routes the
	// bytes through the app at all.
	StreamUpload bool
	// FileACL controls how the field value is presented in API responses.
	// Parsed from mfx:"file_acl:private|signed|public". Default: FileACLPrivate.
	// FileACLSigned replaces the storage key with a pre-signed URL.
	// FileACLPublic replaces the storage key with a permanent public URL.
	FileACL FileACLMode

	// Locale marks this field as a bilingual storage field. The Go type must
	// be maniflex.LocaleString. Stored as TEXT (SQLite) or JSONB (Postgres).
	// Response representation is controlled by LocaleMode (default: split).
	Locale bool

	// LocaleMode overrides the response representation for this specific field.
	// When empty the mode is inherited from ModelConfig.DefaultLocaleMode, then
	// LocaleOptions.DefaultLocaleMode, then split (framework default).
	// Valid values: "split", "resolve", "dynamic".
	LocaleMode LocaleMode

	// LocaleModeConflict is set when more than one of split/resolve/dynamic
	// appears in the same mfx tag. ScanModel rejects such fields at registration.
	LocaleModeConflict bool

	// LocaleDefault is the per-field fallback locale used when the client did
	// not request a specific locale. Only meaningful in resolve/split mode.
	// e.g. mfx:"locale,default_locale:ar" makes Arabic the field-level default.
	LocaleDefault string

	// LockWhen carries conditions parsed from mfx:"lock_when:field=value"
	// directives. Multiple occurrences on the same field accumulate. At
	// registration these per-field lists are flattened into ModelMeta.LockWhen
	// — the conditions reference the record state, not the field they're
	// declared on, so the declaration site is incidental.
	LockWhen []LockCondition

	// LockScope names a registered model whose row must be locked FOR UPDATE
	// before a create. Parsed from mfx:"lock_scope:ModelName".
	// The field value is used as the referenced row ID.
	// Requires an active transaction (maniflex.WithTransaction).
	LockScope string

	// CursorField opts the model into keyset (cursor) pagination and names the
	// column the cursor walks. Parsed from mfx:"cursor_field:created_at". The
	// value is the JSON or DB name of the cursor field; ScanModel resolves it to
	// a DB column and stores it on ModelMeta.CursorField. Declaring it on any one
	// field (typically the embedded BaseModel) enables the model — the value, not
	// the declaration site, picks the cursor column.
	CursorField string

	// Scheduled-operation directive (8.6). Scheduled is the on/off switch; the
	// rest is meaningful only when Scheduled is true. The action flags are NOT
	// mutually validated here — parseScheduledTag records exactly what was
	// written; ScanModel resolves and rejects (see "No implicit action").
	Scheduled    bool   // mfx:"scheduled" present
	SchedSoft    bool   // ;soft-delete
	SchedHard    bool   // ;hard-delete
	SchedField   string // ;field=
	SchedFrom    string // ;from=
	SchedTo      string // ;to=
	SchedHasFrom bool   // distinguishes ;from= absent vs. from="" given
	SchedHasTo   bool   // distinguishes ;to=   absent vs. to=""   given
	SchedBadOpt  string // first unrecognised option, "" if none — ScanModel errors on it

	// UnknownOpts holds every mfx: comma-part the parser did not recognise, in
	// declaration order. ScanModel rejects a field that has any: an unknown
	// option used to be discarded in silence, so a directive typo'd as
	// `read_only` left Readonly false and the field client-writable — the tag
	// failing open in exactly the case it exists to protect.
	UnknownOpts []string

	// MalformedOpts holds mfx: parts whose *key* is recognised but whose value
	// could not be used — mfx:"min:abc", mfx:"max:", mfx:"enum:a||b". These used
	// to be dropped silently, which reads at a glance as a constraint that is
	// enforced and is not. ScanModel turns a non-empty list into a registration
	// error (audit MS-L11).
	MalformedOpts []string

	// Derived names
	JSONName  string
	DBName    string
	OmitEmpty bool
	Ignore    bool // db:"-" or mfx:"-" (excludes the field from persistence — no column)
}

// parseFieldTags derives FieldTags from a struct field's reflect.StructTag.
func parseFieldTags(field reflect.StructField) FieldTags {
	t := FieldTags{}

	// ---- json tag ----
	if jt := field.Tag.Get("json"); jt != "" {
		parts := strings.SplitN(jt, ",", 2)
		switch {
		case parts[0] == "-":
			// json:"-" hides the field from API responses (Hidden) and marks it
			// server-owned (Readonly), but keeps it as a real column. To exclude a
			// field from persistence entirely use db:"-" or mfx:"-".
			t.Hidden = true
			t.Readonly = true
		case parts[0] != "":
			t.JSONName = parts[0]
		}
		if len(parts) > 1 && strings.Contains(parts[1], "omitempty") {
			t.OmitEmpty = true
		}
	}
	if t.JSONName == "" {
		t.JSONName = toSnakeCase(field.Name)
	}

	// ---- db tag ----
	if dt := field.Tag.Get("db"); dt != "" {
		if dt == "-" {
			t.Ignore = true
			return t
		}
		t.DBName = dt
	}
	if t.DBName == "" {
		t.DBName = t.JSONName
	}

	// ---- mfx tag ----
	ct := field.Tag.Get("mfx")
	if ct == "-" {
		t.Ignore = true
		return t
	}
	for _, part := range strings.Split(ct, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "required":
			t.Required = true
		case part == "readonly":
			t.Readonly = true
		case part == "immutable":
			t.Immutable = true
		case part == "filterable":
			t.Filterable = true
		case part == "sortable":
			t.Sortable = true
		case part == "hidden":
			t.Hidden = true
		case part == "writeonly":
			t.WriteOnly = true
		case part == "unique":
			t.Unique = true
		case part == "index":
			t.Index = true
		case part == "relation":
			t.RelationInfer = true
		case part == "norelation":
			t.NoRelation = true
		case part == "searchable":
			t.Searchable = true
		case strings.HasPrefix(part, "enum:"):
			members := strings.Split(strings.TrimPrefix(part, "enum:"), "|")
			// enum: yields [""] and enum:a||b a valid-looking empty member, so
			// an empty option becomes a silently permitted value (audit MS-L11).
			if slices.Contains(members, "") {
				t.MalformedOpts = append(t.MalformedOpts, part)
				break
			}
			t.Enum = members
		case strings.HasPrefix(part, "min:"):
			// A value ParseFloat rejects used to be dropped on the floor, so
			// mfx:"min:abc" read as a constraint and enforced nothing.
			if v, err := strconv.ParseFloat(strings.TrimPrefix(part, "min:"), 64); err == nil {
				t.Min = &v
			} else {
				t.MalformedOpts = append(t.MalformedOpts, part)
			}
		case strings.HasPrefix(part, "max:"):
			if v, err := strconv.ParseFloat(strings.TrimPrefix(part, "max:"), 64); err == nil {
				t.Max = &v
			} else {
				t.MalformedOpts = append(t.MalformedOpts, part)
			}
		case strings.HasPrefix(part, "default:"):
			t.Default = strings.TrimPrefix(part, "default:")

		case part == "encrypted":
			t.Encrypted = true
		case strings.HasPrefix(part, "key:"):
			t.EncryptionKey = strings.TrimPrefix(part, "key:")

		case part == "file":
			t.File = true
			t.AutoDelete = true
		case strings.HasPrefix(part, "max_size:"):
			t.MaxSize = parseByteSize(strings.TrimPrefix(part, "max_size:"))
		case strings.HasPrefix(part, "max_count:"):
			// -1 marks a malformed value for ScanModel to reject by name. It
			// cannot go to UnknownOpts (the key IS known, and knownPrefixOpts
			// must accept any value), and it must not be swallowed the way min:
			// and max: swallow theirs: max_count is protective, so a typo'd
			// mfx:"max_count:1O" would silently widen the cap from 10 to the
			// default 100 rather than tighten it.
			t.MaxCount = -1
			if v, err := strconv.Atoi(strings.TrimPrefix(part, "max_count:")); err == nil && v > 0 {
				t.MaxCount = v
			}
		case strings.HasPrefix(part, "accept:"):
			t.Accept = strings.Split(strings.TrimPrefix(part, "accept:"), "|")
		case part == "auto_delete:false":
			t.AutoDelete = false
		case strings.HasPrefix(part, "lock_when:"):
			if cond, ok := parseLockWhen(part); ok {
				t.LockWhen = append(t.LockWhen, cond)
			}
		case strings.HasPrefix(part, "lock_scope:"):
			t.LockScope = strings.TrimPrefix(part, "lock_scope:")
		case strings.HasPrefix(part, "cursor_field:"):
			t.CursorField = strings.TrimPrefix(part, "cursor_field:")
		case strings.HasPrefix(part, "file_acl:"):
			switch FileACLMode(strings.TrimPrefix(part, "file_acl:")) {
			case FileACLSigned:
				t.FileACL = FileACLSigned
			case FileACLPublic:
				t.FileACL = FileACLPublic
			default:
				t.FileACL = FileACLPrivate
			}
		case strings.HasPrefix(part, "upload:"):
			switch strings.TrimPrefix(part, "upload:") {
			case uploadPresigned:
				t.PresignedUpload = true
			case uploadStream:
				t.StreamUpload = true
			default:
				t.UnknownOpts = append(t.UnknownOpts, part)
			}
		// relation:RelationName;onDelete:cascade
		// The semicolon-separated sub-options all live inside this single comma-part.
		case strings.HasPrefix(part, "relation:"):
			parseRelationTag(part, &t)
		case part == "locale":
			t.Locale = true
		case part == string(LocaleModeSplit), part == string(LocaleModeResolve), part == string(LocaleModeDynamic):
			if t.LocaleMode != "" {
				t.LocaleModeConflict = true
			}
			t.LocaleMode = LocaleMode(part)
		case strings.HasPrefix(part, "default_locale:"):
			t.LocaleDefault = strings.TrimPrefix(part, "default_locale:")
		case strings.HasPrefix(part, "through:"):
			t.Through = strings.TrimPrefix(part, "through:")
		// scheduled;<action>[;<opt>]... — all sub-options live in this comma-part.
		case part == "scheduled":
			t.Scheduled = true
		case strings.HasPrefix(part, "scheduled;"):
			parseScheduledTag(part, &t)

		// An option nobody claimed. Empty parts are not typos: a field with no
		// mfx tag at all splits to [""], and so does a trailing comma
		// (`mfx:"required,"`), so treating those as unknown would reject every
		// untagged field in existence.
		case part != "":
			t.UnknownOpts = append(t.UnknownOpts, part)
		}
	}

	// hidden means the client may neither read nor write the field — that is what
	// separates it from writeonly, which is the "client writes it, never reads it
	// back" case (a password). Only the read half was ever enforced, so a bare
	// hidden field was silently accepted from a request body: an `IsAdmin bool`
	// tagged hidden could be set by anyone via mass assignment, and because the
	// field is scrubbed from responses nothing showed that it had happened.
	//
	// Applied after the loop so tag order cannot matter, and skipped when
	// writeonly is present: that is an explicit statement that the client does
	// write the field, and an explicit directive outranks this implication.
	if t.Hidden && !t.WriteOnly {
		t.Readonly = true
	}
	return t
}

// parseRelationTag parses the "relation:Name;onDelete:action" directive.
// The entire directive is passed as one string (already split on commas).
//
//	"relation:Manager"
//	"relation:Manager;onDelete:cascade"
//	"relation:FrontendDev;onDelete:setNull"
func parseRelationTag(part string, t *FieldTags) {
	// Strip the "relation:" prefix, leaving "Name" or "Name;onDelete:action"
	rest := strings.TrimPrefix(part, "relation:")

	subParts := strings.Split(rest, ";")
	t.Relation = strings.TrimSpace(subParts[0])

	for _, sp := range subParts[1:] {
		sp = strings.TrimSpace(sp)
		kv := strings.SplitN(sp, ":", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(kv[0])) {
		case "ondelete":
			switch strings.ToLower(strings.TrimSpace(kv[1])) {
			case "cascade":
				t.OnDelete = OnDeleteCascade
			case "setnull", "set_null":
				t.OnDelete = OnDeleteSetNull
			case "restrict":
				t.OnDelete = OnDeleteRestrict
			}
		}
	}
}

// parseScheduledTag parses the "scheduled;<action>[;<opt>]..." directive.
// The entire directive is passed as one string (already split on commas).
//
//	"scheduled;soft-delete"
//	"scheduled;hard-delete"
//	"scheduled;field=status;from=draft;to=published"
//	"scheduled;field=color;to=red"
//
// The parser only records what was written; it never decides validity —
// ScanModel resolves and rejects (see "No implicit action").
func parseScheduledTag(part string, t *FieldTags) {
	t.Scheduled = true

	subParts := strings.Split(part, ";")
	// subParts[0] is the leading "scheduled" — skip it.
	for _, sp := range subParts[1:] {
		sp = strings.TrimSpace(sp)
		if sp == "" {
			continue
		}
		switch {
		case sp == "soft-delete":
			t.SchedSoft = true
		case sp == "hard-delete":
			t.SchedHard = true
		default:
			kv := strings.SplitN(sp, "=", 2)
			if len(kv) != 2 {
				if t.SchedBadOpt == "" {
					t.SchedBadOpt = sp
				}
				continue
			}
			key := strings.TrimSpace(kv[0])
			val := strings.TrimSpace(kv[1])
			switch key {
			case "field":
				t.SchedField = val
			case "from":
				t.SchedFrom = val
				t.SchedHasFrom = true
			case "to":
				t.SchedTo = val
				t.SchedHasTo = true
			default:
				if t.SchedBadOpt == "" {
					t.SchedBadOpt = sp
				}
			}
		}
	}
}

// parseByteSize parses a human-readable byte size string into bytes.
// Supported suffixes (case-insensitive): KB, MB, GB. Pure numeric strings
// are treated as bytes.
//
//	"10MB"  → 10 * 1024 * 1024
//	"500KB" → 500 * 1024
//	"1GB"   → 1 * 1024 * 1024 * 1024
//	"4096"  → 4096
func parseByteSize(s string) int64 {
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)

	type suffix struct {
		label      string
		multiplier int64
	}
	suffixes := []suffix{
		{"GB", 1 << 30},
		{"MB", 1 << 20},
		{"KB", 1 << 10},
	}

	for _, sf := range suffixes {
		if strings.HasSuffix(upper, sf.label) {
			numStr := strings.TrimSpace(s[:len(s)-len(sf.label)])
			v, err := strconv.ParseInt(numStr, 10, 64)
			if err != nil {
				return 0
			}
			return v * sf.multiplier
		}
	}

	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
