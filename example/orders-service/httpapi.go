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
	store     Store
	processor *OrderProcessor
}

func NewHTTPAPI(store Store, processor *OrderProcessor) *HTTPAPI {
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

type CreateOrderItem struct {
	SKU      string `json:"sku"`
	Quantity int32  `json:"quantity"`
}

type createOrderRequest struct {
	CustomerID string            `json:"customer_id"`
	Items      []CreateOrderItem `json:"items"`
}

type orderResponse struct {
	ID         string            `json:"id"`
	CustomerID string            `json:"customer_id"`
	Status     string            `json:"status"`
	Items      []CreateOrderItem `json:"items"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
}

func toResponse(o Order) orderResponse {
	items := make([]CreateOrderItem, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, CreateOrderItem(it))
	}
	return orderResponse{
		ID:         o.ID,
		CustomerID: o.CustomerID,
		Status:     string(o.Status),
		Items:      items,
		CreatedAt:  o.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  o.UpdatedAt.Format(time.RFC3339),
	}
}

// maxCreateBodyBytes bounds the POST /orders request body so a single request
// can't force the server to buffer an unbounded payload.
const maxCreateBodyBytes = 64 << 10 // 64 KiB

func (a *HTTPAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBodyBytes)

	var req createOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	items := make([]Item, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, Item(it))
	}
	if reason, ok := ValidateNewOrder(req.CustomerID, items); !ok {
		writeError(w, http.StatusBadRequest, reason)
		return
	}

	order, err := a.store.Create(r.Context(), req.CustomerID, items)
	if err != nil {
		slog.Error("failed to create order", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	enqueueCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.processor.Enqueue(enqueueCtx, order.ID); err != nil {
		// The order is already persisted (PENDING); it will simply never be
		// auto-confirmed and the cleanup job will eventually cancel it.
		slog.Error("failed to enqueue order for processing", "order_id", order.ID, "error", err)
	}

	writeJSON(w, http.StatusCreated, toResponse(order))
}

func (a *HTTPAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	order, err := a.store.Get(r.Context(), id)
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

func (a *HTTPAPI) handleList(w http.ResponseWriter, r *http.Request) {
	orders, err := a.store.List(r.Context())
	if err != nil {
		slog.Error("failed to list orders", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
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
