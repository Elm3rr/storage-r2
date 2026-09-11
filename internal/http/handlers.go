// Package http implementa los 3 endpoints HTTP del servicio: parseo y
// validación técnica de requests, coordinación de la carga múltiple con
// rollback todo-o-nada, y borrado de objetos. No conoce ningún dominio de
// negocio (habitaciones, usuarios, etc.).
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"storage-r2/internal/config"
	"storage-r2/internal/storage"
)

const maxFolderLength = 512

// StoreFactory construye un ObjectStore a partir de las credenciales de R2
// que trajo una request puntual. storage-r2 no tiene una única cuenta/bucket
// fija: cada request arma su propio cliente, así puede ser reusado por
// cualquier microservicio consumidor con su propio token de Cloudflare.
type StoreFactory func(storage.R2Credentials) (storage.ObjectStore, error)

// Handler agrupa las dependencias de los 3 endpoints.
type Handler struct {
	storeFactory StoreFactory
	maxSize      int64
	maxFiles     int
	logger       *slog.Logger
}

// NewHandler construye un Handler listo para registrarse en un http.ServeMux.
func NewHandler(storeFactory StoreFactory, cfg *config.Config, logger *slog.Logger) *Handler {
	return &Handler{
		storeFactory: storeFactory,
		maxSize:      cfg.MaxFileSize,
		maxFiles:     cfg.MaxFilesPerRequest,
		logger:       logger,
	}
}

// extractR2Credentials lee las credenciales de R2 que debe traer CADA
// request a /objects, vía headers. Nunca se deben loguear estos valores
// (ni siquiera el error de validación los incluye) — son secretos de vida
// corta que solo importan para esta request puntual.
func extractR2Credentials(r *http.Request) (storage.R2Credentials, *apiError) {
	creds := storage.R2Credentials{
		AccountID:       r.Header.Get("X-R2-Account-Id"),
		Bucket:          r.Header.Get("X-R2-Bucket"),
		AccessKeyID:     r.Header.Get("X-R2-Access-Key-Id"),
		SecretAccessKey: r.Header.Get("X-R2-Secret-Access-Key"),
	}
	if creds.AccountID == "" || creds.Bucket == "" || creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return storage.R2Credentials{}, &apiError{
			http.StatusBadRequest, "INVALID_REQUEST",
			"faltan credenciales de R2 en los headers (X-R2-Account-Id, X-R2-Bucket, X-R2-Access-Key-Id, X-R2-Secret-Access-Key)",
			nil,
		}
	}
	return creds, nil
}

// apiError representa un error de respuesta HTTP. cause solo se usa para
// logging interno y nunca se serializa al cliente.
type apiError struct {
	Status  int
	Code    string
	Message string
	cause   error
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, logger *slog.Logger, err *apiError) {
	if err.cause != nil {
		logger.Error("request failed", "code", err.Code, "status", err.Status, "error", err.cause)
	} else {
		logger.Warn("request rejected", "code", err.Code, "status", err.Status, "message", err.Message)
	}
	writeJSON(w, err.Status, map[string]any{
		"error": map[string]string{"code": err.Code, "message": err.Message},
	})
}

// HealthCheck confirma que el proceso HTTP está vivo. No consulta R2 ni
// requiere credenciales — storage-r2 ya no tiene ninguna cuenta/bucket fija
// que validar al arrancar (ver extractR2Credentials).
func (h *Handler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

var errInvalidFolder = errors.New("invalid folder")

// validateFolder normaliza y valida el prefijo lógico recibido, rechazando
// rutas absolutas, segmentos ".."/"." y caracteres de control. storage-r2
// trata folder como un valor opaco: no interpreta su significado de negocio,
// solo garantiza que no permite escapar del bucket ni inyectar keys
// malformadas.
func validateFolder(raw string) (string, error) {
	if raw == "" || len(raw) > maxFolderLength {
		return "", errInvalidFolder
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7F {
			return "", errInvalidFolder
		}
	}
	normalized := strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(normalized, "/") {
		return "", errInvalidFolder
	}
	var segments []string
	for _, seg := range strings.Split(normalized, "/") {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return "", errInvalidFolder
		}
		segments = append(segments, seg)
	}
	if len(segments) == 0 {
		return "", errInvalidFolder
	}
	return strings.Join(segments, "/"), nil
}

var extPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,10}$`)

// safeExtension devuelve la extensión del archivo (con el punto) solo si es
// un patrón alfanumérico simple; en cualquier otro caso la omite en lugar de
// propagarla cruda hacia la object key.
func safeExtension(filename string) string {
	ext := strings.TrimPrefix(filepath.Ext(filename), ".")
	if extPattern.MatchString(ext) {
		return "." + strings.ToLower(ext)
	}
	return ""
}

var errFileTooLarge = errors.New("file too large")

// limitedReader corta el streaming apenas se excede el tamaño máximo
// permitido, en vez de dejar que el archivo se suba completo y fallar después.
type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return 0, errFileTooLarge
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		return n, errFileTooLarge
	}
	return n, err
}

// detectContentType hace sniffing del MIME type leyendo hasta 512 bytes y
// reconstruye un reader equivalente al original para no romper el streaming.
// Devuelve io.EOF si la parte no tiene contenido (archivo vacío).
func detectContentType(part *multipart.Part) (io.Reader, string, error) {
	declared := part.Header.Get("Content-Type")
	buf := make([]byte, 512)
	n, err := io.ReadFull(part, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	buf = buf[:n]
	rest := io.MultiReader(bytes.NewReader(buf), part)
	if n == 0 {
		return rest, "", io.EOF
	}
	if declared != "" && declared != "application/octet-stream" {
		return rest, declared, nil
	}
	return rest, http.DetectContentType(buf), nil
}

// rollback elimina los objetos ya subidos en una carga múltiple que falló a
// mitad de camino, usando el mismo store (y por tanto las mismas
// credenciales) de la request que falló. Usa un contexto propio (no el del
// request, que puede estar cancelado por el error original) con timeout
// acotado. Si un delete de rollback también falla, se registra en detalle
// pero la operación completa sigue considerándose fallida.
func (h *Handler) rollback(store storage.ObjectStore, keys []string) {
	for _, key := range keys {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := store.Delete(ctx, key); err != nil {
			h.logger.Error("rollback: no se pudo eliminar objeto huérfano", "key", key, "error", err)
		} else {
			h.logger.Warn("rollback: objeto eliminado tras fallo de carga múltiple", "key", key)
		}
		cancel()
	}
}

// UploadObjects implementa POST /objects. Usa r.MultipartReader() de bajo
// nivel (no ParseMultipartForm) para no bufferizar los archivos completos en
// memoria. Esto exige que el campo "folder" llegue antes que los campos
// "file" en el cuerpo multipart -- es el orden natural en el que cualquier
// cliente (FormData.append, curl -F) arma el body, y se documenta en el
// README como parte del contrato.
func (h *Handler) UploadObjects(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	creds, apiErr := extractR2Credentials(r)
	if apiErr != nil {
		writeError(w, h.logger, apiErr)
		return
	}
	store, err := h.storeFactory(creds)
	if err != nil {
		writeError(w, h.logger, &apiError{http.StatusInternalServerError, "STORAGE_ERROR", "no se pudo inicializar el cliente de almacenamiento", err})
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, h.logger, &apiError{http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "se esperaba multipart/form-data", err})
		return
	}

	var folder string
	folderSet := false
	var uploadedKeys []string
	fileCount := 0

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			h.rollback(store, uploadedKeys)
			writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "cuerpo multipart malformado", err})
			return
		}

		switch part.FormName() {
		case "folder":
			if part.FileName() != "" || folderSet {
				writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "campo folder inválido", nil})
				return
			}
			raw, _ := io.ReadAll(io.LimitReader(part, maxFolderLength+1))
			folder, err = validateFolder(string(raw))
			if err != nil {
				writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "folder inválido", err})
				return
			}
			folderSet = true

		case "file":
			if !folderSet {
				writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "folder debe enviarse antes que los archivos", nil})
				return
			}
			if fileCount >= h.maxFiles {
				h.rollback(store, uploadedKeys)
				writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "demasiados archivos", nil})
				return
			}
			if part.FileName() == "" {
				writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "campo file inválido", nil})
				return
			}

			body, contentType, err := detectContentType(part)
			if err == io.EOF {
				writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "archivo vacío", nil})
				return
			}
			key := folder + "/" + uuid.NewString() + safeExtension(part.FileName())
			limited := &limitedReader{r: body, n: h.maxSize}

			if err := store.Put(r.Context(), key, limited, contentType); err != nil {
				h.rollback(store, uploadedKeys)
				if errors.Is(err, errFileTooLarge) {
					writeError(w, h.logger, &apiError{http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "archivo excede el tamaño máximo permitido", err})
				} else {
					writeError(w, h.logger, &apiError{http.StatusInternalServerError, "STORAGE_ERROR", "error subiendo archivo", err})
				}
				return
			}
			uploadedKeys = append(uploadedKeys, key)
			fileCount++

		default:
			writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "campo no soportado: " + part.FormName(), nil})
			return
		}
	}

	if !folderSet {
		writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "folder es obligatorio", nil})
		return
	}
	if fileCount == 0 {
		writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "al menos un archivo es obligatorio", nil})
		return
	}

	h.logger.Info("upload completado", "files", fileCount, "duration_ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, map[string]any{"keys": uploadedKeys})
}

type deleteRequest struct {
	Keys []string `json:"keys"`
}

// DeleteObjects implementa DELETE /objects. Intenta borrar todas las keys
// recibidas; si alguna falla, responde STORAGE_ERROR aunque otras se hayan
// eliminado correctamente (R2 no ofrece una transacción multiobjeto para
// deshacer eso, y aquí no hay nada que revertir: la key que sí se borró ya
// no existe).
func (h *Handler) DeleteObjects(w http.ResponseWriter, r *http.Request) {
	creds, apiErr := extractR2Credentials(r)
	if apiErr != nil {
		writeError(w, h.logger, apiErr)
		return
	}
	store, err := h.storeFactory(creds)
	if err != nil {
		writeError(w, h.logger, &apiError{http.StatusInternalServerError, "STORAGE_ERROR", "no se pudo inicializar el cliente de almacenamiento", err})
		return
	}

	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, h.logger, &apiError{http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "se esperaba application/json", nil})
		return
	}
	var req deleteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil || len(req.Keys) == 0 {
		writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", `se esperaba {"keys": [...]} no vacío`, err})
		return
	}
	if len(req.Keys) > h.maxFiles {
		writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "demasiadas keys", nil})
		return
	}

	var failed []string
	for _, key := range req.Keys {
		if key == "" {
			writeError(w, h.logger, &apiError{http.StatusBadRequest, "INVALID_REQUEST", "key vacía", nil})
			return
		}
		if err := store.Delete(r.Context(), key); err != nil {
			h.logger.Error("delete falló", "key", key, "error", err)
			failed = append(failed, key)
		}
	}
	if len(failed) > 0 {
		writeError(w, h.logger, &apiError{http.StatusInternalServerError, "STORAGE_ERROR", "no se pudieron eliminar todas las keys", nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": req.Keys})
}
