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

`storage-r2` **no custodia ninguna credencial de R2 de forma permanente**. No tiene una cuenta/bucket fijo configurado al arrancar — cada request a `POST /objects` o `DELETE /objects` trae su propio token de Cloudflare (ver "Credenciales de R2 por request" más abajo). Esto es intencional: el servicio es un proxy genérico, reusable por cualquier microservicio (de este monorepo o de otro proyecto) sin atarlo a una cuenta de R2 en particular, y permite que cada consumidor use un token acotado (least-privilege) a su propio bucket/prefijo en vez de que un único "super-token" tenga acceso a todo.

Estos valores nunca se loguean (ni siquiera en los logs de error) ni se exponen en ninguna respuesta — viajan solo por header, en la red interna, para esa request puntual.

## Credenciales de R2 por request

`POST /objects` y `DELETE /objects` (no `GET /health`) requieren estos 4 headers en cada llamada:

| Header | Descripción |
|---|---|
| `X-R2-Account-Id` | Account ID de Cloudflare. `storage-r2` arma el endpoint S3-compatible como `https://{account_id}.r2.cloudflarestorage.com` — no se manda la URL completa. |
| `X-R2-Bucket` | Bucket destino de esta request. |
| `X-R2-Access-Key-Id` | Access key del token de R2 (permanente, sin expiración). |
| `X-R2-Secret-Access-Key` | Secret del token de R2. |

Si falta alguno, la respuesta es `400 INVALID_REQUEST`. Cada microservicio consumidor guarda su propio token (idealmente generado con permisos acotados a su propio prefijo/bucket vía el dashboard de Cloudflare) en su propia configuración, y lo manda en cada llamada — `storage-r2` no lo recuerda entre requests.

Solo se soportan tokens permanentes (access key + secret); no hay soporte para credenciales temporales de Cloudflare con `session token`.

## Variables de entorno

| Variable | Obligatoria | Default | Descripción |
|---|---|---|---|
| `SERVER_PORT` | no | `8002` | Puerto HTTP del servicio |
| `MAX_FILE_SIZE` | no | `10485760` (10MB) | Tamaño máximo por archivo, en bytes |
| `MAX_FILES_PER_REQUEST` | no | `20` | Máximo de archivos por request de subida |

Ninguna es obligatoria (todas tienen default) — a diferencia de las credenciales de R2, que no son configuración del servicio sino que viajan por request (ver arriba). Ver `.env.example`.

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
  -H "X-R2-Account-Id: <account_id>" \
  -H "X-R2-Bucket: hoteleria-storage" \
  -H "X-R2-Access-Key-Id: <access_key>" \
  -H "X-R2-Secret-Access-Key: <secret_key>" \
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

Por defecto el nombre final de cada objeto es `{folder}/{uuid}.{extension}` — el cliente controla el prefijo lógico, no el nombre.

**Nombre explícito (opcional):** para recursos que necesitan una key determinista (p. ej. un único croquis por piso que se reemplaza al re-subir), se puede enviar el campo `name` **después de `folder` y antes de `file`**. La key queda `{folder}/{name}.{extension}` y, si ya existía, se sobrescribe. `name` debe cumplir `^[A-Za-z0-9_-]{1,128}$` (sin `/`, `.` ni espacios; la extensión sale del archivo) y solo se admite con **un** archivo por request; de lo contrario responde `400 INVALID_REQUEST`.

```bash
curl -X POST http://localhost:8002/objects \
  -H "X-R2-Account-Id: <account_id>" -H "X-R2-Bucket: <bucket>" \
  -H "X-R2-Access-Key-Id: <key>" -H "X-R2-Secret-Access-Key: <secret>" \
  -F folder=habitaciones/pisos \
  -F name=550e8400-e29b-41d4-a716-446655440000 \
  -F file=@croquis.svg
# => {"keys":["habitaciones/pisos/550e8400-e29b-41d4-a716-446655440000.svg"]}
```

`storage-r2` no valida qué extensiones son válidas para tu dominio (eso lo decide el microservicio consumidor antes de llamar); solo aplica controles técnicos globales (tamaño, cantidad, caracteres válidos en `folder`).

**Regla todo-o-nada:** si un archivo falla a mitad de una carga múltiple, `storage-r2` elimina (rollback) los objetos ya subidos en esa misma request y responde con error. Nunca deja objetos huérfanos de una request fallida.

### `DELETE /objects`

Elimina una o varias keys.

```bash
curl -X DELETE http://localhost:8002/objects \
  -H "Content-Type: application/json" \
  -H "X-R2-Account-Id: <account_id>" \
  -H "X-R2-Bucket: hoteleria-storage" \
  -H "X-R2-Access-Key-Id: <access_key>" \
  -H "X-R2-Secret-Access-Key: <secret_key>" \
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
3. Tener su propio token de R2 (account id, bucket, access key, secret) en su propia configuración/env — idealmente un token generado en Cloudflare con permisos acotados a su propio prefijo, no un token con acceso a todo el bucket. `storage-r2` no lo provee ni lo recuerda.
4. Elegir su propio prefijo lógico (`folder`), por ejemplo:
   - `habitaciones/{habitacionId}/galeria`
   - `usuarios/{usuarioId}/perfil`
   - `proveedores/{proveedorId}/documentos`
5. Llamar a `POST /objects` con ese `folder`, los archivos, y sus 4 headers de credenciales (ver "Credenciales de R2 por request").
6. Guardar las `keys` devueltas en su propia base de datos — **no** la URL pública completa, para poder cambiar el dominio público sin tocar todas las bases de datos consumidoras.
7. Llamar a `DELETE /objects` (con sus mismas credenciales) con las keys correspondientes cuando el recurso de negocio se elimine.

Ejemplo: `habitaciones` expondría sus propios endpoints de dominio (`POST /habitaciones/{id}/galeria`, etc.) que internamente validan la habitación y los permisos, y luego llaman a `storage-r2` con `folder=habitaciones/{id}/galeria`.

`storage-r2` no registra estas secciones como catálogo ni las conoce de ninguna forma — son responsabilidad exclusiva de cada consumidor documentarlas.

## Fuera de alcance de esta primera versión

- Presigned URLs y endpoint de descarga (el bucket es público; los consumidores acceden directamente vía la URL pública construida a partir de la `key`).
- Integración en el `docker-compose.yml` raíz del monorepo y en la configuración de Traefik: se deja pendiente a criterio del equipo, siguiendo el mismo criterio ya aplicado con el microservicio `habitaciones`. El `Dockerfile` de este servicio sí está incluido y listo para usarse.

## Desarrollo local

```bash
cp .env.example .env   # ajustar SERVER_PORT/límites si hace falta — no hay credenciales de R2 que completar aquí
go run ./cmd/server
```

Para probar `POST`/`DELETE /objects` contra R2 real, se necesita un token de Cloudflare a mano (ver "Credenciales de R2 por request") — no se configura en `.env`, se manda por header en cada request.

## Tests

```bash
go test ./...
```

Los tests de la capa HTTP usan un `ObjectStore` en memoria (sin credenciales ni red) para cubrir health check, validaciones de request/folder/archivo, upload simple y múltiple, rollback ante fallo parcial (incluyendo el caso donde el propio rollback también falla) y delete simple/múltiple.
