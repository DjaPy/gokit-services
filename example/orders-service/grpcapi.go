package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ordersv1 "github.com/DjaPy/gokit-services/example/orders-service/proto"
)

// GRPCAPI implements ordersv1.OrdersServiceServer over the same Store and
// OrderProcessor as HTTPAPI — the two transports are adapters over one set
// of business logic, not two independent implementations.
type GRPCAPI struct {
	ordersv1.UnimplementedOrdersServiceServer
	store     Store
	processor *OrderProcessor
}

func NewGRPCAPI(store Store, processor *OrderProcessor) *GRPCAPI {
	return &GRPCAPI{store: store, processor: processor}
}

func toProto(o Order) *ordersv1.Order {
	items := make([]*ordersv1.OrderItem, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, &ordersv1.OrderItem{Sku: it.SKU, Quantity: it.Quantity})
	}
	return &ordersv1.Order{
		Id:            o.ID,
		CustomerId:    o.CustomerID,
		Items:         items,
		Status:        toProtoStatus(o.Status),
		CreatedAtUnix: o.CreatedAt.Unix(),
		UpdatedAtUnix: o.UpdatedAt.Unix(),
	}
}

func toProtoStatus(s Status) ordersv1.OrderStatus {
	switch s {
	case StatusPending:
		return ordersv1.OrderStatus_ORDER_STATUS_PENDING
	case StatusConfirmed:
		return ordersv1.OrderStatus_ORDER_STATUS_CONFIRMED
	case StatusCanceled:
		return ordersv1.OrderStatus_ORDER_STATUS_CANCELED
	default:
		return ordersv1.OrderStatus_ORDER_STATUS_UNSPECIFIED
	}
}

func (a *GRPCAPI) CreateOrder(ctx context.Context, req *ordersv1.CreateOrderRequest) (*ordersv1.Order, error) {
	items := make([]Item, 0, len(req.GetItems()))
	for _, it := range req.GetItems() {
		items = append(items, Item{SKU: it.GetSku(), Quantity: it.GetQuantity()})
	}
	if reason, ok := ValidateNewOrder(req.GetCustomerId(), items); !ok {
		return nil, fmt.Errorf("create order: %w", status.Error(codes.InvalidArgument, reason))
	}

	order, err := a.store.Create(ctx, req.GetCustomerId(), items)
	if err != nil {
		return nil, fmt.Errorf("create order: %w", status.Error(codes.Internal, "internal error"))
	}

	enqueueCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.processor.Enqueue(enqueueCtx, order.ID); err != nil {
		slog.Error("failed to enqueue order for processing", "order_id", order.ID, "error", err)
	}

	return toProto(order), nil
}

func (a *GRPCAPI) GetOrder(ctx context.Context, req *ordersv1.GetOrderRequest) (*ordersv1.Order, error) {
	order, err := a.store.Get(ctx, req.GetId())
	if err != nil {
		if errors.Is(err, ErrOrderNotFound) {
			return nil, fmt.Errorf("get order: %w", status.Error(codes.NotFound, "order not found"))
		}
		return nil, fmt.Errorf("get order: %w", status.Error(codes.Internal, "internal error"))
	}
	return toProto(order), nil
}

func (a *GRPCAPI) ListOrders(ctx context.Context, _ *ordersv1.ListOrdersRequest) (*ordersv1.ListOrdersResponse, error) {
	all, err := a.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", status.Error(codes.Internal, "internal error"))
	}
	out := make([]*ordersv1.Order, 0, len(all))
	for _, o := range all {
		out = append(out, toProto(o))
	}
	return &ordersv1.ListOrdersResponse{Orders: out}, nil
}
