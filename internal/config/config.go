package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Config contiene la configuración tipada del servicio, leída de variables
// de entorno. No hay valores hardcodeados para los límites.
//
// Las credenciales de R2 (cuenta, bucket, access key, secret) NO viven aquí:
// storage-r2 es un proxy genérico reusable por cualquier microservicio, y
// cada uno trae su propio token de R2 por request (ver internal/http,
// extractR2Credentials) en vez de que este servicio custodie un único
// "super-token" fijo para todos los consumidores.
type Config struct {
	ServerPort         string
	MaxFileSize        int64
	MaxFilesPerRequest int
}

const (
	defaultServerPort        = "8002"
	defaultMaxFileSize int64 = 10 * 1024 * 1024 // 10MB
	defaultMaxFiles          = 20
)

// Load lee la configuración del servicio. Ninguna variable es obligatoria:
// todo tiene un default razonable. Solo falla si un valor numérico presente
// es inválido.
func Load() (*Config, error) {
	cfg := &Config{
		ServerPort: envOrDefault("SERVER_PORT", defaultServerPort),
	}

	maxFileSize, err := envInt64OrDefault("MAX_FILE_SIZE", defaultMaxFileSize)
	if err != nil {
		return nil, err
	}
	maxFiles, err := envIntOrDefault("MAX_FILES_PER_REQUEST", defaultMaxFiles)
	if err != nil {
		return nil, err
	}
	if maxFileSize <= 0 || maxFiles <= 0 {
		return nil, errors.New("MAX_FILE_SIZE y MAX_FILES_PER_REQUEST deben ser mayores a cero")
	}

	cfg.MaxFileSize = maxFileSize
	cfg.MaxFilesPerRequest = maxFiles
	return cfg, nil
}

func envOrDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt64OrDefault(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s inválido: %w", key, err)
	}
	return n, nil
}

func envIntOrDefault(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s inválido: %w", key, err)
	}
	return n, nil
}
