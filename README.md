# storage-r2

Microservicio de infraestructura, extremadamente ligero, que actúa como capa HTTP genérica entre los microservicios de negocio del monorepo y **Cloudflare R2**.

`storage-r2` no conoce ningún dominio de negocio (habitaciones, usuarios, proveedores, etc.). Solo sabe subir, listar (implícitamente vía las `keys` devueltas) y eliminar objetos bajo un prefijo lógico (`folder`) que le indica el microservicio consumidor.

## Stack

- Go, `net/http` estándar (sin framework HTTP).
- `AWS SDK for Go v2`, usando la compatibilidad S3 de R2.
- Sin base de datos, sin ORM, sin JWT.

## Seguridad

`storage-r2` **no valida JWT**. La autenticación se resuelve en el borde de la infraestructura (Traefik) antes de que el tráfico llegue a los microservicios de negocio, y estos son quienes hablan con `storage-r2` por red interna. `storage-r2` debe permanecer accesible únicamente desde esa red interna.

No validar JWT no significa aceptar cualquier entrada: el servicio sí aplica validaciones técnicas sobre `folder` y los archivos (ver más abajo).

## Variables de entorno

| Variable | Obligatoria | Default | Descripción |
|---|---|---|---|
| `R2_ENDPOINT` | sí | — | Endpoint S3-compatible de la cuenta de R2 |
| `R2_BUCKET` | sí | — | Bucket destino |
| `R2_ACCESS_KEY_ID` | sí | — | Access key de R2 |
| `R2_SECRET_ACCESS_KEY` | sí | — | Secret key de R2 |
| `SERVER_PORT` | no | `8002` | Puerto HTTP del servicio |
| `MAX_FILE_SIZE` | no | `10485760` (10MB) | Tamaño máximo por archivo, en bytes |
| `MAX_FILES_PER_REQUEST` | no | `20` | Máximo de archivos por request de subida |

Si falta alguna variable obligatoria, el servicio falla al arrancar (fail-fast) en vez de arrancar en un estado inconsistente. Ver `.env.example`.

## Endpoints

### `GET /health`

No consulta R2 — solo confirma que el proceso HTTP está vivo. La configuración de R2 ya se validó al arrancar.

```bash
curl http://localhost:8002/health
# 200 {"status":"ok"}
```

### `POST /objects`

Sube uno o varios archivos bajo un mismo prefijo lógico (`folder`).

**Importante — orden del multipart:** el campo `folder` debe enviarse **antes** que los campos `file` en el cuerpo `multipart/form-data`. Es el orden natural en el que cualquier cliente arma el body (`FormData.append('folder', ...)` antes de `.append('file', ...)`, o `curl -F folder=... -F file=...`), y el servicio lo procesa como un stream de una sola pasada para no bufferizar archivos completos en memoria.

```bash
curl -X POST http://localhost:8002/objects \
  -F folder=habitaciones/123/galeria \
  -F file=@foto1.webp \
  -F file=@foto2.webp
```

Respuesta exitosa:

```json
{
  "keys": [
    "habitaciones/123/galeria/550e8400-e29b-41d4-a716-446655440000.webp",
    "habitaciones/123/galeria/8f14e45f-ea6d-4c3b-9f12-123456789abc.webp"
  ]
}
```

El nombre final de cada objeto es `{folder}/{uuid}.{extension}` — el cliente nunca controla el nombre, solo el prefijo lógico. `storage-r2` no valida qué extensiones son válidas para tu dominio (eso lo decide el microservicio consumidor antes de llamar); solo aplica controles técnicos globales (tamaño, cantidad, caracteres válidos en `folder`).

**Regla todo-o-nada:** si un archivo falla a mitad de una carga múltiple, `storage-r2` elimina (rollback) los objetos ya subidos en esa misma request y responde con error. Nunca deja objetos huérfanos de una request fallida.

### `DELETE /objects`

Elimina una o varias keys.

```bash
curl -X DELETE http://localhost:8002/objects \
  -H "Content-Type: application/json" \
  -d '{"keys": ["habitaciones/123/galeria/550e8400-e29b-41d4-a716-446655440000.webp"]}'
```

Respuesta exitosa:

```json
{ "deleted": ["habitaciones/123/galeria/550e8400-e29b-41d4-a716-446655440000.webp"] }
```

## Errores

Formato consistente:

```json
{ "error": { "code": "INVALID_REQUEST", "message": "..." } }
```

| Código | HTTP | Cuándo |
|---|---|---|
| `INVALID_REQUEST` | 400 | Falta `folder`, folder inválido (ruta absoluta, `..`, caracteres de control, vacío, demasiado largo), sin archivos, demasiados archivos, JSON malformado en delete |
| `FILE_TOO_LARGE` | 413 | Un archivo excede `MAX_FILE_SIZE` |
| `UNSUPPORTED_MEDIA_TYPE` | 415 | `Content-Type` no es el esperado por el endpoint |
| `STORAGE_ERROR` | 500 | Falla al hablar con R2 (upload o delete) |

Nunca se exponen detalles del SDK, credenciales ni el endpoint privado de R2 en la respuesta al cliente — esos detalles van a logs estructurados (`log/slog`).

## Integración desde un microservicio consumidor

`storage-r2` conoce objetos; los microservicios de negocio conocen el significado de esos objetos. Cada consumidor es responsable de:

1. Validar que la entidad de negocio exista y que el usuario tenga permiso (su propio contexto de seguridad — `storage-r2` no participa en eso).
2. Decidir sus propios tipos de archivo permitidos y límites de negocio (`storage-r2` solo aplica límites técnicos globales).
3. Elegir su propio prefijo lógico (`folder`), por ejemplo:
   - `habitaciones/{habitacionId}/galeria`
   - `usuarios/{usuarioId}/perfil`
   - `proveedores/{proveedorId}/documentos`
4. Llamar a `POST /objects` con ese `folder` y los archivos.
5. Guardar las `keys` devueltas en su propia base de datos — **no** la URL pública completa, para poder cambiar el dominio público sin tocar todas las bases de datos consumidoras.
6. Llamar a `DELETE /objects` con las keys correspondientes cuando el recurso de negocio se elimine.

Ejemplo: `habitaciones` expondría sus propios endpoints de dominio (`POST /habitaciones/{id}/galeria`, etc.) que internamente validan la habitación y los permisos, y luego llaman a `storage-r2` con `folder=habitaciones/{id}/galeria`.

`storage-r2` no registra estas secciones como catálogo ni las conoce de ninguna forma — son responsabilidad exclusiva de cada consumidor documentarlas.

## Fuera de alcance de esta primera versión

- Presigned URLs y endpoint de descarga (el bucket es público; los consumidores acceden directamente vía la URL pública construida a partir de la `key`).
- Integración en el `docker-compose.yml` raíz del monorepo y en la configuración de Traefik: se deja pendiente a criterio del equipo, siguiendo el mismo criterio ya aplicado con el microservicio `habitaciones`. El `Dockerfile` de este servicio sí está incluido y listo para usarse.

## Desarrollo local

```bash
cp .env.example .env   # completar credenciales reales de R2
go run ./cmd/server
```

## Tests

```bash
go test ./...
```

Los tests de la capa HTTP usan un `ObjectStore` en memoria (sin credenciales ni red) para cubrir health check, validaciones de request/folder/archivo, upload simple y múltiple, rollback ante fallo parcial (incluyendo el caso donde el propio rollback también falla) y delete simple/múltiple.
