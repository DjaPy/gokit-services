package orders

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// HTTPAPI exposes Store/OrderProcessor over REST. It is a thin transport
// adapter — all business logic lives in Store and OrderProcessor, so the
// gRPC adapter in grpcapi.go implements the exact same operations against
// the exact same state.
type HTTPAPI struct {
	store     *Store
	processor *OrderProcessor
}

func NewHTTPAPI(store *Store, processor *OrderProcessor) *HTTPAPI {
	return &HTTPAPI{store: store, processor: processor}
}

// Mux builds the http.Handler for httpserver.NewServer.
func (a *HTTPAPI) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", a.handleCreate)
	mux.HandleFunc("GET /orders", a.handleList)
	mux.HandleFunc("GET /orders/{id}", a.handleGet)
	return mux
}

type createOrderItem struct {
	SKU      string `json:"sku"`
	Quantity int32  `json:"quantity"`
}

type createOrderRequest struct {
	CustomerID string            `json:"customer_id"`
	Items      []createOrderItem `json:"items"`
}

type orderResponse struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func toResponse(o Order) orderResponse {
	return orderResponse{
		ID:         o.ID,
		CustomerID: o.CustomerID,
		Status:     string(o.Status),
		CreatedAt:  o.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  o.UpdatedAt.Format(time.RFC3339),
	}
}

func (a *HTTPAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CustomerID == "" || len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "customer_id and items are required")
		return
	}

	items := make([]Item, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, Item(it))
	}

	order := a.store.Create(req.CustomerID, items)

	enqueueCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.processor.Enqueue(enqueueCtx, order.ID); err != nil {
		// The order is already persisted (PENDING); it will simply never be
		// auto-confirmed and the cleanup job will eventually cancel it.
		slog.Error("failed to enqueue order for processing", "order_id", order.ID, "error", err)
	}

	writeJSON(w, http.StatusCreated, toResponse(*order))
}

func (a *HTTPAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	order, err := a.store.Get(id)
	if err != nil {
		if errors.Is(err, ErrOrderNotFound) {
			writeError(w, http.StatusNotFound, "order not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toResponse(order))
}

func (a *HTTPAPI) handleList(w http.ResponseWriter, _ *http.Request) {
	orders := a.store.List()
	out := make([]orderResponse, 0, len(orders))
	for _, o := range orders {
		out = append(out, toResponse(o))
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write JSON response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
