package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
)

// apiClient issues in-process HTTP requests against the maniflex server's own
// generated REST API. Going through the real handler means auth, validation,
// the six-step pipeline, filtering, and soft-delete all apply unchanged — the
// admin panel is just another API client, never a pipeline bypass.
type apiClient struct {
	handler   http.Handler // the server's API handler (server.Handler())
	apiPrefix string       // the server's route prefix, e.g. "/api"
}

// listPage is the decoded result of a list request.
type listPage struct {
	Items []map[string]any
	Total int64
	Page  int
	Limit int
	Pages int64
}

// fieldError is one field-level validation failure surfaced by the API.
type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// apiError is a decoded non-2xx API response. It is returned as an error so
// callers can distinguish a validation failure (Fields populated) from a
// generic failure and re-render a form with inline messages.
type apiError struct {
	Status  int
	Code    string
	Message string
	Fields  []fieldError
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("API error (HTTP %d)", e.Status)
}

// envelope mirrors the maniflex JSON response envelope. Data is kept raw so it can
// be decoded as either an object (read/create/update) or an array (list).
type envelope struct {
	Data json.RawMessage `json:"data"`
	Meta *struct {
		Total int64 `json:"total"`
		Page  int   `json:"page"`
		Limit int   `json:"limit"`
		Pages int64 `json:"pages"`
	} `json:"meta"`
	Error *struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
}

// do issues one in-process request and decodes the envelope. headers from src,
// when non-nil, are copied so the caller's identity (cookies, Authorization)
// carries through to the API's auth step.
func (c *apiClient) do(src *http.Request, method, target, contentType string, body io.Reader) (*envelope, int, error) {
	req := httptest.NewRequest(method, target, body)
	if src != nil {
		req.Header = src.Header.Clone()
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusNoContent {
		return &envelope{}, rec.Code, nil
	}

	var env envelope
	if len(bytes.TrimSpace(rec.Body.Bytes())) > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			return nil, rec.Code, fmt.Errorf("decoding response (HTTP %d): %w", rec.Code, err)
		}
	}
	if rec.Code >= 400 {
		ae := &apiError{Status: rec.Code}
		if env.Error != nil {
			ae.Code = env.Error.Code
			ae.Message = env.Error.Message
			_ = json.Unmarshal(env.Error.Details, &ae.Fields)
		} else {
			ae.Message = fmt.Sprintf("HTTP %d", rec.Code)
		}
		return &env, rec.Code, ae
	}
	return &env, rec.Code, nil
}

// list fetches a page of records for table. rawQuery is an already-encoded
// query string (page/limit/sort/filter/include).
func (c *apiClient) list(src *http.Request, table, rawQuery string) (*listPage, error) {
	target := c.apiPrefix + "/" + table
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	env, _, err := c.do(src, http.MethodGet, target, "", nil)
	if err != nil {
		return nil, err
	}

	lp := &listPage{}
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &lp.Items); err != nil {
			return nil, fmt.Errorf("decoding %s list: %w", table, err)
		}
	}
	if env.Meta != nil {
		lp.Total = env.Meta.Total
		lp.Page = env.Meta.Page
		lp.Limit = env.Meta.Limit
		lp.Pages = env.Meta.Pages
	}
	return lp, nil
}

// get fetches a single record by id.
func (c *apiClient) get(src *http.Request, table, id, rawQuery string) (map[string]any, error) {
	target := c.apiPrefix + "/" + table + "/" + id
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	env, _, err := c.do(src, http.MethodGet, target, "", nil)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(env.Data, &rec); err != nil {
		return nil, fmt.Errorf("decoding %s record: %w", table, err)
	}
	return rec, nil
}

// create issues a POST. body is a JSON-keyed field map.
func (c *apiClient) create(src *http.Request, table string, body map[string]any) (map[string]any, error) {
	return c.write(src, http.MethodPost, c.apiPrefix+"/"+table, body)
}

// update issues a PATCH for one record.
func (c *apiClient) update(src *http.Request, table, id string, body map[string]any) (map[string]any, error) {
	return c.write(src, http.MethodPatch, c.apiPrefix+"/"+table+"/"+id, body)
}

// write encodes body as JSON and issues a state-changing request.
func (c *apiClient) write(src *http.Request, method, target string, body map[string]any) (map[string]any, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	env, _, err := c.do(src, method, target, "application/json", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	_ = json.Unmarshal(env.Data, &rec)
	return rec, nil
}

// writeMultipart encodes body and files as multipart/form-data. It is used for
// models with mfx:"file" fields, whose API accepts multipart create/update.
func (c *apiClient) writeMultipart(src *http.Request, method, target string, body map[string]string, files map[string]*uploadedFile) (map[string]any, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range body {
		_ = mw.WriteField(k, v)
	}
	for field, f := range files {
		if f == nil {
			continue
		}
		part, err := mw.CreateFormFile(field, f.Filename)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	env, _, err := c.do(src, method, target, mw.FormDataContentType(), &buf)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	_ = json.Unmarshal(env.Data, &rec)
	return rec, nil
}

// delete issues a DELETE for one record.
func (c *apiClient) delete(src *http.Request, table, id string) error {
	target := c.apiPrefix + "/" + table + "/" + id
	_, _, err := c.do(src, http.MethodDelete, target, "", nil)
	return err
}

// uploadedFile is a fully-buffered file part collected from a panel form.
type uploadedFile struct {
	Filename string
	Data     []byte
}
