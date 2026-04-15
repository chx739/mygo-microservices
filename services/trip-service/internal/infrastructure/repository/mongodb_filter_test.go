package repository

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestBuildTripUpdateFilter_AcceptedRequiresPending(t *testing.T) {
	id := primitive.NewObjectID()

	filter := buildTripUpdateFilter(id, "accepted")

	gotID, ok := filter["_id"].(primitive.ObjectID)
	if !ok {
		t.Fatalf("_id should be primitive.ObjectID")
	}
	if gotID != id {
		t.Fatalf("expected _id=%s, got %s", id.Hex(), gotID.Hex())
	}

	status, ok := filter["status"].(string)
	if !ok {
		t.Fatalf("status should exist for accepted update")
	}
	if status != "pending" {
		t.Fatalf("expected status gate pending, got %s", status)
	}
}

func TestBuildTripUpdateFilter_NonAcceptedNoStatusGate(t *testing.T) {
	id := primitive.NewObjectID()

	filter := buildTripUpdateFilter(id, "paid")

	gotID, ok := filter["_id"].(primitive.ObjectID)
	if !ok {
		t.Fatalf("_id should be primitive.ObjectID")
	}
	if gotID != id {
		t.Fatalf("expected _id=%s, got %s", id.Hex(), gotID.Hex())
	}

	if _, exists := filter["status"]; exists {
		t.Fatalf("status gate should not exist for non-accepted update")
	}
}
