// Package proxy matches incoming requests to routes and forwards them to
// upstream services.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ZeynepDilanKilic/sentinel-gateway/internal/config"
)

// route is the runtime form of config.Route: everything pre-computed so the
// hot path (ServeHTTP) does no parsing or allocation beyond what is unavoidable.
type route struct {
	name   string
	prefix string
	strip  bool
	target *url.URL
	rp     *httputil.ReverseProxy
}

// Handler is the routing + proxying core of the gateway.
type Handler struct {
	routes []route // sorted longest prefix first
	log    *slog.Logger
}

// NewTransport builds the HTTP client transport used to talk to upstreams.
// Every timeout here is a defense against a slow or broken upstream
// tying up gateway resources.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil, // never honor HTTP_PROXY env vars for upstream traffic
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,  // TCP connect
			KeepAlive: 30 * time.Second, // TCP keepalive probes
		}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, // time to first byte of response headers
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
	}
}

// New builds a Handler from validated config.
func New(routes []config.Route, transport http.RoundTripper, log *slog.Logger) (*Handler, error) {
	if len(routes) == 0 {
		return nil, errors.New("proxy: no routes")
	}
	h := &Handler{log: log}

	for i := range routes {
		rc := routes[i]
		target := rc.UpstreamURL()
		if target == nil {
			return nil, errors.New("proxy: route " + rc.Name + " has no parsed upstream (config not validated?)")
		}

		r := route{name: rc.Name, prefix: rc.PathPrefix, strip: rc.StripPrefix, target: target}

		// Rewrite (Go 1.20+) replaces the older Director hook. Its main
		// advantage: ProxyRequest gives us both the inbound request (In) and
		// the outbound one (Out), and helpers that handle the security
		// details correctly (e.g. X-Forwarded-* spoofing).
		r.rp = &httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)  // scheme, host, and base path of the upstream
				pr.SetXForwarded() // X-Forwarded-For/Host/Proto from the *real* client
				pr.Out.Host = target.Host
				if r.strip {
					pr.Out.URL.Path = stripPrefix(pr.In.URL.Path, r.prefix)
					pr.Out.URL.RawPath = "" // let net/url re-encode from Path
				}
			},
			ErrorHandler: h.errorHandler(r.name),
			// FlushInterval < 0 means flush immediately; required for
			// streaming responses (SSE, chunked) to not buffer at the proxy.
			FlushInterval: -1,
		}
		h.routes = append(h.routes, r)
	}

	// Longest prefix must win: "/api/admin" before "/api".
	sort.SliceStable(h.routes, func(i, j int) bool {
		return len(h.routes[i].prefix) > len(h.routes[j].prefix)
	})
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r := h.match(req.URL.Path)
	if r == nil {
		http.Error(w, "no route", http.StatusNotFound)
		return
	}
	// Expose the chosen route to later middleware/logging via context.
	req = req.WithContext(withRouteName(req.Context(), r.name))
	r.rp.ServeHTTP(w, req)
}

func (h *Handler) match(path string) *route {
	for i := range h.routes {
		if matchPrefix(path, h.routes[i].prefix) {
			return &h.routes[i]
		}
	}
	return nil
}

// matchPrefix is a *segment-aware* prefix match: "/api" matches "/api" and
// "/api/x" but NOT "/apiary". A plain strings.HasPrefix would leak requests
// to the wrong service.
func matchPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

func stripPrefix(path, prefix string) string {
	if prefix == "/" {
		return path
	}
	p := strings.TrimPrefix(path, prefix)
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	return p
}

// errorHandler is called when the upstream is unreachable or misbehaves.
// We deliberately return a generic body: leaking "dial tcp 10.0.3.7:9001:
// connection refused" to clients is an information disclosure.
func (h *Handler) errorHandler(routeName string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, req *http.Request, err error) {
		status := http.StatusBadGateway
		if errors.Is(err, context.Canceled) {
			// Client went away; nothing sensible to write.
			status = 499 // nginx convention: client closed request
		} else if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		h.log.Error("upstream error",
			"route", routeName,
			"method", req.Method,
			"path", req.URL.Path,
			"status", status,
			"err", err,
		)
		if status == 499 {
			return
		}
		http.Error(w, http.StatusText(status), status)
	}
}

// --- request-scoped context ------------------------------------------------

type ctxKey int

const routeNameKey ctxKey = iota

func withRouteName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, routeNameKey, name)
}

// RouteName returns the matched route name for a request, or "" if none.
func RouteName(ctx context.Context) string {
	v, _ := ctx.Value(routeNameKey).(string)
	return v
}
