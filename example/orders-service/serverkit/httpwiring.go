package serverkit

import (
	"fmt"
	"net/http"

	httpcli "github.com/DjaPy/gokit-services/pkg/http/client"
	httpsrv "github.com/DjaPy/gokit-services/pkg/http/server"
	"github.com/prometheus/client_golang/prometheus"
)

// Addr builds the loopback address in-process clients dial for the given port.
func Addr(dialHost string, port int) string {
	return fmt.Sprintf("%s:%d", dialHost, port)
}

// NewHTTPServer builds an httpsrv.Server bound on bindHost:port serving
// handler, with metrics on registry — the shared shape of the API and
// dashboard servers.
func NewHTTPServer(
	bindHost string,
	port int,
	appName string,
	handler http.Handler,
	registry prometheus.Registerer,
) *httpsrv.Server {
	return httpsrv.NewServer(handler,
		httpsrv.WithHost(bindHost),
		httpsrv.WithPort(port),
		httpsrv.WithAppName(appName),
		httpsrv.WithPrometheusRegisterer(registry),
	)
}

func MustHTTPClient(baseURL string) *httpcli.Client {
	c, err := httpcli.New(baseURL)
	if err != nil {
		panic(err)
	}
	return c
}
