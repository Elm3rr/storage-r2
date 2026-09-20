package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"storage-r2/internal/config"
	"storage-r2/internal/storage"
)

type putCall struct {
	key         string
	contentType string
	data        []byte
}

type fakeStore struct {
	mu         sync.Mutex
	puts       []putCall
	deletes    []string
	failPutOn  int // índice (1-based) del Put que debe fallar; 0 = nunca falla
	failDelete bool
	putCalls   int
}

var _ storage.ObjectStore = (*fakeStore)(nil)

func (f *fakeStore) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	f.mu.Lock()
	f.putCalls++
	call := f.putCalls
	f.mu.Unlock()

	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	if f.failPutOn != 0 && call == f.failPutOn {
		return errors.New("simulated put failure")
	}

	f.mu.Lock()
	f.puts = append(f.puts, putCall{key: key, contentType: contentType, data: data})
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, key)
	if f.failDelete {
		return errors.New("simulated delete failure")
	}
	return nil
}

func newTestHandler(store storage.ObjectStore, maxSize int64, maxFiles int) (*Handler, *bytes.Buffer) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	cfg := &config.Config{MaxFileSize: maxSize, MaxFilesPerRequest: maxFiles}
	return NewHandler(store, cfg, logger), &logBuf
}

func buildMultipartBody(t *testing.T, folder string, files map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	if folder != "" || folder == "" { // folder siempre se escribe primero, incluso si es ""
		if err := w.WriteField("folder", folder); err != nil {
			t.Fatalf("WriteField folder: %v", err)
		}
	}
	for name, content := range files {
		fw, err := w.CreateFormFile("file", name)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("write file content: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, w.FormDataContentType()
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v (body=%s)", err, rec.Body.String())
	}
	return resp.Error.Code, resp.Error.Message
}

func TestHealthCheck(t *testing.T) {
	store := &failingIfCalledStore{t: t}
	h, _ := newTestHandler(store, 1<<20, 20)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.HealthCheck(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
}

// failingIfCalledStore falla la prueba si se le llama: HealthCheck no debe
// tocar el storage bajo ninguna circunstancia.
type failingIfCalledStore struct{ t *testing.T }

func (f *failingIfCalledStore) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	f.t.Fatal("Put no debería llamarse desde HealthCheck")
	return nil
}
func (f *failingIfCalledStore) Delete(ctx context.Context, key string) error {
	f.t.Fatal("Delete no debería llamarse desde HealthCheck")
	return nil
}

func TestUpload_InvalidContentType(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	req := httptest.NewRequest(http.MethodPost, "/objects", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.UploadObjects(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	if code, _ := decodeError(t, rec); code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("code = %q, want UNSUPPORTED_MEDIA_TYPE", code)
	}
}

func TestUpload_InvalidFolder(t *testing.T) {
	cases := []string{"../../etc", "/absolute/path", ""}
	for _, folder := range cases {
		t.Run(folder, func(t *testing.T) {
			store := &fakeStore{}
			h, _ := newTestHandler(store, 1<<20, 20)

			body, contentType := buildMultipartBody(t, folder, map[string]string{"a.txt": "hello"})
			req := httptest.NewRequest(http.MethodPost, "/objects", body)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			h.UploadObjects(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (folder=%q)", rec.Code, folder)
			}
			if len(store.puts) != 0 || len(store.deletes) != 0 {
				t.Errorf("no debería haber puts/deletes para folder inválido")
			}
		})
	}
}

func TestUpload_FileTooLarge(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 10, 20) // 10 bytes máximo

	body, contentType := buildMultipartBody(t, "habitaciones/1/galeria", map[string]string{
		"foto.webp": strings.Repeat("x", 100),
	})
	req := httptest.NewRequest(http.MethodPost, "/objects", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.UploadObjects(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if code, _ := decodeError(t, rec); code != "FILE_TOO_LARGE" {
		t.Errorf("code = %q, want FILE_TOO_LARGE", code)
	}
}

func TestUpload_TooManyFiles(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 2) // máximo 2 archivos

	body, contentType := buildMultipartBody(t, "habitaciones/1/galeria", map[string]string{
		"a.txt": "1", "b.txt": "2", "c.txt": "3",
	})
	req := httptest.NewRequest(http.MethodPost, "/objects", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.UploadObjects(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(store.deletes) != 2 {
		t.Errorf("deletes = %d, want 2 (rollback de los 2 ya subidos)", len(store.deletes))
	}
}

var keyPattern = regexp.MustCompile(`^habitaciones/1/galeria/[0-9a-f-]{36}\.\w+$`)

func TestUpload_SingleFileSuccess(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	body, contentType := buildMultipartBody(t, "habitaciones/1/galeria", map[string]string{
		"foto.webp": "contenido-de-prueba",
	})
	req := httptest.NewRequest(http.MethodPost, "/objects", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.UploadObjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Keys) != 1 {
		t.Fatalf("keys = %v, want 1 elemento", resp.Keys)
	}
	if !keyPattern.MatchString(resp.Keys[0]) {
		t.Errorf("key %q no matchea el patrón esperado", resp.Keys[0])
	}
	if len(store.puts) != 1 || string(store.puts[0].data) != "contenido-de-prueba" {
		t.Errorf("puts inesperado: %+v", store.puts)
	}
}

func TestUpload_MultiFileSuccess(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	body, contentType := buildMultipartBody(t, "habitaciones/1/galeria", map[string]string{
		"a.webp": "1", "b.webp": "2", "c.webp": "3",
	})
	req := httptest.NewRequest(http.MethodPost, "/objects", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.UploadObjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(store.puts) != 3 {
		t.Errorf("puts = %d, want 3", len(store.puts))
	}
	if len(store.deletes) != 0 {
		t.Errorf("deletes = %d, want 0", len(store.deletes))
	}
}

func TestUpload_PartialFailureRollback(t *testing.T) {
	store := &fakeStore{failPutOn: 2}
	h, _ := newTestHandler(store, 1<<20, 20)

	body, contentType := buildMultipartBody(t, "habitaciones/1/galeria", map[string]string{
		"a.webp": "1", "b.webp": "2", "c.webp": "3",
	})
	req := httptest.NewRequest(http.MethodPost, "/objects", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.UploadObjects(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code, _ := decodeError(t, rec); code != "STORAGE_ERROR" {
		t.Errorf("code = %q, want STORAGE_ERROR", code)
	}
	if len(store.puts) != 1 {
		t.Errorf("puts exitosos = %d, want 1 (solo el primero antes del fallo)", len(store.puts))
	}
	if len(store.deletes) != 1 {
		t.Errorf("deletes de rollback = %d, want 1", len(store.deletes))
	}
	if store.putCalls != 2 {
		t.Errorf("putCalls = %d, want 2 (el tercer archivo no debe intentarse tras el abort)", store.putCalls)
	}
}

func TestUpload_RollbackDeleteAlsoFails(t *testing.T) {
	store := &fakeStore{failPutOn: 2, failDelete: true}
	h, logBuf := newTestHandler(store, 1<<20, 20)

	body, contentType := buildMultipartBody(t, "habitaciones/1/galeria", map[string]string{
		"a.webp": "1", "b.webp": "2",
	})
	req := httptest.NewRequest(http.MethodPost, "/objects", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()

	// No debe entrar en pánico aunque el propio rollback falle.
	h.UploadObjects(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logBuf.String(), "no se pudo eliminar objeto huérfano") {
		t.Errorf("se esperaba log de fallo de rollback, log=%s", logBuf.String())
	}
}

func TestDelete_Single(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	reqBody, _ := json.Marshal(map[string]any{"keys": []string{"habitaciones/1/galeria/a.webp"}})
	req := httptest.NewRequest(http.MethodDelete, "/objects", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.DeleteObjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(store.deletes) != 1 || store.deletes[0] != "habitaciones/1/galeria/a.webp" {
		t.Errorf("deletes inesperado: %v", store.deletes)
	}
}

func TestDelete_Multiple(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	keys := []string{"a.webp", "b.webp", "c.webp"}
	reqBody, _ := json.Marshal(map[string]any{"keys": keys})
	req := httptest.NewRequest(http.MethodDelete, "/objects", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.DeleteObjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(store.deletes) != 3 {
		t.Errorf("deletes = %d, want 3", len(store.deletes))
	}
}

func TestDelete_InvalidBody(t *testing.T) {
	cases := map[string]string{
		"malformed": `{not-json`,
		"empty":     `{"keys":[]}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			h, _ := newTestHandler(store, 1<<20, 20)

			req := httptest.NewRequest(http.MethodDelete, "/objects", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.DeleteObjects(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestValidateFolder(t *testing.T) {
	valid := []string{"habitaciones/123/galeria", "a", "a/b/c"}
	for _, f := range valid {
		if _, err := validateFolder(f); err != nil {
			t.Errorf("validateFolder(%q) = %v, want nil", f, err)
		}
	}

	invalid := []string{"", "/abs", "../escape", "a/../b", "a/./b", strings.Repeat("a", 600)}
	for _, f := range invalid {
		if _, err := validateFolder(f); err == nil {
			t.Errorf("validateFolder(%q) = nil, want error", f)
		}
	}
}

func TestSafeExtension(t *testing.T) {
	cases := map[string]string{
		"foto.webp":     ".webp",
		"documento.PDF": ".pdf",
		"sinextension":  "",
		"raro.tar.gz":   ".gz",
		"malicioso.":    "",
	}
	for filename, want := range cases {
		if got := safeExtension(filename); got != want {
			t.Errorf("safeExtension(%q) = %q, want %q", filename, got, want)
		}
	}
}

// multipartPart describe un campo del multipart en el orden exacto en que se
// escribe, para poder probar contratos sensibles al orden (`folder`, `name`
// y `file`).
type multipartPart struct {
	field    string // nombre del campo (folder, name, file)
	filename string // solo para file
	content  string
}

func buildOrderedMultipart(t *testing.T, parts []multipartPart) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	for _, p := range parts {
		if p.filename != "" {
			fw, err := w.CreateFormFile(p.field, p.filename)
			if err != nil {
				t.Fatalf("CreateFormFile: %v", err)
			}
			if _, err := fw.Write([]byte(p.content)); err != nil {
				t.Fatalf("write file content: %v", err)
			}
			continue
		}
		if err := w.WriteField(p.field, p.content); err != nil {
			t.Fatalf("WriteField %s: %v", p.field, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, w.FormDataContentType()
}

func postUpload(h *Handler, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/objects", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.UploadObjects(rec, req)
	return rec
}

func TestUpload_NameProducesDeterministicKey(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	body, ct := buildOrderedMultipart(t, []multipartPart{
		{field: "folder", content: "habitaciones/pisos"},
		{field: "name", content: "0b8f1c1e-7f0a-4c39-9d54-2a3b5c6d7e8f"},
		{field: "file", filename: "croquis.SVG", content: "<svg/>"},
	})
	rec := postUpload(h, body, ct)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := "habitaciones/pisos/0b8f1c1e-7f0a-4c39-9d54-2a3b5c6d7e8f.svg"
	if len(resp.Keys) != 1 || resp.Keys[0] != want {
		t.Fatalf("keys = %v, want [%s]", resp.Keys, want)
	}
	if len(store.puts) != 1 || store.puts[0].key != want {
		t.Errorf("puts inesperado: %+v", store.puts)
	}
}

func TestUpload_NameOverwritesSameKeyOnResubmit(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	for _, content := range []string{"<svg>v1</svg>", "<svg>v2</svg>"} {
		body, ct := buildOrderedMultipart(t, []multipartPart{
			{field: "folder", content: "habitaciones/pisos"},
			{field: "name", content: "piso-1"},
			{field: "file", filename: "croquis.svg", content: content},
		})
		if rec := postUpload(h, body, ct); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	}
	if len(store.puts) != 2 || store.puts[0].key != store.puts[1].key {
		t.Fatalf("se esperaban 2 Put sobre la misma key, got %+v", store.puts)
	}
	if len(store.deletes) != 0 {
		t.Errorf("no debe borrar nada al reemplazar, deletes=%v", store.deletes)
	}
}

func TestUpload_InvalidName(t *testing.T) {
	for _, name := range []string{"", "a/b", "../x", "con.punto", "con espacio", strings.Repeat("a", maxNameLength+1)} {
		store := &fakeStore{}
		h, _ := newTestHandler(store, 1<<20, 20)

		body, ct := buildOrderedMultipart(t, []multipartPart{
			{field: "folder", content: "habitaciones/pisos"},
			{field: "name", content: name},
			{field: "file", filename: "croquis.svg", content: "<svg/>"},
		})
		rec := postUpload(h, body, ct)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("name=%q: status = %d, want 400", name, rec.Code)
		}
		if code, _ := decodeError(t, rec); code != "INVALID_REQUEST" {
			t.Errorf("name=%q: code = %q, want INVALID_REQUEST", name, code)
		}
		if len(store.puts) != 0 {
			t.Errorf("name=%q: no debe subir nada, puts=%+v", name, store.puts)
		}
	}
}

func TestUpload_NameAfterFileRejected(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	body, ct := buildOrderedMultipart(t, []multipartPart{
		{field: "folder", content: "habitaciones/pisos"},
		{field: "file", filename: "croquis.svg", content: "<svg/>"},
		{field: "name", content: "piso-1"},
	})
	rec := postUpload(h, body, ct)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpload_NameWithMultipleFilesRejectedWithoutRollback(t *testing.T) {
	store := &fakeStore{}
	h, _ := newTestHandler(store, 1<<20, 20)

	body, ct := buildOrderedMultipart(t, []multipartPart{
		{field: "folder", content: "habitaciones/pisos"},
		{field: "name", content: "piso-1"},
		{field: "file", filename: "a.svg", content: "<svg/>"},
		{field: "file", filename: "b.svg", content: "<svg/>"},
	})
	rec := postUpload(h, body, ct)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	// La key es determinista y pudo reemplazar a un objeto previo: no se borra.
	if len(store.deletes) != 0 {
		t.Errorf("no debe hacer rollback con name, deletes=%v", store.deletes)
	}
}
