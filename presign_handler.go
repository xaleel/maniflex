package maniflex

// presign_handler.go — POST /{model}/{field}/upload-url (R5).
//
// An mfx:"file" field's bytes normally travel client → app → storage, and the
// app materialises all of them: ParseMultipartForm drains the body before the
// handler runs, and the in-memory buffer defaults to the same 32 MB as the body
// cap, so nothing spools to disk either. A 60 MB video therefore costs 60 MB of
// server memory and two hops of bandwidth to store one object. Adding
// upload:presigned to the field mints a one-shot authorisation instead, and the
// bytes go straight to the bucket.
//
// The route carries no record id, deliberately. The obvious shape —
// POST /{model}/{id}/{field}/upload-url — cannot serve a create-time file field
// at all, because the record does not exist yet; the requester's own example was
// a create-time Video field behind exactly that route. Model + field works for
// both, and the completion step is the ordinary write that names the key.
//
// The client never chooses the key. It is minted here through FilesConfig.KeyGen
// (so a per-tenant prefix scheme covers presigned uploads too) and returned. A
// client that could name the key could aim its upload at another record's
// object.

import (
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// presignUploadRequest is the body of POST /{model}/{field}/upload-url.
//
// The client declares what it is about to upload, and it is checked here rather
// than only afterwards: the point of pinning the limits into the signature is to
// find out before the bytes move, not after they are stored and billed.
type presignUploadRequest struct {
	// Filename is the client's original filename. It is sanitised into the key.
	Filename string `json:"filename"`

	// ContentType is the media type the client will send. It is checked against
	// the field's mfx:"accept" and then bound into the signature, so a client
	// cannot declare video/mp4 to obtain the URL and upload something else.
	ContentType string `json:"content_type"`

	// Size, when non-zero, is the byte count the client intends to send. It is
	// advisory — the signature is what enforces the cap — but checking it here
	// turns "upload 5 GB, then get a 413" into an immediate refusal.
	Size int64 `json:"size"`
}

// PresignUpload returns the handler for POST /{model}/{field}/upload-url. It is
// mounted only for fields tagged mfx:"file,upload:presigned", and only when
// FilesConfig.Storage is configured.
func (h *handlers) PresignUpload(meta *ModelMeta, field FieldMeta) http.HandlerFunc {
	fieldCopy := field
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cleanup := h.buildContext(w, r, meta, OpPresignUpload)
		defer cleanup()

		ctx.AttachmentField = &fieldCopy
		err := h.pipeline.executeTrimmed(ctx, func(ctx *ServerContext) error {
			return h.mintUploadURL(ctx, fieldCopy)
		})
		if err != nil {
			h.writePipelineError(w, ctx, err,
				slog.String("model", meta.Name), slog.String("field", fieldCopy.Tags.JSONName))
			return
		}
		writeResponse(w, ctx)
	}
}

// mintUploadURL is the handler body: validate what the client says it will send,
// mint a key, and ask the backend for an authorisation bounded by the field's own
// rules.
func (h *handlers) mintUploadURL(ctx *ServerContext, field FieldMeta) error {
	if h.steps.storage == nil {
		ctx.Abort(http.StatusNotImplemented, "NO_STORAGE",
			"no file storage is configured, so an upload cannot be presigned")
		return nil
	}

	var req presignUploadRequest
	if err := ctx.BindJSON(&req); err != nil {
		return nil // BindJSON has already aborted
	}
	req.Filename = strings.TrimSpace(req.Filename)
	req.ContentType = strings.TrimSpace(req.ContentType)

	if req.Filename == "" {
		ctx.Abort(http.StatusUnprocessableEntity, "VALIDATION_ERROR",
			"filename is required: it is what the storage key is derived from")
		return nil
	}
	if req.ContentType == "" {
		ctx.Abort(http.StatusUnprocessableEntity, "VALIDATION_ERROR",
			"content_type is required: it is bound into the signature, so the upload "+
				"must declare it up front rather than after the URL is issued")
		return nil
	}

	// The field's rules, checked before anything is minted. A URL issued for a
	// file the field would refuse is a URL whose only possible use is to store an
	// object no record can ever reference.
	if err := h.steps.checkFileRules(ctx, field, req.Size, req.ContentType); err != nil {
		return nil // checkFileRules has already aborted with 413 / 415
	}

	key := h.uploadKey(ctx, req)
	opts := PresignUploadOptions{
		TTL:         h.presignTTL(),
		MaxSize:     field.Tags.MaxSize,
		ContentType: req.ContentType,
		Filename:    req.Filename,
	}

	up, err := h.steps.storage.PresignUpload(ctx.Ctx, key, opts)
	if err != nil {
		if errors.Is(err, ErrPresignUnsupported) {
			ctx.Abort(http.StatusNotImplemented, "PRESIGN_UNSUPPORTED", fmt.Sprintf(
				"the configured file storage backend cannot presign uploads, so %q cannot "+
					"use upload:presigned — upload through the server instead, or switch to a "+
					"backend that can (storage/s3)", field.Tags.JSONName))
			return nil
		}
		ctx.Abort(http.StatusInternalServerError, "STORAGE_ERROR",
			fmt.Sprintf("failed to presign an upload for %q: %s", field.Tags.JSONName, err.Error()))
		return nil
	}

	ctx.Response = &APIResponse{StatusCode: http.StatusOK, Data: up}
	return nil
}

// uploadKey mints the storage key, through FilesConfig.KeyGen so that whatever
// prefixing an app applies to a POST /files upload applies here too — a per-tenant
// prefix that covered one path and not the other would be worse than none.
//
// KeyGen takes a *multipart.FileHeader because that is what the standalone upload
// has; this request has no multipart body, so one is synthesised from the two
// fields KeyGen can meaningfully read. It is a description of the upload either
// way.
func (h *handlers) uploadKey(ctx *ServerContext, req presignUploadRequest) string {
	gen := h.cfg.FilesConfig.KeyGen
	if gen == nil {
		gen = DefaultKeyGen
	}
	hdr := &multipart.FileHeader{
		Filename: req.Filename,
		Size:     req.Size,
		Header:   textproto.MIMEHeader{"Content-Type": []string{req.ContentType}},
	}
	return gen(ctx, hdr)
}

// presignTTL is how long a minted authorisation lasts. It reuses
// FilesConfig.SignedURLTTL — the same knob that bounds a signed download — since
// both answer "how long may this one handle to one object stay usable".
func (h *handlers) presignTTL() time.Duration {
	if ttl := h.cfg.FilesConfig.SignedURLTTL; ttl > 0 {
		return ttl
	}
	return DefaultFileSignedURLTTL
}
