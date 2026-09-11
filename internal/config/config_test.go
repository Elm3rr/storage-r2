package config

import "testing"

func TestLoad_AllPresent(t *testing.T) {
	t.Setenv("SERVER_PORT", "9090")
	t.Setenv("MAX_FILE_SIZE", "123456")
	t.Setenv("MAX_FILES_PER_REQUEST", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServerPort != "9090" {
		t.Errorf("ServerPort = %q, want 9090", cfg.ServerPort)
	}
	if cfg.MaxFileSize != 123456 {
		t.Errorf("MaxFileSize = %d, want 123456", cfg.MaxFileSize)
	}
	if cfg.MaxFilesPerRequest != 5 {
		t.Errorf("MaxFilesPerRequest = %d, want 5", cfg.MaxFilesPerRequest)
	}
}

func TestLoad_InvalidMaxFileSize(t *testing.T) {
	t.Setenv("MAX_FILE_SIZE", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid MAX_FILE_SIZE, got nil")
	}
}

func TestLoad_InvalidMaxFiles(t *testing.T) {
	t.Setenv("MAX_FILES_PER_REQUEST", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid MAX_FILES_PER_REQUEST, got nil")
	}
}

func TestLoad_ZeroLimitsRejected(t *testing.T) {
	t.Setenv("MAX_FILE_SIZE", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for zero MAX_FILE_SIZE, got nil")
	}
}

// Ninguna variable es obligatoria: storage-r2 ya no tiene una cuenta/bucket
// de R2 fija que validar al arrancar (viaja por request, ver internal/http).
func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServerPort != defaultServerPort {
		t.Errorf("ServerPort = %q, want default %q", cfg.ServerPort, defaultServerPort)
	}
	if cfg.MaxFileSize != defaultMaxFileSize {
		t.Errorf("MaxFileSize = %d, want default %d", cfg.MaxFileSize, defaultMaxFileSize)
	}
	if cfg.MaxFilesPerRequest != defaultMaxFiles {
		t.Errorf("MaxFilesPerRequest = %d, want default %d", cfg.MaxFilesPerRequest, defaultMaxFiles)
	}
}
