package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/metrics"
	"github.com/traefik/traefik/v3/pkg/middlewares/accesslog"
	metricsMiddle "github.com/traefik/traefik/v3/pkg/middlewares/metrics"
	"github.com/traefik/traefik/v3/pkg/rules"
	"github.com/traefik/traefik/v3/pkg/tracing"
	otelTrace "go.opentelemetry.io/otel/trace"
	"golang.org/x/net/http/httpguts"
)

// Discovery is service discovery.
type Discovery interface {
	// GetService return the service instances in memory according to the service name.
	//GetService(ctx context.Context, serviceName string) ([]*ServiceInstance, error)

	// PickService return the service instances in memory according to the service name.
	PickService(ctx context.Context, serviceName string) (string, error)
}

var defaultDC Discovery

func SetDefaultDiscovery(dc Discovery) {
	defaultDC = dc
}

var errNoAvailableServer = errors.New("no available server")

type DiscoveryBalancer struct {
	dc          Discovery
	serviceName string
	servers     []dynamic.Server
	sticky      *dynamic.Sticky
	tree        *rules.Tree
	reg         *regexp.Regexp

	metricsRegistry metrics.Registry
	passHostHeader  bool
	flushInterval   time.Duration
	roundTripper    http.RoundTripper
	bufferPool      httputil.BufferPool
}

const (
	RulePathRegexp = "PathRegexp"
	RuleHeader     = "Header"
	RuleConst      = "Const"
)

// StatusClientClosedRequest non-standard HTTP status code for client disconnection.
const StatusClientClosedRequest = 499

// StatusClientClosedRequestText non-standard HTTP status for client disconnection.
const StatusClientClosedRequestText = "Client Closed Request"

const MetadataPrefix = "x-md-global-"
const HeaderRequestId = "request-id"
const AccessLogRequestId = "RequestId"
const AccessLogTraceId = "TraceId"

func New(sticky *dynamic.Sticky, serviceName string, transRule string, servers []dynamic.Server) (*DiscoveryBalancer, error) {
	if defaultDC == nil && len(servers) == 0 {
		return nil, fmt.Errorf("cannot found default discovery service")
	}

	newParser, err := rules.NewParser([]string{RulePathRegexp, RuleHeader, RuleConst})
	if err != nil {
		return nil, fmt.Errorf("failed to NewParser. %v", err)
	}

	parse, err := newParser.Parse(transRule)
	if err != nil {
		return nil, fmt.Errorf("failed to parse. %v", err)
	}

	treeBuilder, ok := parse.(rules.TreeBuilder)
	if !ok {
		return nil, fmt.Errorf("failed to parse. %v", err)
	}

	tree := treeBuilder()
	matcherValue := tree.Value[0]

	balancer := &DiscoveryBalancer{
		dc:             defaultDC,
		serviceName:    serviceName,
		servers:        servers,
		sticky:         sticky,
		tree:           tree,
		passHostHeader: true,
		flushInterval:  time.Duration(dynamic.DefaultFlushInterval),
	}

	if balancer.tree.Matcher == RulePathRegexp {
		balancer.reg = regexp.MustCompile(matcherValue)
	}

	return balancer, nil

}

func (b *DiscoveryBalancer) SetProxyParams(passHostHeader bool,
	flushInterval time.Duration,
	roundTripper http.RoundTripper,
	bufferPool httputil.BufferPool, metricsRegistry metrics.Registry) {
	b.bufferPool = bufferPool
	b.passHostHeader = passHostHeader
	b.flushInterval = flushInterval
	b.roundTripper = roundTripper

}

func (b *DiscoveryBalancer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var realName string
	matcherValue := b.tree.Value[0]
	switch b.tree.Matcher {
	case RulePathRegexp:
		realName = b.reg.FindString(req.URL.Path)
	case RuleHeader:
		realName = req.Header.Get(matcherValue)
	default:
		realName = matcherValue
	}

	if b.dc == nil {
		http.Error(w, "discovery service is not configured", http.StatusInternalServerError)
		return
	}

	realName = strings.Replace(realName, "/", "", -1)
	endpoint, err := b.dc.PickService(req.Context(), realName)
	if err != nil && len(b.servers) == 0 {
		log.Ctx(req.Context()).Error().Msgf("PickService [%s] failed. err:%v", realName, err)
		http.Error(w, "unknown service", http.StatusInternalServerError)
		return
	}

	if endpoint == "" {
		if len(b.servers) == 0 || b.servers[0].URL == "" {
			log.Ctx(req.Context()).Error().Msgf("PickService [%s] is not available. err:%v", realName, errNoAvailableServer)
			http.Error(w, errNoAvailableServer.Error(), http.StatusServiceUnavailable)
			return
		}

		log.Ctx(req.Context()).Warn().Msgf("PickService [%s] is not available. err:%v", realName, errNoAvailableServer)
		endpoint = b.servers[0].URL
		log.Ctx(req.Context()).Debug().Msgf("discovery endpoint failed. use default endpoint: %s", endpoint)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		log.Ctx(req.Context()).Error().Msgf("url.Parse [%s] failed. err:%v", endpoint, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	target := req.URL
	target.Host = u.Host
	if req.ProtoMajor == 1 {
		target.Scheme = "http"
	} else if req.ProtoMajor == 2 {
		target.Scheme = "h2c"
	} else {
		log.Ctx(req.Context()).Error().Msgf("request URL:[%s] protoMajor[%d] is not supported.", req.URL.String(), req.ProtoMajor)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	targetStr := target.String()
	log.Ctx(req.Context()).Debug().Msgf("target: %s", targetStr)
	proxy := buildSingleHostProxy(target, b.passHostHeader, b.flushInterval, b.roundTripper, nil)
	proxy = accesslog.NewFieldHandler(proxy, accesslog.ServiceURL, targetStr, nil)
	proxy = accesslog.NewFieldHandler(proxy, accesslog.ServiceAddr, target.Host, nil)
	proxy = accesslog.NewFieldHandler(proxy, accesslog.ServiceName, b.serviceName, nil)

	if b.metricsRegistry != nil {
		proxy = metricsMiddle.NewServiceMiddleware(req.Context(), proxy, b.metricsRegistry, b.serviceName)
	}

	span := tracing.GetSpan(req)
	if span != nil {
		span.SetTag("backend.endpoint", endpoint)
		span.SetTag("backend.url", targetStr)
		span.SetTag("backend.service", realName)
	}

	var requestId string
	if req.Header != nil {
		for k, _ := range req.Header {
			lowerKey := strings.ToLower(k)
			if !strings.HasPrefix(lowerKey, MetadataPrefix) {
				continue
			}
			_, key, ok := strings.Cut(lowerKey, MetadataPrefix)
			if !ok {
				continue
			}

			if strings.Contains(key, HeaderRequestId) {
				requestId = req.Header.Get(k)
			}

			if span != nil {
				span.SetTag(strings.Replace(key, "-", ".", -1), req.Header.Get(k))
			}
		}
	}

	traceId := getTraceID(req.Context())
	if requestId == "" {
		if traceId != "" {
			requestId = traceId
		} else {
			requestId = uuid.NewString()
		}
		req.Header.Set(MetadataPrefix+HeaderRequestId, requestId)
	}

	proxy = accesslog.NewFieldHandler(proxy, AccessLogTraceId, traceId, nil)
	proxy = accesslog.NewFieldHandler(proxy, AccessLogRequestId, requestId, accesslog.AddServiceFields)

	//bl := wrr.New(b.sticky, false)
	//bl.Add(b.serviceName, proxy, nil)
	//bl.ServeHTTP(w, req)

	proxy.ServeHTTP(w, req)
}

func buildSingleHostProxy(target *url.URL, passHostHeader bool, flushInterval time.Duration, roundTripper http.RoundTripper, bufferPool httputil.BufferPool) http.Handler {
	return &httputil.ReverseProxy{
		Director:      directorBuilder(target, passHostHeader),
		Transport:     roundTripper,
		FlushInterval: flushInterval,
		BufferPool:    bufferPool,
		ErrorHandler:  errorHandler,
	}
}

func directorBuilder(target *url.URL, passHostHeader bool) func(req *http.Request) {
	return func(outReq *http.Request) {
		outReq.URL.Scheme = target.Scheme
		outReq.URL.Host = target.Host

		u := outReq.URL
		if outReq.RequestURI != "" {
			parsedURL, err := url.ParseRequestURI(outReq.RequestURI)
			if err == nil {
				u = parsedURL
			}
		}

		outReq.URL.Path = u.Path
		outReq.URL.RawPath = u.RawPath
		// If a plugin/middleware adds semicolons in query params, they should be urlEncoded.
		outReq.URL.RawQuery = strings.ReplaceAll(u.RawQuery, ";", "&")
		outReq.RequestURI = "" // Outgoing request should not have RequestURI

		outReq.Proto = "HTTP/1.1"
		outReq.ProtoMajor = 1
		outReq.ProtoMinor = 1

		// Do not pass client Host header unless optsetter PassHostHeader is set.
		if !passHostHeader {
			outReq.Host = outReq.URL.Host
		}

		cleanWebSocketHeaders(outReq)
	}
}

// cleanWebSocketHeaders Even if the websocket RFC says that headers should be case-insensitive,
// some servers need Sec-WebSocket-Key, Sec-WebSocket-Extensions, Sec-WebSocket-Accept,
// Sec-WebSocket-Protocol and Sec-WebSocket-Version to be case-sensitive.
// https://tools.ietf.org/html/rfc6455#page-20
func cleanWebSocketHeaders(req *http.Request) {
	if !isWebSocketUpgrade(req) {
		return
	}

	req.Header["Sec-WebSocket-Key"] = req.Header["Sec-Websocket-Key"]
	delete(req.Header, "Sec-Websocket-Key")

	req.Header["Sec-WebSocket-Extensions"] = req.Header["Sec-Websocket-Extensions"]
	delete(req.Header, "Sec-Websocket-Extensions")

	req.Header["Sec-WebSocket-Accept"] = req.Header["Sec-Websocket-Accept"]
	delete(req.Header, "Sec-Websocket-Accept")

	req.Header["Sec-WebSocket-Protocol"] = req.Header["Sec-Websocket-Protocol"]
	delete(req.Header, "Sec-Websocket-Protocol")

	req.Header["Sec-WebSocket-Version"] = req.Header["Sec-Websocket-Version"]
	delete(req.Header, "Sec-Websocket-Version")
}

func isWebSocketUpgrade(req *http.Request) bool {
	return httpguts.HeaderValuesContainsToken(req.Header["Connection"], "Upgrade") &&
		strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
}

func errorHandler(w http.ResponseWriter, req *http.Request, err error) {
	statusCode := http.StatusInternalServerError

	switch {
	case errors.Is(err, io.EOF):
		statusCode = http.StatusBadGateway
	case errors.Is(err, context.Canceled):
		statusCode = StatusClientClosedRequest
	default:
		var netErr net.Error
		if errors.As(err, &netErr) {
			if netErr.Timeout() {
				statusCode = http.StatusGatewayTimeout
			} else {
				statusCode = http.StatusBadGateway
			}
		}
	}

	logger := log.Ctx(req.Context())
	logger.Debug().Err(err).Msgf("%d %s", statusCode, statusText(statusCode))

	w.WriteHeader(statusCode)
	if _, werr := w.Write([]byte(statusText(statusCode))); werr != nil {
		logger.Debug().Err(werr).Msg("Error while writing status code")
	}
}

func statusText(statusCode int) string {
	if statusCode == StatusClientClosedRequest {
		return StatusClientClosedRequestText
	}
	return http.StatusText(statusCode)
}

// TraceID returns a trace id valuer.
func getTraceID(ctx context.Context) string {
	if span := otelTrace.SpanContextFromContext(ctx); span.HasTraceID() {
		return span.TraceID().String()
	}
	return ""
}
