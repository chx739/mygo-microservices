package main

import (
	"encoding/json"
	"log"
	"net/http"
	"ride-sharing/services/api-gateway/grpc_clients"
	"ride-sharing/shared/contracts"
	"time"
)

func handleTripPreview(w http.ResponseWriter, r *http.Request) {
	time.Sleep(time.Second * 9)
	var reqBody previewTripRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		http.Error(w, "Failed to parse JSON data", http.StatusBadRequest)
		return
	}

	defer r.Body.Close()

	// validation
	if reqBody.UserID == "" {
		http.Error(w, "userID is required", http.StatusBadRequest)
		return
	}

	// grpc connection
	tripService, err := grpc_clients.NewTripServiceClient()
	if err != nil {
		log.Print(err)
		http.Error(w, "failed to connect to trip service", http.StatusInternalServerError)
		return
	}
	defer tripService.Close()

	// TODO: Call Trip Service
	tripPreview, err := tripService.Client.PreviewTrip(r.Context(), reqBody.ToProto())
	if err != nil {
		log.Printf("Failed to preview a trip: %v", err)
		http.Error(w, "Failed to preview a trip", http.StatusInternalServerError)
		return
	}

	respBody := tripPreview.GetRoute()

	response := contracts.APIResponse{Data: respBody}
	writeJSON(w, http.StatusCreated, response)
}
