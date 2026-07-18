package e2e

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	maniflex "github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// 3B.2a — streaming uploads. A model field tagged mfx:"file,upload:stream" (and
// the standalone POST /files, always) pipes its bytes straight to storage as
// they arrive off the socket, instead of buffering the whole request to disk
// first.
//
// Teeth-check baked into every case via sizeSpyStorage: the streaming path hands
// FileStorage.Store a zero meta.Size (the length is not known until the stream is
// read), while the buffered path hands it the real size. So a captured Store size
// of 0 proves the streaming path ran, and a non-zero size proves the buffered one
// did — if streaming silently fell back to buffering, the 0 assertions fail.

// sizeSpyStorage records the meta.Size passed to each Store call, then delegates
// to a real in-memory backend.
type sizeSpyStorage struct {
	*testutil.MemoryStorage
	mu    sync.Mutex
	sizes []int64
}

func newSizeSpy() *sizeSpyStorage {
	return &sizeSpyStorage{MemoryStorage: testutil.NewMemoryStorage()}
}

func (s *sizeSpyStorage) Store(ctx context.Context, key string, r io.Reader, meta maniflex.FileMeta) error {
	s.mu.Lock()
	s.sizes = append(s.sizes, meta.Size)
	s.mu.Unlock()
	return s.MemoryStorage.Store(ctx, key, r, meta)
}

func (s *sizeSpyStorage) storeSizes() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.sizes...)
}

type streamVideoDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required"`
	Video string `json:"video" db:"video" mfx:"file,upload:stream"`
}

type streamClipDoc struct {
	maniflex.BaseModel
	Clip string `json:"clip" db:"clip" mfx:"file,upload:stream,accept:application/pdf,max_size:1KB"`
}

// streamMixedDoc carries one streamed file field and one buffered one, so the
// streaming parser has to store the first and spill the second to a temp file in
// a single request.
type streamMixedDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required"`
	Video string `json:"video" db:"video" mfx:"file,upload:stream"`
	Thumb string `json:"thumb" db:"thumb" mfx:"file"`
}

func waitForNoKeys(t *testing.T, store *sizeSpyStorage) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(store.Keys()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("expected the partial object to be cleaned up, storage still holds: %v", store.Keys())
}

func TestStreamUpload_Model(t *testing.T) {
	t.Parallel()

	t.Run("stores_streaming_and_serves_back", func(t *testing.T) {
		t.Parallel()
		store := newSizeSpy()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      []any{streamVideoDoc{}},
			FileStorage: store,
		})

		body := []byte("streamed video bytes, not buffered to disk first")
		resp := srv.POSTMultipart("/stream_video_docs", map[string]string{"title": "Clip"},
			map[string]testutil.FileUpload{
				"video": {Filename: "clip.mp4", ContentType: "video/mp4", Body: body},
			})
		resp.AssertStatus(http.StatusCreated)

		key := testutil.Field(t, resp.Data(), "video")
		testutil.AssertNotEmpty(t, "video key", key)
		if !store.HasKey(key) {
			t.Fatalf("streamed object not in storage at %q", key)
		}

		// Teeth-check: the streaming path passed Store a zero size.
		if sizes := store.storeSizes(); len(sizes) != 1 || sizes[0] != 0 {
			t.Errorf("Store sizes = %v, want exactly [0] (streaming path passes size 0)", sizes)
		}

		// The stored bytes must be intact — the sniff-and-replay must not drop the head.
		served := srv.GETRaw("/files/" + key)
		served.AssertStatus(http.StatusOK)
		if string(served.Body) != string(body) {
			t.Errorf("served body = %q, want %q", served.Body, body)
		}
	})

	t.Run("accept_rejects_wrong_type_before_storing", func(t *testing.T) {
		t.Parallel()
		store := newSizeSpy()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      []any{streamClipDoc{}},
			FileStorage: store,
		})

		// Clip accepts application/pdf; send text/plain.
		resp := srv.POSTMultipart("/stream_clip_docs", nil,
			map[string]testutil.FileUpload{
				"clip": {Filename: "note.txt", ContentType: "text/plain", Body: []byte("hello")},
			})
		resp.AssertStatus(http.StatusUnsupportedMediaType)

		// Accept is checked from the head before a byte is stored, so nothing was written.
		if got := store.Keys(); len(got) != 0 {
			t.Errorf("a rejected type still stored something: %v", got)
		}
		if sizes := store.storeSizes(); len(sizes) != 0 {
			t.Errorf("Store was called for a rejected type: sizes=%v", sizes)
		}
	})

	t.Run("max_size_rejects_midstream_and_cleans_up", func(t *testing.T) {
		t.Parallel()
		store := newSizeSpy()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      []any{streamClipDoc{}},
			FileStorage: store,
		})

		// Clip is max_size:1KB. Declare application/pdf so accept passes, then send
		// 2KB so the measured reader trips mid-stream.
		big := make([]byte, 2048)
		copy(big, "%PDF-1.4\n")
		resp := srv.POSTMultipart("/stream_clip_docs", nil,
			map[string]testutil.FileUpload{
				"clip": {Filename: "big.pdf", ContentType: "application/pdf", Body: big},
			})
		resp.AssertStatus(http.StatusRequestEntityTooLarge)

		// Store was entered (proving mid-stream enforcement, not an up-front check),
		// so a partial object existed; the request's non-2xx cleanup must remove it.
		if sizes := store.storeSizes(); len(sizes) != 1 || sizes[0] != 0 {
			t.Errorf("Store sizes = %v, want [0] (max_size is a mid-stream trip)", sizes)
		}
		waitForNoKeys(t, store)
	})

	t.Run("mixed_streamed_and_buffered_in_one_request", func(t *testing.T) {
		t.Parallel()
		store := newSizeSpy()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      []any{streamMixedDoc{}},
			FileStorage: store,
		})

		videoBody := []byte("the streamed video part")
		thumbBody := fakePNG
		resp := srv.POSTMultipart("/stream_mixed_docs", map[string]string{"title": "Mix"},
			map[string]testutil.FileUpload{
				"video": {Filename: "v.mp4", ContentType: "video/mp4", Body: videoBody},
				"thumb": {Filename: "t.png", ContentType: "image/png", Body: thumbBody},
			})
		resp.AssertStatus(http.StatusCreated)

		videoKey := testutil.Field(t, resp.Data(), "video")
		thumbKey := testutil.Field(t, resp.Data(), "thumb")
		testutil.AssertEqual(t, "title", testutil.Field(t, resp.Data(), "title"), "Mix")
		if !store.HasKey(videoKey) || !store.HasKey(thumbKey) {
			t.Fatalf("both objects should be stored: video=%v thumb=%v",
				store.HasKey(videoKey), store.HasKey(thumbKey))
		}

		// Teeth-check: one Store got size 0 (the streamed video), the other a real
		// size (the buffered thumb). If both streamed or both buffered, this fails.
		var zero, nonzero int
		for _, s := range store.storeSizes() {
			if s == 0 {
				zero++
			} else {
				nonzero++
			}
		}
		if zero != 1 || nonzero != 1 {
			t.Errorf("Store sizes = %v, want one 0 (streamed) and one non-zero (buffered)", store.storeSizes())
		}

		// Both must serve back intact.
		if got := srv.GETRaw("/files/" + videoKey); string(got.Body) != string(videoBody) {
			t.Errorf("streamed video served %q, want %q", got.Body, videoBody)
		}
		if got := srv.GETRaw("/files/" + thumbKey); string(got.Body) != string(thumbBody) {
			t.Errorf("buffered thumb served %q, want %q", got.Body, thumbBody)
		}
	})

	t.Run("update_replaces_and_autodeletes_old", func(t *testing.T) {
		t.Parallel()
		store := newSizeSpy()
		srv := testutil.NewServer(t, testutil.Options{
			Models:      []any{streamVideoDoc{}},
			FileStorage: store,
		})

		create := srv.POSTMultipart("/stream_video_docs", map[string]string{"title": "V1"},
			map[string]testutil.FileUpload{
				"video": {Filename: "v1.mp4", ContentType: "video/mp4", Body: []byte("version one")},
			})
		create.AssertStatus(http.StatusCreated)
		id := create.ID()
		oldKey := testutil.Field(t, create.Data(), "video")

		update := srv.PATCHMultipart("/stream_video_docs/"+id, nil,
			map[string]testutil.FileUpload{
				"video": {Filename: "v2.mp4", ContentType: "video/mp4", Body: []byte("version two, longer")},
			})
		update.AssertStatus(http.StatusOK)
		newKey := testutil.Field(t, update.Data(), "video")
		if newKey == oldKey {
			t.Fatal("update should mint a new key")
		}

		// auto_delete default deletes the replaced blob after the successful write.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && store.HasKey(oldKey) {
			time.Sleep(20 * time.Millisecond)
		}
		if store.HasKey(oldKey) {
			t.Error("old streamed blob should be deleted on update (auto_delete default)")
		}
		if !store.HasKey(newKey) {
			t.Error("new streamed blob should be present")
		}
	})
}

func TestStreamUpload_Standalone(t *testing.T) {
	t.Parallel()

	store := newSizeSpy()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      testutil.FileModels(),
		FileStorage: store,
	})

	body := []byte("a standalone streamed upload, straight to storage")
	resp := srv.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
		"file": {Filename: "big.bin", ContentType: "application/octet-stream", Body: body},
	})
	resp.AssertStatus(http.StatusCreated)

	data := resp.Data()
	key := testutil.Field(t, data, "key")
	testutil.AssertNotEmpty(t, "key", key)
	// The response echoes the size measured while streaming.
	testutil.AssertEqual(t, "size", testutil.Field(t, data, "size"), float64(len(body)))

	// POST /files always streams now: Store saw size 0.
	if sizes := store.storeSizes(); len(sizes) != 1 || sizes[0] != 0 {
		t.Errorf("Store sizes = %v, want [0] (POST /files streams)", sizes)
	}

	served := srv.GETRaw("/files/" + key)
	served.AssertStatus(http.StatusOK)
	if string(served.Body) != string(body) {
		t.Errorf("served %q, want %q", served.Body, body)
	}
}

// TestStreamUpload_NonStreamingModelStillBuffers is the regression guard: a model
// with no upload:stream field must take the unchanged ParseMultipartForm path,
// which hands Store the real size. If the streaming parser leaked into every
// model, this size would be 0.
func TestStreamUpload_NonStreamingModelStillBuffers(t *testing.T) {
	t.Parallel()
	store := newSizeSpy()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      testutil.FileModels(),
		FileStorage: store,
	})

	resp := srv.POSTMultipart("/documents", map[string]string{"title": "Buffered"},
		map[string]testutil.FileUpload{
			"file": {Filename: "d.pdf", ContentType: "application/pdf", Body: fakePDF},
		})
	resp.AssertStatus(http.StatusCreated)

	sizes := store.storeSizes()
	if len(sizes) != 1 || sizes[0] == 0 {
		t.Errorf("Store sizes = %v, want one non-zero size (buffered path knows the length up front)", sizes)
	}
}
