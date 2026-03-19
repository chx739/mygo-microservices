package main

import (
	"log"
	"net/http"
	h "ride-sharing/services/trip-service/internal/infrastructure/http"
	"ride-sharing/services/trip-service/internal/infrastructure/repository"
	"ride-sharing/services/trip-service/internal/service"
)

/*
curl -X POST http://127.0.0.1:8081/trip/preview \
  -H "Content-Type: application/json" \
  -d '{
    "userID": "12",
    "destination": {
      "latitude": 37.7960377140795,
      "longitude": -122.42859007644311
    },
    "pickup": {
      "latitude": 37.783678087129466,
      "longitude": -122.40777303207217
    }
  }'
*/

func main() {
	inmemRepo := repository.NewInmemRepository()
	svc := service.NewService(inmemRepo)

	mux := http.NewServeMux()
	httphandler := h.HttpHandler{Service: svc}
	mux.HandleFunc("POST /preview", httphandler.HandleTripPreview)

	server := &http.Server{
		Addr:    ":8083",
		Handler: mux,
	}

	if err := server.ListenAndServe(); err != nil {
		log.Printf("HTTP server error: %v", err)
	}
}
