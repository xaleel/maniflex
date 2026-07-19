package testutil

import "github.com/xaleel/maniflex"

// ── Test models ───────────────────────────────────────────────────────────────
// These are the canonical fixtures used across the whole e2e suite.
// They deliberately exercise every mfx tag directive so a single suite covers
// the full tag surface.

// User is a platform user. Exercises: required, filterable, sortable,
// unique, immutable, writeonly, enum.
type User struct {
	maniflex.BaseModel
	Name     string  `json:"name"     db:"name"     mfx:"required,filterable,sortable"`
	Email    string  `json:"email"    db:"email"    mfx:"required,filterable,unique,immutable"`
	Password string  `json:"password" db:"password" mfx:"required,writeonly"`
	Role     string  `json:"role"     db:"role"     mfx:"filterable,sortable,enum:admin|editor|viewer,default:viewer"`
	Score    int     `json:"score"    db:"score"    mfx:"filterable,sortable,default:0"`
	Posts    []Post  `json:"posts,omitempty"`
	Owner    *string `json:"owner,omitempty"        mfx:"default:"`
}

// Post is a blog post. Exercises: soft-delete (deleted_at), required,
// filterable, sortable, readonly, enum, FK (BelongsTo User), HasMany Comments.
type Post struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title    string    `json:"title"    db:"title"   mfx:"required,filterable,sortable"`
	Body     string    `json:"body"     db:"body"    mfx:"required"`
	Status   string    `json:"status"   db:"status"  mfx:"required,filterable,sortable,enum:draft|published|archived"`
	Views    int       `json:"views"    db:"views"   mfx:"readonly,filterable,sortable,default:0"`
	UserID   string    `json:"user_id"  db:"user_id" mfx:"required,filterable,relation"`
	Comments []Comment `json:"comments,omitempty"`
}

// Comment is a reply. Exercises: required, filterable, immutable (post_id),
// FK to Post and User. Score exercises min/max (set in validate middleware tests).
type Comment struct {
	maniflex.BaseModel
	Body     string `json:"body"     db:"body"     mfx:"required"`
	PostID   string `json:"post_id"  db:"post_id"  mfx:"required,filterable,immutable,relation"`
	UserID   string `json:"user_id"  db:"user_id"  mfx:"required,filterable,relation"`
	Approved bool   `json:"approved" db:"approved" mfx:"filterable,sortable,default:false"`
}

// Order and OrderPayment exercise maniflex.Rollup. Order carries denormalised
// columns (PaidAmount, PaymentCount) maintained from its OrderPayment children.
type Order struct {
	maniflex.BaseModel
	Reference    string `json:"reference"     db:"reference"     mfx:"required,filterable"`
	PaidAmount   int    `json:"paid_amount"   db:"paid_amount"   mfx:"filterable,sortable,default:0"`
	PaymentCount int    `json:"payment_count" db:"payment_count" mfx:"filterable,default:0"`
	TopPayment   *int   `json:"top_payment"   db:"top_payment"`
}

// OrderPayment is a payment against an Order. Soft-deletable, so a rollup must
// exclude soft-deleted payments.
type OrderPayment struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	OrderID string `json:"order_id" db:"order_id" mfx:"required,filterable,relation"`
	Amount  int    `json:"amount"   db:"amount"   mfx:"required,filterable"`
}

// Tag exercises soft-delete via WithIsDeleted (bool style).
type Tag struct {
	maniflex.BaseModel
	maniflex.WithIsDeleted
	Name  string `json:"name"  db:"name"  mfx:"required,filterable,sortable,unique"`
	Color string `json:"color" db:"color" mfx:"filterable"`
}

// Document exercises file upload features: required file with constraints,
// optional file with auto_delete:false.
type Document struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required"`
	File  string `json:"file"  db:"file"  mfx:"file,required,max_size:1MB,accept:application/pdf|text/plain"`
	Icon  string `json:"icon"  db:"icon"  mfx:"file,max_size:500KB,accept:image/*,auto_delete:false"`
}

// ACLDoc exercises the mfx:"file_acl" directive. Each field selects a
// different ACL mode so a single fixture can probe all three response shapes
// from rewriteFileACL.
type ACLDoc struct {
	maniflex.BaseModel
	Title       string `json:"title"        db:"title"        mfx:"required"`
	RawKey      string `json:"raw_key"      db:"raw_key"      mfx:"file"`                 // default = private (raw key)
	SignedFile  string `json:"signed_file"  db:"signed_file"  mfx:"file,file_acl:signed"` // -> URL via FileStorage.URL(ttl)
	PublicFile  string `json:"public_file"  db:"public_file"  mfx:"file,file_acl:public"` // -> URL via FileStorage.URL(0)
}

// Gallery exercises maniflex.FileKeys — a file field holding many storage keys.
// Images is auto_delete (the default) so an update's dropped keys are GC'd;
// Attachments opts out, and caps its count, so both halves are probed.
type Gallery struct {
	maniflex.BaseModel
	Title       string            `json:"title"       db:"title"       mfx:"required"`
	Images      maniflex.FileKeys `json:"images"      db:"images"      mfx:"file,accept:image/*,max_size:1MB,file_acl:signed"`
	Attachments maniflex.FileKeys `json:"attachments" db:"attachments" mfx:"file,max_count:2,auto_delete:false"`
}

// SoftDoc exercises file fields on a soft-delete model (files should persist).
type SoftDoc struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Name   string `json:"name"   db:"name"   mfx:"required"`
	Attach string `json:"attach" db:"attach" mfx:"file"`
}

// LockedInvoice exercises mfx:"lock_when". `posted` and `void` are terminal
// states: once Status holds either value, updates and deletes must 422.
type LockedInvoice struct {
	maniflex.BaseModel
	Number string `json:"number" db:"number" mfx:"required,filterable"`
	Status string `json:"status" db:"status" mfx:"required,enum:draft|posted|void,lock_when:status=posted,lock_when:status=void"`
	Amount int    `json:"amount" db:"amount"`
}

// WorkflowDoc exercises middleware/workflow. No lock_when — we want
// transitions to be possible so the workflow middleware is what gates them.
type WorkflowDoc struct {
	maniflex.BaseModel
	Title  string `json:"title"  db:"title"  mfx:"required"`
	Status string `json:"status" db:"status" mfx:"required"`
}

// ExportableRow exercises the auto-generated CSV/XLSX export. Hidden and
// writeonly fields are present to verify they are excluded from the dump.
type ExportableRow struct {
	maniflex.BaseModel
	Name   string `json:"name"   db:"name"   mfx:"required,filterable,sortable"`
	Email  string `json:"email"  db:"email"`
	Secret string `json:"secret" db:"secret" mfx:"writeonly"` // never in output
	Notes  string `json:"notes"  db:"notes"  mfx:"hidden"`    // never in output
}

// StockBalance holds the available quantity for a pharmacy stock item.
// Used by Dispense to exercise mfx:"lock_scope".
type StockBalance struct {
	maniflex.BaseModel
	Name     string `json:"name"     db:"name"     mfx:"required,filterable"`
	Quantity int    `json:"quantity" db:"quantity" mfx:"required,filterable"`
}

// Dispense records a dispensing event against a StockBalance row.
// The lock_scope:StockBalance tag causes the DB step to acquire a FOR UPDATE
// lock on the referenced StockBalance row before the insert, preventing
// write-skew races on concurrent dispensing.
// Requires maniflex.WithTransaction(nil) on the Service step.
type Dispense struct {
	maniflex.BaseModel
	StockID  string `json:"stock_id"  db:"stock_id"  mfx:"required,lock_scope:StockBalance"`
	Quantity int    `json:"quantity"  db:"quantity"  mfx:"required"`
}

// DefaultModels returns all test model fixtures.
//
// Comment needs no junction opt-out: it has two BelongsTo relations (Post, User)
// but also carries body and approved, so it is not the two-keys-and-nothing-else
// shape auto-detection accepts. Under the old rule it silently registered a
// Post↔User many-to-many nothing asked for — audit MS-L9, in the framework's own
// fixtures.
func DefaultModels() []any {
	return []any{User{}, Post{}, Comment{}, Tag{}}
}

// FileModels returns models used in file upload tests.
func FileModels() []any {
	return []any{Document{}, SoftDoc{}}
}
