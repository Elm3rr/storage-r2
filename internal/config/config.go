package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Config contiene la configuración tipada del servicio, leída de variables
// de entorno. No hay valores hardcodeados para los límites ni para las
// credenciales de R2.
type Config struct {
	R2Endpoint         string
	R2Bucket           string
	R2AccessKeyID      string
	R2SecretAccessKey  string
	ServerPort         string
	MaxFileSize        int64
	MaxFilesPerRequest int
}

const (
	defaultServerPort        = "8002"
	defaultMaxFileSize int64 = 10 * 1024 * 1024 // 10MB
	defaultMaxFiles          = 20
)

// Load lee y valida la configuración obligatoria, fallando rápido si falta
// alguna variable requerida o si algún valor numérico es inválido.
func Load() (*Config, error) {
	var missing []string
	req := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}

	cfg := &Config{
		R2Endpoint:        req("R2_ENDPOINT"),
		R2Bucket:          req("R2_BUCKET"),
		R2AccessKeyID:     req("R2_ACCESS_KEY_ID"),
		R2SecretAccessKey: req("R2_SECRET_ACCESS_KEY"),
		ServerPort:        envOrDefault("SERVER_PORT", defaultServerPort),
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("faltan variables de entorno obligatorias: %v", missing)
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
