package maniflex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
)

// errStreamTooLarge is returned by measuredReader when a streamed upload runs
// past its field's max_size. It is a sentinel so the storage call's failure can
// be told apart from a real backend error and answered 413 rather than 500.
var errStreamTooLarge = errors.New("stream exceeds max_size")

// measuredReader counts the bytes read through it and, when max > 0, returns
// errStreamTooLarge once more than max bytes have passed. A streamed upload's
// size is not known until it has been read, so max_size cannot be checked up
// front the way the buffered path checks it — this enforces it mid-flight
// instead, aborting the store rather than writing an over-limit object in full.
// max == 0 counts without limiting (the standalone /files path, whose ceiling is
// the request-body cap instead).
type measuredReader struct {
	r   io.Reader
	max int64
	n   int64
}

func (m *measuredReader) Read(p []byte) (int, error) {
	read, err := m.r.Read(p)
	m.n += int64(read)
	if m.max > 0 && m.n > m.max {
		return read, errStreamTooLarge
	}
	return read, err
}

// resolveContentTypeStreaming is the streaming twin of resolveContentType: it
// sniffs a non-seekable reader by buffering the first 512 bytes, then returns a
// reader that replays that prefix ahead of the rest — so the caller can hand the
// returned reader straight to FileStorage.Store without the sniffed head being
// lost. A declared type that is neither empty nor application/octet-stream is
// trusted as-is (matching resolveContentType), and the original reader is
// returned untouched.
func resolveContentTypeStreaming(declared string, r io.Reader) (string, io.Reader, error) {
	if ct := strings.TrimSpace(declared); ct != "" &&
		!strings.EqualFold(mediaType(ct), "application/octet-stream") {
		return ct, r, nil
	}

	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", nil, err
	}
	head = head[:n]
	body := io.MultiReader(bytes.NewReader(head), r)
	if n == 0 {
		return declared, body, nil // empty part — nothing to sniff
	}
	return http.DetectContentType(head), body, nil
}

// tempFileReader is an *os.File that removes its backing temp file on Close. A
// non-streaming file part in a streaming request is spilled to a temp file
// (net/http's own multipart temp machinery is not reachable once we drive the
// reader ourselves), and this makes that temp file clean up through the very
// same ctx.Files reader-close the buffered path relies on.
type tempFileReader struct {
	*os.File
	path string
}

func (t *tempFileReader) Close() error {
	err := t.File.Close()
	if rmErr := os.Remove(t.path); err == nil {
		err = rmErr
	}
	return err
}

// maxFormValueBytes caps a single non-file part in the streaming multipart path,
// mirroring net/http's own 10 MB default for form values. The whole body is
// already bounded by MaxUploadBytes; this stops one scalar part from claiming
// all of it.
const maxFormValueBytes = 10 << 20

// readFormValue reads a non-file multipart part as a string, bounded so a single
// value cannot consume the whole request budget.
func readFormValue(part *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, maxFormValueBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > maxFormValueBytes {
		return "", fmt.Errorf("form value %q exceeds %d bytes", part.FormName(), maxFormValueBytes)
	}
	return string(b), nil
}

// bufferPartToUploadedFile spills a non-streaming file part to a temp file and
// returns it as a seekable UploadedFile, exactly as the buffered path would have
// via ParseMultipartForm. It exists because a model can carry both a streaming
// and a buffered file field, and once we drive the multipart reader ourselves
// (for the streaming field) the buffered field's part is ours to materialise.
// The returned Reader deletes the temp file on Close, so the standard ctx.Files
// cleanup collects it.
func bufferPartToUploadedFile(part *multipart.Part) (*UploadedFile, error) {
	tmp, err := os.CreateTemp("", "mfx-upload-*")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	discard := func() {
		tmp.Close()
		os.Remove(path)
	}

	n, err := io.Copy(tmp, part)
	if err != nil {
		discard()
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		discard()
		return nil, err
	}
	contentType, err := resolveContentType(part.Header.Get("Content-Type"), tmp)
	if err != nil {
		discard()
		return nil, err
	}
	return &UploadedFile{
		Filename:    part.FileName(),
		ContentType: contentType,
		Size:        n,
		Reader:      &tempFileReader{File: tmp, path: path},
	}, nil
}

// parseMultipartStreaming is the multipart parser used when the model has at
// least one mfx:"file,upload:stream" field. It drives the multipart reader part
// by part instead of ParseMultipartForm, so a streamed file field's bytes go
// straight to storage as they arrive off the socket rather than landing on the
// app server's disk first. Value parts and any non-streaming file parts are
// handled exactly as the buffered path would handle them, so a mixed model keeps
// working. Models with no streaming field never reach this — they take the
// unchanged ParseMultipartForm path — so existing uploads are untouched.
func (s *defaultSteps) parseMultipartStreaming(ctx *ServerContext) error {
	limitMultipartBody(ctx, s.uploadLimit(ctx))

	mr, err := ctx.Request.MultipartReader()
	if err != nil {
		ctx.Abort(http.StatusBadRequest, "MULTIPART_ERROR",
			fmt.Sprintf("failed to read multipart form: %s", err.Error()))
		return err
	}

	if ctx.ParsedBody == nil {
		ctx.ParsedBody = NewRequestBody(nil)
	}
	present := make(map[string]struct{})
	files := make(map[string]*UploadedFile)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s.abortMultipartRead(ctx, err)
		}
		if err := s.handleStreamPart(ctx, part, present, files); err != nil {
			return err
		}
	}

	ctx.present = present
	ctx.Files = files
	return nil
}

// handleStreamPart routes one multipart part: a form value into ParsedBody, an
// upload:stream file field straight to storage, and any other file part to a
// temp file so the Service step handles it as a normal buffered upload.
func (s *defaultSteps) handleStreamPart(ctx *ServerContext, part *multipart.Part, present map[string]struct{}, files map[string]*UploadedFile) error {
	defer part.Close()

	name := part.FormName()
	present[name] = struct{}{}

	if part.FileName() == "" {
		val, err := readFormValue(part)
		if err != nil {
			if isBodyTooLarge(err) {
				return s.abortMultipartRead(ctx, err)
			}
			ctx.Abort(http.StatusBadRequest, "MULTIPART_ERROR",
				fmt.Sprintf("failed to read form value %q: %s", name, err.Error()))
			return err
		}
		ctx.ParsedBody.m[name] = val
		return nil
	}

	field := ctx.Model.FieldByJSONName(name)
	if field != nil && field.Tags.File && field.Tags.StreamUpload && !field.IsFileList() {
		return s.streamStoreFile(ctx, *field, part)
	}

	// A buffered file field (or a file part matching no streaming field):
	// materialise it to a temp file so the Service step sees it as usual.
	uf, err := bufferPartToUploadedFile(part)
	if err != nil {
		if isBodyTooLarge(err) {
			return s.abortMultipartRead(ctx, err)
		}
		ctx.Abort(http.StatusBadRequest, "FILE_READ_ERROR",
			fmt.Sprintf("failed to read uploaded file %q: %s", name, err.Error()))
		return err
	}
	files[name] = uf
	return nil
}

// abortMultipartRead maps a multipart read error to the right response: the
// body-cap overflow becomes 413, anything else a 400.
func (s *defaultSteps) abortMultipartRead(ctx *ServerContext, err error) error {
	if isBodyTooLarge(err) {
		_ = ctx.abortBodyTooLarge(s.uploadLimit(ctx))
		return err
	}
	ctx.Abort(http.StatusBadRequest, "MULTIPART_ERROR",
		fmt.Sprintf("failed to parse multipart form: %s", err.Error()))
	return err
}

// streamStoreFile stores an upload:stream field's part directly to storage as it
// streams off the socket, then records the resulting key in the body so it flows
// to the DB like any other file field. It mirrors handleFileUpload's rules and
// bookkeeping, with two differences forced by streaming: accept is checked from
// the sniffed head before a byte is stored, while max_size — unknowable until
// the object is fully read — is enforced by measuredReader mid-stream, and the
// key is tracked for cleanup before Store so a mid-stream abort's partial object
// is collected by the request's non-2xx file rollback.
func (s *defaultSteps) streamStoreFile(ctx *ServerContext, field FieldMeta, part *multipart.Part) error {
	jn := field.Tags.JSONName

	contentType, body, err := resolveContentTypeStreaming(part.Header.Get("Content-Type"), part)
	if err != nil {
		if isBodyTooLarge(err) {
			_ = ctx.abortBodyTooLarge(s.uploadLimit(ctx))
			return err
		}
		ctx.Abort(http.StatusInternalServerError, "FILE_READ_ERROR",
			fmt.Sprintf("failed to read uploaded file %q: %s", jn, err.Error()))
		return err
	}
	if !matchesAccept(contentType, field.Tags.Accept) {
		ctx.Abort(http.StatusUnsupportedMediaType, "FILE_TYPE_NOT_ALLOWED",
			fmt.Sprintf("file %q has type %q, allowed: %v", jn, contentType, field.Tags.Accept))
		return fmt.Errorf("file type not allowed")
	}

	// On update with auto_delete, queue the replaced object for deletion after
	// the write commits, not now — a later failure would otherwise orphan the
	// row while its blob is already gone (BUG-1). Same as handleFileUpload.
	if ctx.Operation == OpUpdate && field.Tags.AutoDelete {
		if oldKey, ok := s.getOldFileKey(ctx, field); ok && oldKey != "" {
			ctx.trackReplacedFile(oldKey, s.storage)
		}
	}

	key := applyKeyScope(resolveKeyScope(s.keyScope, ctx), fmt.Sprintf("%s/%s/%s/%s",
		ctx.Model.TableName,
		uuid.New().String(),
		field.Tags.DBName,
		sanitizeFilename(part.FileName())))

	// Track before Store: if the measured reader trips max_size partway through,
	// the object is already partly written, and the request's non-2xx cleanup
	// (cleanupOrphanedFiles) is what removes it (3B.2b).
	ctx.TrackStoredFile(key, s.storage)

	reader := &measuredReader{r: body, max: field.Tags.MaxSize}
	meta := FileMeta{
		Key:         key,
		ContentType: contentType,
		Filename:    part.FileName(),
		// Size is unknown until the stream is fully read; the backend receives 0
		// and must store an unsized reader (S3: multipart upload). It is filled in
		// on the record from reader.n after Store returns, below.
	}
	if err := s.storage.Store(ctx.Ctx, key, reader, meta); err != nil {
		if errors.Is(err, errStreamTooLarge) {
			ctx.Abort(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE",
				fmt.Sprintf("file %q exceeds maximum size of %d bytes", jn, field.Tags.MaxSize))
			return err
		}
		if isBodyTooLarge(err) {
			_ = ctx.abortBodyTooLarge(s.uploadLimit(ctx))
			return err
		}
		ctx.Abort(http.StatusInternalServerError, "FILE_STORE_ERROR",
			fmt.Sprintf("failed to store file %q: %s", jn, err.Error()))
		return err
	}

	// The key flows to the DB as the field's value, and the field is marked so the
	// Service step does not re-process it as a key reference (an extra Stat).
	ctx.SetField(jn, key)
	ctx.markStreamedFile(jn)
	return nil
}
