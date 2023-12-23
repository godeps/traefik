package discovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/rules"
	"github.com/traefik/traefik/v3/pkg/server/service/loadbalancer/wrr"
)

// ServiceInstance is an instance of a service in a discovery system.
type ServiceInstance struct {
	// ID is the unique instance ID as registered.
	ID string `json:"id"`
	// Name is the service name as registered.
	Name string `json:"name"`
	// Version is the version of the compiled.
	Version string `json:"version"`
	// Metadata is the kv pair metadata associated with the service instance.
	Metadata map[string]string `json:"metadata"`
	// Endpoints are endpoint addresses of the service instance.
	// schema:
	//   http://127.0.0.1:8000?isSecure=false
	//   grpc://127.0.0.1:9000?isSecure=false
	Endpoints []string `json:"endpoints"`
}

// Discovery is service discovery.
type Discovery interface {
	// GetService return the service instances in memory according to the service name.
	//GetService(ctx context.Context, serviceName string) ([]*ServiceInstance, error)

	// PickService return the service instances in memory according to the service name.
	PickService(ctx context.Context, serviceName string) (*ServiceInstance, error)
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
	bl          *wrr.Balancer
	tree        *rules.Tree
	reg         *regexp.Regexp
}

const (
	RulePathRegexp = "PathRegexp"
	RuleHeader     = "Header"
	RuleConst      = "Const"
)

func New(sticky *dynamic.Sticky, serviceName string, backendServiceName string, servers []dynamic.Server) (*DiscoveryBalancer, error) {
	if defaultDC == nil && len(servers) == 0 {
		return nil, fmt.Errorf("cannot found default discovery service")
	}

	newParser, err := rules.NewParser([]string{RulePathRegexp, RuleHeader, RuleConst})
	if err != nil {
		return nil, fmt.Errorf("failed to NewParser. %v", err)
	}

	parse, err := newParser.Parse(backendServiceName)
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
		dc:          defaultDC,
		serviceName: serviceName,
		servers:     servers,
		bl:          wrr.New(sticky, false),
		tree:        tree,
	}

	if balancer.tree.Matcher == RulePathRegexp {
		balancer.reg = regexp.MustCompile(matcherValue)
	}

	return balancer, nil

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

	realName = strings.Replace(realName, "/", "", -1)
	serviceInfo, err := b.dc.PickService(req.Context(), realName)
	if err != nil {
		log.Ctx(req.Context()).Error().Msgf("PickService [%s] failed. err:%v", realName, err)
		//  have some backup servers ,we can try
		if len(b.servers) == 0 {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	var endpoint string
	if serviceInfo == nil || len(serviceInfo.Endpoints) == 0 {
		log.Ctx(req.Context()).Error().Msgf("PickService [%s] is not available. err:%v", realName, errNoAvailableServer)
		if len(b.servers) == 0 {
			http.Error(w, errNoAvailableServer.Error(), http.StatusServiceUnavailable)
			return
		}
		endpoint = b.servers[0].URL
	} else {
		endpoint = serviceInfo.Endpoints[0]
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		log.Ctx(req.Context()).Error().Msgf("url.Parse [%s] failed. err:%v", endpoint, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	target := req.URL
	target.Host = u.Host
	fmt.Println(target.String())

	proxy := httputil.NewSingleHostReverseProxy(target)

	b.bl.Add(b.serviceName, proxy, nil)

	b.bl.ServeHTTP(w, req)
}
