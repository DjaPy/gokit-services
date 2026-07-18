package serverkit

import (
	"fmt"
	"net/http"

	"github.com/DjaPy/gokit-services/httpclient"
	"github.com/DjaPy/gokit-services/httpserver"
	"github.com/prometheus/client_golang/prometheus"
)

// Addr builds the loopback address in-process clients dial for the given port.
func Addr(dialHost string, port int) string {
	return fmt.Sprintf("%s:%d", dialHost, port)
}

// NewHTTPServer builds an httpserver.Server bound on bindHost:port serving
// handler, with metrics on registry — the shared shape of the API and
// dashboard servers.
func NewHTTPServer(
	bindHost string,
	port int,
	appName string,
	handler http.Handler,
	registry prometheus.Registerer,
) *httpserver.Server {
	return httpserver.NewServer(handler,
		httpserver.WithHost(bindHost),
		httpserver.WithPort(port),
		httpserver.WithAppName(appName),
		httpserver.WithPrometheusRegisterer(registry),
	)
}

func MustHTTPClient(baseURL string) *httpclient.Client {
	c, err := httpclient.New(baseURL)
	if err != nil {
		panic(err)
	}
	return c
}
