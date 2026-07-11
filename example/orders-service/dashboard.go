package orders

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DjaPy/gokit-services/grpcclient"
	"github.com/DjaPy/gokit-services/httpclient"

	ordersv1 "github.com/DjaPy/gokit-services/example/orders-service/proto"
)

//go:embed static/htmx.min.js static/style.css
var dashboardStatic embed.FS

// Dashboard is an htmx-powered UI that exercises every transport this
// example exposes. It deliberately holds no reference to Store: order
// creation and listing go through the REST API via httpclient.Client
// exactly as an external caller would, order listing is also available
// through a live grpcclient.Client connection to grpcserver, and liveness
// is read from healthserver over the same kind of httpclient call. The
// dashboard is a client of the other services, not a shortcut around them.
type Dashboard struct {
	restClient   *httpclient.Client
	healthClient *httpclient.Client
	grpcClient   *grpcclient.Client
}

// NewDashboard wires the dashboard to already-constructed clients. grpcClient
// may still be mid-connection when requests arrive — handlers check
// grpcClient.Conn() per-request rather than requiring it upfront.
func NewDashboard(restClient, healthClient *httpclient.Client, grpcClient *grpcclient.Client) *Dashboard {
	return &Dashboard{restClient: restClient, healthClient: healthClient, grpcClient: grpcClient}
}

// Mux builds the http.Handler for httpserver.NewServer.
func (d *Dashboard) Mux() http.Handler {
	mux := http.NewServeMux()
	staticFS, err := fs.Sub(dashboardStatic, "static")
	if err != nil {
		panic(fmt.Errorf("dashboard: sub static fs: %w", err))
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	mux.HandleFunc("GET /{$}", d.handleIndex)
	mux.HandleFunc("GET /partials/orders", d.handleOrdersPartial)
	mux.HandleFunc("POST /partials/orders", d.handleCreatePartial)
	mux.HandleFunc("GET /partials/grpc-orders", d.handleGRPCOrdersPartial)
	mux.HandleFunc("GET /partials/health", d.handleHealthPartial)
	return mux
}

func (d *Dashboard) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, nil); err != nil {
		slog.Error("dashboard: render index", "error", err)
	}
}

// dashboardOrder is the uniform shape both the REST-sourced and
// gRPC-sourced order tables render from.
type dashboardOrder struct {
	ID         string
	CustomerID string
	Status     string
	BadgeClass string
	CreatedAt  string
}

func (d *Dashboard) handleOrdersPartial(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	list, err := httpclient.Do[[]orderResponse](ctx, d.restClient, http.MethodGet, "/orders")
	if err != nil {
		renderOrders(w, nil, fmt.Sprintf("REST API error: %v", err))
		return
	}

	rows := make([]dashboardOrder, 0, len(list))
	for _, o := range list {
		rows = append(rows, dashboardOrder{
			ID:         o.ID,
			CustomerID: o.CustomerID,
			Status:     o.Status,
			BadgeClass: "badge-" + strings.ToLower(o.Status),
			CreatedAt:  o.CreatedAt,
		})
	}
	renderOrders(w, rows, "")
}

func (d *Dashboard) handleCreatePartial(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	const maxQuantity = 1_000_000
	quantity, err := strconv.ParseInt(r.FormValue("quantity"), 10, 32)
	if err != nil || quantity < 1 || quantity > maxQuantity {
		quantity = 1
	}

	body, err := json.Marshal(createOrderRequest{
		CustomerID: r.FormValue("customer_id"),
		Items:      []createOrderItem{{SKU: r.FormValue("sku"), Quantity: int32(quantity)}},
	})
	if err != nil {
		http.Error(w, "encode request", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := httpclient.Do[orderResponse](ctx, d.restClient, http.MethodPost, "/orders",
		httpclient.WithBody(bytes.NewReader(body), "application/json"),
	); err != nil {
		slog.Error("dashboard: create order via REST API", "error", err)
	}

	d.handleOrdersPartial(w, r)
}

func (d *Dashboard) handleGRPCOrdersPartial(w http.ResponseWriter, r *http.Request) {
	conn := d.grpcClient.Conn()
	if conn == nil {
		renderOrders(w, nil, "gRPC client has not connected yet — try again in a moment")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := ordersv1.NewOrdersServiceClient(conn).ListOrders(ctx, &ordersv1.ListOrdersRequest{})
	if err != nil {
		renderOrders(w, nil, fmt.Sprintf("gRPC error: %v", err))
		return
	}

	rows := make([]dashboardOrder, 0, len(resp.GetOrders()))
	for _, o := range resp.GetOrders() {
		status := o.GetStatus().String()
		rows = append(rows, dashboardOrder{
			ID:         o.GetId(),
			CustomerID: o.GetCustomerId(),
			Status:     status,
			BadgeClass: "badge-" + strings.ToLower(status),
			CreatedAt:  time.Unix(o.GetCreatedAtUnix(), 0).Format(time.RFC3339),
		})
	}
	renderOrders(w, rows, "")
}

type healthStatus struct {
	Status string `json:"status"`
}

func (d *Dashboard) handleHealthPartial(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	live, liveOK := "unreachable", false
	if resp, err := httpclient.Do[healthStatus](ctx, d.healthClient, http.MethodGet, "/healthz"); err == nil {
		live, liveOK = resp.Status, true
	}

	var ready string
	var readyOK bool
	if resp, err := httpclient.Do[healthStatus](ctx, d.healthClient, http.MethodGet, "/readyz"); err == nil {
		ready, readyOK = resp.Status, true
	} else {
		ready = err.Error()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := healthTmpl.Execute(w, healthView{
		Live: live, LiveOK: liveOK,
		Ready: ready, ReadyOK: readyOK,
	}); err != nil {
		slog.Error("dashboard: render health", "error", err)
	}
}

func renderOrders(w http.ResponseWriter, rows []dashboardOrder, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ordersTmpl.Execute(w, ordersView{Rows: rows, Error: errMsg}); err != nil {
		slog.Error("dashboard: render orders", "error", err)
	}
}

type ordersView struct {
	Rows  []dashboardOrder
	Error string
}

type healthView struct {
	Live    string
	LiveOK  bool
	Ready   string
	ReadyOK bool
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>orders-service dashboard</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx.min.js"></script>
</head>
<body>
<header class="topbar">
  <h1>orders-service</h1>
  <span>gokit-services example: entrypoint + httpserver + httpclient + grpcserver + grpcclient + healthserver + periodic + workerpool</span>
</header>
<main>
  <div class="card">
    <h2>Create order</h2>
    <p class="hint">POSTs to the REST API via <code>httpclient</code>; the handler enqueues async confirmation on <code>workerpool</code>.</p>
    <form hx-post="/partials/orders" hx-target="#rest-orders" hx-swap="innerHTML">
      <label>Customer ID
        <input type="text" name="customer_id" value="cust_1" required>
      </label>
      <label>SKU
        <input type="text" name="sku" value="WIDGET" required>
      </label>
      <label>Quantity
        <input type="number" name="quantity" value="1" min="1" required>
      </label>
      <button class="btn btn-primary" type="submit">Create order<span class="spinner">&hellip;</span></button>
    </form>
  </div>

  <div class="card">
    <h2>Health</h2>
    <p class="hint">Polls <code>/healthz</code> and <code>/readyz</code> on healthserver via <code>httpclient</code>. Readiness flips once the periodic cleanup job has swept at least once.</p>
    <div id="health" hx-get="/partials/health" hx-trigger="load, every 3s" hx-swap="innerHTML">Loading&hellip;</div>
  </div>

  <div class="card full-width">
    <h2>Orders &mdash; REST API</h2>
    <p class="hint">Polled every 2s via <code>httpclient.Do[T]</code> against the REST API.</p>
    <div id="rest-orders" hx-get="/partials/orders" hx-trigger="load, every 2s" hx-swap="innerHTML">Loading&hellip;</div>
  </div>

  <div class="card full-width">
    <h2>Orders &mdash; gRPC API</h2>
    <p class="hint">Calls <code>OrdersService/ListOrders</code> over a live <code>grpcclient.Client</code> connection to <code>grpcserver</code> &mdash; the same Store as the REST view above.</p>
    <button class="btn" hx-get="/partials/grpc-orders" hx-target="#grpc-orders" hx-swap="innerHTML">Call gRPC ListOrders<span class="spinner">&hellip;</span></button>
    <div id="grpc-orders" style="margin-top:0.9rem">Click the button to issue a live gRPC call.</div>
  </div>
</main>
<footer>gokit-services example &mdash; <code>go run ./example/orders-service/cmd/orders-service</code></footer>
</body>
</html>
`))

var ordersTmpl = template.Must(template.New("orders").Parse(`
{{- if .Error -}}
<p class="error">{{ .Error }}</p>
{{- else if not .Rows -}}
<p class="empty">No orders yet.</p>
{{- else -}}
<table>
  <thead><tr><th>ID</th><th>Customer</th><th>Status</th><th>Created</th></tr></thead>
  <tbody>
  {{- range .Rows }}
    <tr>
      <td class="id">{{ .ID }}</td>
      <td class="customer">{{ .CustomerID }}</td>
      <td><span class="badge {{ .BadgeClass }}">{{ .Status }}</span></td>
      <td>{{ .CreatedAt }}</td>
    </tr>
  {{- end }}
  </tbody>
</table>
{{- end -}}
`))

var healthTmpl = template.Must(template.New("health").Parse(`
<div class="health-row">
  <div class="health-item">
    <span class="health-label">Liveness (/healthz)</span>
    <span class="dot {{ if .LiveOK }}dot-ok{{ else }}dot-err{{ end }}">{{ .Live }}</span>
  </div>
  <div class="health-item">
    <span class="health-label">Readiness (/readyz)</span>
    <span class="dot {{ if .ReadyOK }}dot-ok{{ else }}dot-err{{ end }}">{{ .Ready }}</span>
  </div>
</div>
`))
