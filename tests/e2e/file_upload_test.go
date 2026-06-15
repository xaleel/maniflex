package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	maniflex "github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Real magic-number prefixes so http.DetectContentType sniffs the right type.
// The backend determines content-type from bytes, ignoring client-declared values.
var (
	fakePDF  = []byte("%PDF-1.4\n% fake pdf body for tests\n%%EOF")
	fakePNG  = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1}
	fakeJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9}
)

// fileServer returns a test server configured with MemoryStorage and file models.
func fileServer(t *testing.T, storage *testutil.MemoryStorage) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models:      testutil.FileModels(),
		FileStorage: storage,
	})
}

func TestFileUpload(t *testing.T) {
	t.Parallel()

	// ── Standalone endpoints ─────────────────────────────────────────────────

	t.Run("standalone_upload_returns_201_with_metadata", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {
				Filename:    "readme.txt",
				ContentType: "text/plain",
				Body:        []byte("hello world"),
			},
		})
		resp.AssertStatus(http.StatusCreated)
		data := resp.Data()
		key := testutil.Field(t, data, "key")
		testutil.AssertNotEmpty(t, "key", key)
		testutil.AssertEqual(t, "content_type", testutil.Field(t, data, "content_type"), "text/plain")
		testutil.AssertEqual(t, "filename", testutil.Field(t, data, "filename"), "readme.txt")

		if !store.HasKey(key) {
			t.Errorf("file not found in storage with key %q", key)
		}
	})

	t.Run("standalone_serve_returns_file_content", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Upload first
		uploadResp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "hello.txt", ContentType: "text/plain", Body: []byte("file content here")},
		})
		uploadResp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, uploadResp.Data(), "key")

		// Serve
		resp := srv.GETRaw("/files/" + key)
		resp.AssertStatus(http.StatusOK)
		if string(resp.Body) != "file content here" {
			t.Errorf("body: got %q, want %q", string(resp.Body), "file content here")
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
			t.Errorf("Content-Type: got %q, want %q", ct, "text/plain")
		}
	})

	t.Run("standalone_serve_missing_key_returns_404", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.GETRaw("/files/nonexistent/key.txt")
		resp.AssertStatus(http.StatusNotFound)
	})

	t.Run("standalone_delete_returns_204", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Upload
		uploadResp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "todelete.txt", ContentType: "text/plain", Body: []byte("bye")},
		})
		uploadResp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, uploadResp.Data(), "key")

		// Delete
		srv.DELETE("/files/" + key).AssertStatus(http.StatusNoContent)

		// Verify gone
		if store.HasKey(key) {
			t.Error("file should have been deleted from storage")
		}
		// Serve after delete → 404
		srv.GETRaw("/files/" + key).AssertStatus(http.StatusNotFound)
	})

	t.Run("standalone_delete_missing_returns_404", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)
		srv.DELETE("/files/does/not/exist.txt").AssertStatus(http.StatusNotFound)
	})

	// ── Model multipart create ───────────────────────────────────────────────

	t.Run("multipart_create_returns_201_with_file_key", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.POSTMultipart("/documents", map[string]string{
			"title": "My Doc",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "report.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		resp.AssertStatus(http.StatusCreated)

		data := resp.Data()
		fileKey := testutil.Field(t, data, "file")
		testutil.AssertNotEmpty(t, "file key", fileKey)
		testutil.AssertEqual(t, "title", testutil.Field(t, data, "title"), "My Doc")

		if !store.HasKey(fileKey) {
			t.Errorf("file not stored at key %q", fileKey)
		}
	})

	t.Run("multipart_update_replaces_file_and_deletes_old", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Create
		createResp := srv.POSTMultipart("/documents", map[string]string{
			"title": "Doc",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "v1.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		createResp.AssertStatus(http.StatusCreated)
		id := createResp.ID()
		oldKey := testutil.Field(t, createResp.Data(), "file")

		// Update with new file
		updateResp := srv.PATCHMultipart("/documents/"+id, nil, map[string]testutil.FileUpload{
			"file": {Filename: "v2.pdf", ContentType: "application/pdf", Body: append([]byte("%PDF-1.4 v2\n"), fakePDF...)},
		})
		updateResp.AssertStatus(http.StatusOK)
		newKey := testutil.Field(t, updateResp.Data(), "file")

		if newKey == oldKey {
			t.Error("new key should differ from old key")
		}
		if store.HasKey(oldKey) {
			t.Error("old file should have been deleted (auto_delete default)")
		}
		if !store.HasKey(newKey) {
			t.Error("new file should be in storage")
		}
	})

	// ── JSON create with pre-uploaded key (file reuse) ───────────────────────

	t.Run("json_create_with_preexisting_key", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Pre-upload via standalone endpoint
		uploadResp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "shared.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		uploadResp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, uploadResp.Data(), "key")

		// Create document referencing the pre-uploaded key
		resp := srv.POST("/documents", map[string]any{
			"title": "Reuse Doc",
			"file":  key,
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "file key", testutil.Field(t, resp.Data(), "file"), key)
	})

	t.Run("json_create_with_invalid_key_returns_422", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.POST("/documents", map[string]any{
			"title": "Bad Ref",
			"file":  "nonexistent/key/file.pdf",
		})
		resp.AssertStatus(http.StatusUnprocessableEntity)
	})

	// ── File key reuse across multiple records ───────────────────────────────

	t.Run("file_key_reuse_across_records", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Upload once
		uploadResp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "shared.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		uploadResp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, uploadResp.Data(), "key")

		// Reference from two different records
		r1 := srv.POST("/documents", map[string]any{"title": "Doc1", "file": key})
		r1.AssertStatus(http.StatusCreated)
		r2 := srv.POST("/documents", map[string]any{"title": "Doc2", "file": key})
		r2.AssertStatus(http.StatusCreated)

		testutil.AssertEqual(t, "doc1 file", testutil.Field(t, r1.Data(), "file"), key)
		testutil.AssertEqual(t, "doc2 file", testutil.Field(t, r2.Data(), "file"), key)
	})

	// ── Validation: size and type constraints ────────────────────────────────

	t.Run("file_too_large_returns_413", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Document.File has max_size:1MB — send 2MB
		bigBody := make([]byte, 2*1024*1024)
		resp := srv.POSTMultipart("/documents", map[string]string{
			"title": "Big",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "huge.pdf", ContentType: "application/pdf", Body: bigBody},
		})
		resp.AssertStatus(http.StatusRequestEntityTooLarge)
	})

	t.Run("wrong_content_type_returns_415", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Document.File accepts application/pdf|text/plain — send image/png
		resp := srv.POSTMultipart("/documents", map[string]string{
			"title": "WrongType",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "pic.png", ContentType: "image/png", Body: fakePNG},
		})
		resp.AssertStatus(http.StatusUnsupportedMediaType)
	})

	t.Run("missing_required_file_returns_422", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Document.File is required — omit it
		resp := srv.POST("/documents", map[string]any{
			"title": "NoFile",
		})
		resp.AssertStatus(http.StatusUnprocessableEntity)
	})

	// ── Hard-delete cleans up files ──────────────────────────────────────────

	t.Run("hard_delete_removes_file_from_storage", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.POSTMultipart("/documents", map[string]string{
			"title": "Deletable",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "doc.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		resp.AssertStatus(http.StatusCreated)
		id := resp.ID()
		fileKey := testutil.Field(t, resp.Data(), "file")

		srv.DELETE("/documents/" + id).AssertStatus(http.StatusNoContent)

		// The file cleanup runs asynchronously in a goroutine — give it a moment
		time.Sleep(100 * time.Millisecond)

		if store.HasKey(fileKey) {
			t.Error("file should be deleted from storage after hard delete")
		}
	})

	// ── Soft-delete preserves files ──────────────────────────────────────────

	t.Run("soft_delete_preserves_file_in_storage", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.POSTMultipart("/soft_docs", map[string]string{
			"name": "Soft",
		}, map[string]testutil.FileUpload{
			"attach": {Filename: "keep.txt", ContentType: "text/plain", Body: []byte("keep me")},
		})
		resp.AssertStatus(http.StatusCreated)
		id := resp.ID()
		key := testutil.Field(t, resp.Data(), "attach")

		srv.DELETE("/soft_docs/" + id).AssertStatus(http.StatusNoContent)
		time.Sleep(100 * time.Millisecond)

		if !store.HasKey(key) {
			t.Error("file should persist after soft delete")
		}
	})

	// ── auto_delete:false preserves old file on update ───────────────────────

	t.Run("auto_delete_false_preserves_old_file_on_update", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Create with both file and icon
		resp := srv.POSTMultipart("/documents", map[string]string{
			"title": "WithIcon",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "doc.pdf", ContentType: "application/pdf", Body: fakePDF},
			"icon": {Filename: "icon.png", ContentType: "image/png", Body: fakePNG},
		})
		resp.AssertStatus(http.StatusCreated)
		id := resp.ID()
		oldIconKey := testutil.Field(t, resp.Data(), "icon")

		// Update icon
		updateResp := srv.PATCHMultipart("/documents/"+id, nil, map[string]testutil.FileUpload{
			"icon": {Filename: "icon2.png", ContentType: "image/png", Body: append([]byte{}, fakePNG...)},
		})
		updateResp.AssertStatus(http.StatusOK)
		newIconKey := testutil.Field(t, updateResp.Data(), "icon")

		if newIconKey == oldIconKey {
			t.Error("new icon key should differ from old")
		}
		// auto_delete:false — old icon should still exist
		if !store.HasKey(oldIconKey) {
			t.Error("old icon should persist (auto_delete:false)")
		}
		if !store.HasKey(newIconKey) {
			t.Error("new icon should exist in storage")
		}
	})

	// ── No storage configured ────────────────────────────────────────────────

	t.Run("no_storage_standalone_upload_returns_501", func(t *testing.T) {
		t.Parallel()
		// Server with NO FileStorage — standalone endpoint should 501
		srv := testutil.NewServer(t, testutil.Options{
			Models: testutil.FileModels(),
			// FileStorage intentionally nil
		})

		resp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "test.txt", ContentType: "text/plain", Body: []byte("hi")},
		})
		// When no storage is configured, /files routes are not mounted at all,
		// so we expect 404 (Method Not Allowed or Not Found).
		if resp.Status != http.StatusNotFound && resp.Status != http.StatusMethodNotAllowed {
			t.Errorf("expected 404 or 405 with no storage configured, got %d", resp.Status)
		}
	})

	// ── Icon field accepts image/* wildcard ──────────────────────────────────

	t.Run("icon_field_accepts_image_wildcard", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Icon accepts image/* — send image/jpeg
		resp := srv.POSTMultipart("/documents", map[string]string{
			"title": "ImageDoc",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "f.pdf", ContentType: "application/pdf", Body: fakePDF},
			"icon": {Filename: "photo.jpg", ContentType: "image/jpeg", Body: fakeJPEG},
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertNotEmpty(t, "icon key", testutil.Field(t, resp.Data(), "icon"))
	})

	t.Run("icon_field_rejects_non_image", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.POSTMultipart("/documents", map[string]string{
			"title": "BadIcon",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "f.pdf", ContentType: "application/pdf", Body: fakePDF},
			"icon": {Filename: "doc.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		resp.AssertStatus(http.StatusUnsupportedMediaType)
	})

	// ── File serving via key from model record ───────────────────────────────

	t.Run("serve_file_uploaded_via_model_endpoint", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		fileContent := append([]byte("%PDF-1.4\nThis is a test PDF content\n%%EOF"), 0x00)
		createResp := srv.POSTMultipart("/documents", map[string]string{
			"title": "Servable",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "serve.pdf", ContentType: "application/pdf", Body: fileContent},
		})
		createResp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, createResp.Data(), "file")

		// The file key should be servable via GET /files/*
		serveResp := srv.GETRaw("/files/" + key)
		serveResp.AssertStatus(http.StatusOK)
		if string(serveResp.Body) != string(fileContent) {
			t.Errorf("served body: got %q, want %q", string(serveResp.Body), string(fileContent))
		}
	})

	// ── Read record returns file key (not file content) ──────────────────────

	t.Run("read_record_returns_file_key_as_string", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		createResp := srv.POSTMultipart("/documents", map[string]string{
			"title": "ReadBack",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "r.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		createResp.AssertStatus(http.StatusCreated)
		id := createResp.ID()
		key := testutil.Field(t, createResp.Data(), "file")

		// GET the record — file field should be a string key
		readResp := srv.GET("/documents/" + id)
		readResp.AssertStatus(http.StatusOK)
		readKey := testutil.Field(t, readResp.Data(), "file")
		testutil.AssertEqual(t, "file key on read", readKey, key)

		// Key should contain the model table name and original filename
		if !strings.Contains(readKey, "documents") && !strings.Contains(readKey, "mfx_documents") {
			t.Errorf("file key %q should contain the table name", readKey)
		}
	})

	// ── OpenAPI includes file endpoints ──────────────────────────────────────

	t.Run("openapi_includes_file_endpoints", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.GET("/openapi.json")
		resp.AssertStatus(http.StatusOK)

		resp.AssertJSON(func(body map[string]any) {
			paths, ok := body["paths"].(map[string]any)
			if !ok {
				t.Fatal("no paths in OpenAPI spec")
			}

			// OpenAPI paths are relative to the server URL (prefix stripped)
			if _, ok := paths["/files"]; !ok {
				t.Errorf("missing /files path in OpenAPI, available paths: %v", pathKeys(paths))
			}
			if _, ok := paths["/files/{key}"]; !ok {
				t.Errorf("missing /files/{key} path in OpenAPI, available paths: %v", pathKeys(paths))
			}
		})
	})

	// ── OpenAPI model endpoint has multipart content ─────────────────────────

	t.Run("openapi_document_has_multipart_content_type", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.GET("/openapi.json")
		resp.AssertStatus(http.StatusOK)

		resp.AssertJSON(func(body map[string]any) {
			paths, _ := body["paths"].(map[string]any)
			// Find the POST documents endpoint
			var docPath map[string]any
			for k, v := range paths {
				if strings.Contains(k, "documents") && !strings.Contains(k, "{") {
					docPath, _ = v.(map[string]any)
					break
				}
			}
			if docPath == nil {
				t.Fatal("no documents path found in OpenAPI")
			}
			post, ok := docPath["post"].(map[string]any)
			if !ok {
				t.Fatal("no POST operation on documents")
			}
			reqBody, ok := post["requestBody"].(map[string]any)
			if !ok {
				t.Fatal("no requestBody on POST documents")
			}
			content, ok := reqBody["content"].(map[string]any)
			if !ok {
				t.Fatal("no content in requestBody")
			}
			if _, ok := content["multipart/form-data"]; !ok {
				t.Error("POST documents should have multipart/form-data content type")
			}
		})
	})
}

// TestFileUploadNoModels verifies that file endpoints don't interfere when
// models have no file fields.
func TestFileUploadNoModels(t *testing.T) {
	t.Parallel()

	t.Run("standard_models_with_storage_still_work", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			FileStorage: store,
		})

		// Standard CRUD should work fine
		resp := srv.POST("/users", map[string]any{
			"name": "Alice", "email": "alice@test.com",
			"password": "secret", "role": "admin",
		})
		resp.AssertStatus(http.StatusCreated)

		// Standalone file upload should also work
		uploadResp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "test.txt", ContentType: "text/plain", Body: []byte("hi")},
		})
		uploadResp.AssertStatus(http.StatusCreated)
	})
}

// TestFileUploadConcurrency verifies file operations under concurrent access.
func TestFileUploadConcurrency(t *testing.T) {
	t.Parallel()

	t.Run("concurrent_uploads_to_different_records", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		const n = 5
		type result struct {
			id  string
			key string
		}
		results := make(chan result, n)
		errs := make(chan error, n)

		for i := range n {
			go func(i int) {
				resp := srv.POSTMultipart("/documents", map[string]string{
					"title": "Concurrent " + string(rune('A'+i)),
				}, map[string]testutil.FileUpload{
					"file": {
						Filename:    "file.pdf",
						ContentType: "application/pdf",
						Body:        fakePDF,
					},
				})
				if resp.Status != http.StatusCreated {
					errs <- &concErr{status: resp.Status, body: string(resp.Body)}
					return
				}
				results <- result{id: resp.ID(), key: testutil.Field(t, resp.Data(), "file")}
			}(i)
		}

		seen := make(map[string]bool)
		for range n {
			select {
			case r := <-results:
				if seen[r.key] {
					t.Errorf("duplicate file key: %s", r.key)
				}
				seen[r.key] = true
			case err := <-errs:
				t.Errorf("concurrent create failed: %v", err)
			}
		}

		if len(seen) != n {
			t.Errorf("expected %d unique keys, got %d", n, len(seen))
		}
	})
}

type concErr struct {
	status int
	body   string
}

func (e *concErr) Error() string {
	return "status " + http.StatusText(e.status) + ": " + e.body
}

// TestFileUploadEdgeCases covers edge-case scenarios.
func TestFileUploadEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("update_only_non_file_field_leaves_file_intact", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		createResp := srv.POSTMultipart("/documents", map[string]string{
			"title": "Original",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "doc.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		createResp.AssertStatus(http.StatusCreated)
		id := createResp.ID()
		fileKey := testutil.Field(t, createResp.Data(), "file")

		// PATCH only the title — file should remain unchanged
		updateResp := srv.PATCH("/documents/"+id, map[string]any{
			"title": "Updated Title",
		})
		updateResp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "file key unchanged",
			testutil.Field(t, updateResp.Data(), "file"), fileKey)

		if !store.HasKey(fileKey) {
			t.Error("file should still exist in storage")
		}
	})

	t.Run("empty_string_file_key_on_create_stores_empty", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// An empty string satisfies "required" (value is present and non-nil)
		// but is not treated as a file key reference — it flows through as-is.
		resp := srv.POST("/documents", map[string]any{
			"title": "Empty",
			"file":  "",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "file", testutil.Field(t, resp.Data(), "file"), "")
	})

	t.Run("optional_file_field_can_be_omitted", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Document.Icon is optional — omit it
		resp := srv.POSTMultipart("/documents", map[string]string{
			"title": "NoIcon",
		}, map[string]testutil.FileUpload{
			"file": {Filename: "doc.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		resp.AssertStatus(http.StatusCreated)

		// Icon field should be empty
		data := resp.Data()
		icon, _ := data["icon"].(string)
		if icon != "" {
			t.Errorf("icon should be empty, got %q", icon)
		}
	})

	t.Run("delete_file_via_standalone_then_model_reference_fails", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		// Upload via standalone
		uploadResp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: "temp.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
		uploadResp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, uploadResp.Data(), "key")

		// Delete via standalone
		srv.DELETE("/files/" + key).AssertStatus(http.StatusNoContent)

		// Now try to create a document referencing the deleted key
		resp := srv.POST("/documents", map[string]any{
			"title": "Dangling",
			"file":  key,
		})
		resp.AssertStatus(http.StatusUnprocessableEntity)
	})

	t.Run("model_create_with_soft_delete_and_file", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.POSTMultipart("/soft_docs", map[string]string{
			"name": "SoftCreate",
		}, map[string]testutil.FileUpload{
			"attach": {Filename: "attach.txt", ContentType: "text/plain", Body: []byte("attached")},
		})
		resp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, resp.Data(), "attach")
		testutil.AssertNotEmpty(t, "attach key", key)

		if !store.HasKey(key) {
			t.Error("file should be in storage")
		}
	})
}

// TestFileOrphanCleanup verifies 3B.2b: a file stored during the Service step is
// deleted from storage when the request fails afterwards, so a failed
// create/update never leaks blobs.
func TestFileOrphanCleanup(t *testing.T) {
	t.Parallel()

	// waitForEmpty polls the store until it holds no keys, or fails after a
	// short deadline. Orphan cleanup runs on the background runner, so the
	// deletion is observed asynchronously (mirrors the hard-delete tests).
	waitForEmpty := func(t *testing.T, store *testutil.MemoryStorage) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(store.Keys()) == 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Errorf("expected stored file to be cleaned up, storage still has keys: %v", store.Keys())
	}

	t.Run("db_step_failure_deletes_stored_file", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FileStorage: store,
			Middleware: func(s *maniflex.Server) {
				// Runs after the Service step stored the file but before the
				// default DB handler — short-circuits with a non-2xx response.
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusConflict, "FORCED_DB_FAILURE", "simulated DB failure")
					return nil
				}, maniflex.ForModel("Document"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})

		resp := srv.POSTMultipart("/documents", map[string]string{"title": "Orphan"},
			map[string]testutil.FileUpload{
				"file": {Filename: "orphan.pdf", ContentType: "application/pdf", Body: fakePDF},
			})
		resp.AssertStatus(http.StatusConflict)
		waitForEmpty(t, store)
	})

	t.Run("post_service_abort_deletes_stored_file", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      testutil.FileModels(),
			FileStorage: store,
			Middleware: func(s *maniflex.Server) {
				// An After-Service middleware that aborts post-store — the case
				// the eager Service-step storage made unsafe before 3B.2b.
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnprocessableEntity, "POLICY_DENIED", "denied after store")
					return nil
				}, maniflex.AtPosition(maniflex.After), maniflex.ForModel("Document"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})

		resp := srv.POSTMultipart("/documents", map[string]string{"title": "Orphan"},
			map[string]testutil.FileUpload{
				"file": {Filename: "orphan.pdf", ContentType: "application/pdf", Body: fakePDF},
			})
		resp.AssertStatus(http.StatusUnprocessableEntity)
		waitForEmpty(t, store)
	})

	t.Run("successful_create_keeps_stored_file", func(t *testing.T) {
		t.Parallel()
		store := testutil.NewMemoryStorage()
		srv := fileServer(t, store)

		resp := srv.POSTMultipart("/documents", map[string]string{"title": "Kept"},
			map[string]testutil.FileUpload{
				"file": {Filename: "kept.pdf", ContentType: "application/pdf", Body: fakePDF},
			})
		resp.AssertStatus(http.StatusCreated)
		key := testutil.Field(t, resp.Data(), "file")

		// Give any (incorrect) async cleanup a chance to run, then assert the
		// file is still present on the success path.
		time.Sleep(100 * time.Millisecond)
		if !store.HasKey(key) {
			t.Errorf("file %q should be retained on a successful create", key)
		}
	})
}

// pathKeys extracts map keys for diagnostic messages.
func pathKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
