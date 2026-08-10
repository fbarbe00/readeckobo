package webserver

import (
	"fmt"
	"net/http"

	"readeckobo/internal/app"
	"readeckobo/internal/logger"
)

// ListenAndServe starts the HTTP server on the specified port.
func ListenAndServe(port int, application *app.App, logger *logger.Logger) {
	addr := fmt.Sprintf(":%d", port)
	logger.Infof("Web server starting on port %s", addr)

	mux := http.NewServeMux()

	// Register handlers
	mux.HandleFunc("/api/kobo/get", application.HandleKoboGet)
	mux.HandleFunc("/api/kobo/download", application.HandleKoboDownload)
	mux.HandleFunc("/api/kobo/send", application.HandleKoboSend)
	mux.HandleFunc("/api/convert-image", application.HandleConvertImage)

	// Proxy routes
	mux.HandleFunc("/instapaper-proxy/storeapi/v1/initialization", application.HandleStoreAPIInitialization)
	mux.HandleFunc("/instapaper-proxy/storeapi", application.HandleStoreAPIProxy)
	mux.HandleFunc("/instapaper-proxy/storeapi/", application.HandleStoreAPIProxy)
	mux.HandleFunc("/instapaper-proxy/instapaper", application.HandleInstapaperProxy)
	mux.HandleFunc("/instapaper-proxy/instapaper/", application.HandleInstapaperProxy)

	// Optionally preserve book syncing by forwarding every other request.
	mux.HandleFunc("/", application.HandleFallbackProxy)

	// Apply logging middleware
	loggedMux := LoggingMiddleware(mux)

	if err := http.ListenAndServe(addr, loggedMux); err != nil {
		logger.Errorf("Web server failed to start: %v", err)
	}
}
