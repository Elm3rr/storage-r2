// Package storage encapsula el acceso a Cloudflare R2 vía su API compatible
// con S3. No contiene lógica de ningún dominio de negocio.
package storage

import (
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"storage-r2/internal/config"
)

// ObjectStore es el contrato mínimo que necesita la capa HTTP. Permite
// inyectar una implementación en memoria en los tests sin depender de
// credenciales de R2 reales.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader, contentType string) error
	Delete(ctx context.Context, key string) error
}

// uploadPartSize es el tamaño mínimo permitido por S3/R2 para partes de un
// multipart upload; también acota cuánta memoria usa el Uploader por archivo.
const uploadPartSize = 5 * 1024 * 1024

// R2Store implementa ObjectStore sobre el SDK de S3 apuntando a un endpoint
// de Cloudflare R2.
type R2Store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// NewR2Store inicializa el cliente S3/R2. No realiza ninguna llamada de red;
// las credenciales y el endpoint se validan de forma efectiva en el primer
// Put/Delete real.
func NewR2Store(cfg *config.Config) (*R2Store, error) {
	creds := credentials.NewStaticCredentialsProvider(
		cfg.R2AccessKeyID, cfg.R2SecretAccessKey, "",
	)

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("auto"),
		awsconfig.WithCredentialsProvider(creds),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.R2Endpoint)
		o.UsePathStyle = true
		// R2 no es totalmente compatible con el cálculo/validación de
		// checksums flexibles que el SDK activa por defecto desde v1.x
		// recientes; forzarlo a "when required" evita fallos de firma/
		// checksum en PutObject y multipart upload contra R2.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = uploadPartSize
		u.Concurrency = 1 // acota memoria: nunca más de un part en buffer
	})

	return &R2Store{client: client, uploader: uploader, bucket: cfg.R2Bucket}, nil
}

var _ ObjectStore = (*R2Store)(nil)

// Put sube body (un io.Reader no necesariamente buscable, como un
// multipart.Part) bajo key. El manager.Uploader decide internamente si hace
// un PutObject simple o un multipart upload real, sin requerir el tamaño
// total por adelantado ni cargar el archivo completo en memoria.
func (s *R2Store) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	return err
}

// Delete elimina el objeto bajo key.
func (s *R2Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}
