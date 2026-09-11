package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"storage-r2/internal/config"
	httpapi "storage-r2/internal/http"
	"storage-r2/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuración inválida", "error", err)
		os.Exit(1)
	}

	// No hay un único cliente R2 que inicializar al arrancar: cada request
	// trae su propia cuenta/bucket/token (ver internal/http.extractR2Credentials),
	// así que storage.NewR2Store se pasa directo como factory por request.
	h := httpapi.NewHandler(storage.NewR2Store, cfg, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.HealthCheck)
	mux.HandleFunc("POST /objects", h.UploadObjects)
	mux.HandleFunc("DELETE /objects", h.DeleteObjects)

	srv := &http.Server{
		Addr:              ":" + cfg.ServerPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// El upload sube sincrónicamente a R2 dentro del mismo ciclo
		// request-response; estos timeouts cubren leer el multipart + subir
		// + responder, tolerando MAX_FILES_PER_REQUEST x MAX_FILE_SIZE en
		// redes lentas sin dejarlos indefinidos.
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("storage-r2 iniciado", "port", cfg.ServerPort)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil {
			logger.Error("error del servidor", "error", err)
			os.Exit(1)
		}
	case <-stop:
		logger.Info("señal de apagado recibida")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("apagado ordenado falló", "error", err)
		}
		logger.Info("servidor detenido")
	}
}
