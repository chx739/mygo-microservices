package main

import (
	"bytes"
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

	jsonBody, _ := json.Marshal(reqBody)
	reader := bytes.NewReader(jsonBody)

	// grpc connection
	tripService, err := grpc_clients.NewTripServiceClient()
	if err != nil {
		log.Print(err)
		http.Error(w, "failed to connect to trip service", http.StatusInternalServerError)
		return
	}
	defer tripService.Close()

	// TODO: Call Trip Service
	resp, err := http.Post("http://trip-service:8083/preview", "application/json", reader)
	if err != nil {
		log.Print(err)
		return
	}

	defer resp.Body.Close()

	var respBody any
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		http.Error(w, "failed to parse JSON data from trip service", http.StatusBadRequest)
		return
	}

	response := contracts.APIResponse{Data: respBody}
	writeJSON(w, http.StatusCreated, response)
}
