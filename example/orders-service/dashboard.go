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
	mux.HandleFunc("GET /partials/stats", d.handleStatsPartial)
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
// gRPC-sourced order tables render from. Status is normalized to the
// lowercase domain word (pending/confirmed/canceled) regardless of
// transport, so one set of badge styles serves both sources.
type dashboardOrder struct {
	ID         string
	CustomerID string
	Status     string
	Qty        int64
	CreatedAt  string // relative, e.g. "12s ago"
	CreatedTS  string // full RFC3339, shown as tooltip
}

// normalizeStatus maps both REST ("PENDING") and gRPC
// ("ORDER_STATUS_PENDING") status spellings to "pending".
func normalizeStatus(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ToUpper(s), "ORDER_STATUS_"))
}

// relTime renders t as a coarse "how long ago" string; the exact
// timestamp is still available in the row tooltip.
func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func (d *Dashboard) fetchOrders(ctx context.Context) ([]orderResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return httpclient.Do[[]orderResponse](ctx, d.restClient, http.MethodGet, "/orders")
}

func (d *Dashboard) handleStatsPartial(w http.ResponseWriter, r *http.Request) {
	list, err := d.fetchOrders(r.Context())
	view := statsView{}
	if err != nil {
		view.Error = fmt.Sprintf("REST API error: %v", err)
	}
	view.Total = len(list)
	for _, o := range list {
		switch normalizeStatus(o.Status) {
		case "pending":
			view.Pending++
		case "confirmed":
			view.Confirmed++
		case "canceled":
			view.Canceled++
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statsTmpl.Execute(w, view); err != nil {
		slog.Error("dashboard: render stats", "error", err)
	}
}

func (d *Dashboard) handleOrdersPartial(w http.ResponseWriter, r *http.Request) {
	list, err := d.fetchOrders(r.Context())
	if err != nil {
		renderOrdersPanel(w, sourceREST, nil, fmt.Sprintf("REST API error: %v", err))
		return
	}

	rows := make([]dashboardOrder, 0, len(list))
	for _, o := range list {
		var qty int64
		for _, it := range o.Items {
			qty += int64(it.Quantity)
		}
		row := dashboardOrder{
			ID:         o.ID,
			CustomerID: o.CustomerID,
			Status:     normalizeStatus(o.Status),
			Qty:        qty,
			CreatedAt:  o.CreatedAt,
			CreatedTS:  o.CreatedAt,
		}
		if t, err := time.Parse(time.RFC3339, o.CreatedAt); err == nil {
			row.CreatedAt = relTime(t)
		}
		rows = append(rows, row)
	}
	renderOrdersPanel(w, sourceREST, rows, "")
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
		Items:      []CreateOrderItem{{SKU: r.FormValue("sku"), Quantity: int32(quantity)}},
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

	if r.FormValue("source") == sourceGRPC {
		d.handleGRPCOrdersPartial(w, r)
		return
	}
	d.handleOrdersPartial(w, r)
}

func (d *Dashboard) handleGRPCOrdersPartial(w http.ResponseWriter, r *http.Request) {
	conn := d.grpcClient.Conn()
	if conn == nil {
		renderOrdersPanel(w, sourceGRPC, nil, "gRPC client has not connected yet — try again in a moment")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := ordersv1.NewOrdersServiceClient(conn).ListOrders(ctx, &ordersv1.ListOrdersRequest{})
	if err != nil {
		renderOrdersPanel(w, sourceGRPC, nil, fmt.Sprintf("gRPC error: %v", err))
		return
	}

	rows := make([]dashboardOrder, 0, len(resp.GetOrders()))
	for _, o := range resp.GetOrders() {
		var qty int64
		for _, it := range o.GetItems() {
			qty += int64(it.GetQuantity())
		}
		created := time.Unix(o.GetCreatedAtUnix(), 0)
		rows = append(rows, dashboardOrder{
			ID:         o.GetId(),
			CustomerID: o.GetCustomerId(),
			Status:     normalizeStatus(o.GetStatus().String()),
			Qty:        qty,
			CreatedAt:  relTime(created),
			CreatedTS:  created.Format(time.RFC3339),
		})
	}
	renderOrdersPanel(w, sourceGRPC, rows, "")
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

// Orders panel sources. The panel partial re-renders its own header (title,
// segmented source switcher, refresh indicator), so the pressed state of the
// switcher always matches the data below it — no client-side script needed.
const (
	sourceREST = "rest"
	sourceGRPC = "grpc"
)

func renderOrdersPanel(w http.ResponseWriter, source string, rows []dashboardOrder, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ordersPanelTmpl.Execute(w, ordersPanelView{Source: source, Rows: rows, Error: errMsg}); err != nil {
		slog.Error("dashboard: render orders panel", "error", err)
	}
}

type ordersPanelView struct {
	Source string
	Rows   []dashboardOrder
	Error  string
}

type statsView struct {
	Total     int
	Pending   int
	Confirmed int
	Canceled  int
	Error     string
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
  <div class="brand">
    <span class="glyph">os</span>
    orders-service
    <span class="sub">gokit-services example</span>
  </div>
  <div class="spacer"></div>
  <span id="health-pills" class="pills">
    <span class="pill">health&hellip;</span>
  </span>
</header>
<main>
  <section id="stats" class="stats" aria-label="Orders summary"
           hx-get="/partials/stats" hx-trigger="load, every 2s" hx-swap="innerHTML"></section>

  <div class="cols">
    <div class="side">
      <section class="card">
        <div class="card-head"><h2>Create order</h2></div>
        <div class="card-body">
          <p class="hint">POSTs to the REST API via <code>httpclient</code>; the handler enqueues async confirmation on <code>workerpool</code>.</p>
          <form hx-post="/partials/orders" hx-include="#current-source" hx-target="#orders-panel" hx-swap="innerHTML">
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
      </section>

      <section class="card">
        <div class="card-head">
          <h2>Health</h2>
          <div class="spacer"></div>
          <span class="refresh">every 3s</span>
        </div>
        <div class="card-body">
          <p class="hint">Polls <code>/healthz</code> and <code>/readyz</code> on healthserver via <code>httpclient</code>. Readiness flips once the periodic cleanup job has swept at least once.</p>
          <div id="health" hx-get="/partials/health" hx-trigger="load, every 3s" hx-swap="innerHTML">Loading&hellip;</div>
        </div>
      </section>
    </div>

    <section class="card">
      <div id="orders-panel" hx-get="/partials/orders" hx-trigger="load" hx-swap="innerHTML">
        <div class="card-head"><h2>Orders</h2></div>
        <div class="card-body"><p class="empty">Loading&hellip;</p></div>
      </div>
    </section>
  </div>
</main>
<footer>gokit-services example &mdash; <code>go run ./example/orders-service/cmd/orders-service</code></footer>
</body>
</html>
`))

// ordersPanelTmpl renders the whole orders card content: header with the
// source switcher, the table, and — only for the REST source — an empty
// polling element that refreshes the panel every 2s. Selecting gRPC swaps
// that element away, which stops the polling; selecting REST brings it back.
var ordersPanelTmpl = template.Must(template.New("orders-panel").Parse(`
<input type="hidden" id="current-source" name="source" value="{{ .Source }}" hx-swap-oob="true">
<div class="card-head">
  <h2>Orders</h2>
  <div class="seg" role="group" aria-label="Data source">
    <button type="button" aria-pressed="{{ if eq .Source "rest" }}true{{ else }}false{{ end }}"
            hx-get="/partials/orders" hx-target="#orders-panel" hx-swap="innerHTML">REST /orders</button>
    <button type="button" aria-pressed="{{ if eq .Source "grpc" }}true{{ else }}false{{ end }}"
            hx-get="/partials/grpc-orders" hx-target="#orders-panel" hx-swap="innerHTML">gRPC ListOrders</button>
  </div>
  <div class="spacer"></div>
  <span class="refresh">{{ if eq .Source "rest" }}<span class="refresh-dot"></span>every 2s{{ else }}on demand{{ end }}</span>
</div>
<div class="card-body">
  {{ if eq .Source "rest" -}}
  <p class="hint">Polled every 2s via <code>httpclient.Do[T]</code> against the REST API.</p>
  {{- else -}}
  <p class="hint">Live <code>OrdersService/ListOrders</code> call over <code>grpcclient</code> &mdash; the same Store as the REST view. Click the source button again to refresh.</p>
  {{- end }}
  {{ if .Error -}}
  <p class="error">{{ .Error }}</p>
  {{- else if not .Rows -}}
  <p class="empty">No orders yet.</p>
  {{- else -}}
  <div class="table-wrap">
    <table>
      <thead><tr><th>ID</th><th>Customer</th><th>Status</th><th class="num">Qty</th><th>Created</th></tr></thead>
      <tbody>
      {{- range .Rows }}
        <tr>
          <td class="mono">{{ .ID }}</td>
          <td class="mono">{{ .CustomerID }}</td>
          <td><span class="badge badge-{{ .Status }}"><span class="dot"></span>{{ .Status }}</span></td>
          <td class="num">{{ .Qty }}</td>
          <td class="time" title="{{ .CreatedTS }}">{{ .CreatedAt }}</td>
        </tr>
      {{- end }}
      </tbody>
    </table>
  </div>
  {{- end }}
</div>
{{ if eq .Source "rest" }}<div hx-get="/partials/orders" hx-trigger="every 2s" hx-target="#orders-panel" hx-swap="innerHTML"></div>{{ end }}
`))

var statsTmpl = template.Must(template.New("stats").Parse(`
{{- if .Error -}}
<p class="error">{{ .Error }}</p>
{{- else -}}
<div class="stat">
  <span class="k">Total orders</span>
  <span class="v">{{ .Total }}</span>
  <span class="d">in store</span>
</div>
<div class="stat">
  <span class="k"><span class="dot dot-pending"></span>Pending</span>
  <span class="v">{{ .Pending }}</span>
  <span class="d">awaiting confirm on workerpool</span>
</div>
<div class="stat">
  <span class="k"><span class="dot dot-confirmed"></span>Confirmed</span>
  <span class="v">{{ .Confirmed }}</span>
  <span class="d">confirmed asynchronously</span>
</div>
<div class="stat">
  <span class="k"><span class="dot dot-canceled"></span>Canceled</span>
  <span class="v">{{ .Canceled }}</span>
  <span class="d">stale, swept by cleanup</span>
</div>
{{- end -}}
`))

// healthTmpl renders the health card body and, out-of-band, the compact
// status pills in the topbar — one poll updates both places.
var healthTmpl = template.Must(template.New("health").Parse(`
<div class="health-list">
  <div class="health-row">
    <span class="endpoint">GET /healthz</span>
    <span class="spacer"></span>
    <span class="state {{ if .LiveOK }}state-ok{{ else }}state-err{{ end }}"><span class="dot"></span><span class="txt">{{ .Live }}</span></span>
  </div>
  <div class="health-row">
    <span class="endpoint">GET /readyz</span>
    <span class="spacer"></span>
    <span class="state {{ if .ReadyOK }}state-ok{{ else }}state-err{{ end }}"><span class="dot"></span><span class="txt">{{ .Ready }}</span></span>
  </div>
</div>
<span id="health-pills" hx-swap-oob="innerHTML">
  <span class="pill"><span class="pip {{ if .LiveOK }}pip-ok live{{ else }}pip-err{{ end }}"></span>live <b>{{ if .LiveOK }}{{ .Live }}{{ else }}down{{ end }}</b></span>
  <span class="pill"><span class="pip {{ if .ReadyOK }}pip-ok{{ else }}pip-err{{ end }}"></span>ready <b>{{ if .ReadyOK }}{{ .Ready }}{{ else }}down{{ end }}</b></span>
</span>
`))
