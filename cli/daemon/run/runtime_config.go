package run

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/rs/xid"

	encore "encore.dev"
	"encore.dev/appruntime/exported/config"
	"encr.dev/pkg/option"
	"encr.dev/pkg/svcproxy"
	meta "encr.dev/proto/encore/parser/meta/v1"
)

const (
	runtimeCfgEnvVar    = "ENCORE_RUNTIME_CONFIG"
	appSecretsEnvVar    = "ENCORE_APP_SECRETS"
	serviceCfgEnvPrefix = "ENCORE_CFG_"
	listenEnvVar        = "ENCORE_LISTEN_ADDR"
)

type RuntimeEnvGenerator struct {
	// DI interfaces

	// The application to generate the config for
	App interface {
		PlatformID() string
		PlatformOrLocalID() string
	}

	// The infra manager to use
	InfraManager interface {
		SQLConfig(db *meta.SQLDatabase) (config.SQLServer, config.SQLDatabase, error)
		PubSubTopicConfig(topic *meta.PubSubTopic) (config.PubsubProvider, config.PubsubTopic, error)
		PubSubSubscriptionConfig(topic *meta.PubSubTopic, sub *meta.PubSubTopic_Subscription) (config.PubsubSubscription, error)
		RedisConfig(redis *meta.CacheCluster) (config.RedisServer, config.RedisDatabase, error)
	}

	// Data from the build which is required
	Meta       *meta.Data        // The metadata for the build
	Secrets    map[string]string // All the secrets for the application
	SvcConfigs map[string]string // All the compiled service configs for the application

	// General data about the application
	AppID           option.Option[string]                 // The ID of the application (if not set defaults to the local or platform ID)
	EnvID           option.Option[string]                 // The ID of the environment (if not set defaults to "local")
	EnvName         option.Option[string]                 // The name of the environment (if not set defaults to "local")
	EnvType         option.Option[encore.EnvironmentType] // The type of the environment (if not set defaults to the development environment type)
	CloudType       option.Option[encore.CloudProvider]   // The cloud type (if not set defaults to the local cloud type)
	TraceEndpoint   option.Option[string]                 // The endpoint to send trace data to (if not set defaults to none)
	ServiceAuthType option.Option[string]                 // Auth type to use for service to service calls (defaults to "encore-auth")
	AuthKey         option.Option[config.EncoreAuthKey]   // The auth key to use for service to service calls (if not set generates one on init)
	MetricsConfig   option.Option[*config.Metrics]        // The metrics config to use (if not set defaults to none)

	// Data about this specific run
	DaemonProxyAddr option.Option[netip.AddrPort] // The address of the daemon proxy (if not set defaults to the gateway address in ListenAddresses)
	ListenAddresses *ListenAddresses              // The listen addresses for the application

	// Internal data set the first time a value is requested
	initOnce   sync.Once // used to protect [init] from being called more than once
	deployID   string
	deployTime time.Time
	authKey    config.EncoreAuthKey

	pkgsBySvc        map[string][]*meta.Package
	dbsBySvc         map[string][]*meta.SQLDatabase
	topicsBySvc      map[string][]*meta.PubSubTopic
	subsBySvcByTopic map[string]map[string][]*meta.PubSubTopic_Subscription
	cachesBySvc      map[string][]*meta.CacheCluster
}

// ForServices generates the runtime environmental variables required for the build to
// startup and run the given service(s)
func (g *RuntimeEnvGenerator) ForServices(listenAddr netip.AddrPort, services ...*meta.Service) ([]string, error) {
	// Generate all we need for a given service
	runtimeCfg, err := g.runtimeConfigForServices(services)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate runtime config")
	}

	requiredSecrets, err := g.secretsForServices(services)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate secrets")
	}

	serviceCfgs, err := g.serviceConfigsForServices(services)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate service configs")
	}

	// Create env vars
	envs := []string{
		fmt.Sprintf("%s=%s", runtimeCfgEnvVar, runtimeCfg),
		fmt.Sprintf("%s=%s", appSecretsEnvVar, requiredSecrets),
		fmt.Sprintf("%s=%s", listenEnvVar, listenAddr.String()),
	}

	for serviceName, cfgString := range serviceCfgs {
		envs = append(envs,
			fmt.Sprintf(
				"%s%s=%s",
				serviceCfgEnvPrefix,
				strings.ToUpper(serviceName),
				base64.RawURLEncoding.EncodeToString([]byte(cfgString)),
			),
		)
	}

	return envs, nil
}

// runtimeConfigForServices generates the runtime config for a built binary to
// run the given service(s)
func (g *RuntimeEnvGenerator) runtimeConfigForServices(services []*meta.Service) (string, error) {
	g.init()

	daemonProxyURL := option.Map(g.DaemonProxyAddr, func(t netip.AddrPort) string { return fmt.Sprintf("http://%s", t) })

	// Build the base config
	runtimeCfg := &config.Runtime{
		AppID:         g.AppID.GetOrElseF(g.App.PlatformOrLocalID),
		AppSlug:       g.App.PlatformID(),
		APIBaseURL:    daemonProxyURL.GetOrElse(g.ListenAddresses.Gateway.BaseURL),
		EnvID:         g.EnvID.GetOrElse("local"),
		EnvName:       g.EnvName.GetOrElse("local"),
		EnvType:       string(g.EnvType.GetOrElse(encore.EnvDevelopment)),
		EnvCloud:      g.CloudType.GetOrElse(encore.CloudLocal),
		DeployID:      g.deployID,
		DeployedAt:    g.deployTime,
		TraceEndpoint: g.TraceEndpoint.GetOrElse(""),
		AuthKeys:      []config.EncoreAuthKey{g.authKey},
		CORS:          nil, // Services behind a gateway should not have CORS enabled
		Metrics:       g.MetricsConfig.GetOrElse(nil),
		ServiceAuth:   []config.ServiceAuth{{Method: g.ServiceAuthType.GetOrElse("encore-auth")}},
		PubsubTopics:  make(map[string]*config.PubsubTopic),
	}

	// If we're not running an all-in-one service, we need to generate a service discovery map
	if len(services) < len(g.Meta.Svcs) {
		svcDiscovery, err := g.ListenAddresses.GenerateServiceDiscoveryMap(g.Meta.Svcs, g.ServiceAuthType.GetOrElse("encore-auth"))
		if err != nil {
			return "", errors.Wrap(err, "failed to generate service discovery map")
		}

		runtimeCfg.ServiceDiscovery = svcDiscovery
	}

	sqlServers := newIndexTracker[config.SQLServer]()
	pubsubProviders := newIndexTracker[config.PubsubProvider]()
	redisServers := newIndexTracker[config.RedisServer]()

	// For each service within the target, add the specific infrastructure config required
	for _, svc := range services {
		// Configure all the SQL databases for the service
		for _, sqlDB := range g.dbsBySvc[svc.Name] {
			server, db, err := g.InfraManager.SQLConfig(sqlDB)
			if err != nil {
				return "", errors.Wrapf(err, "failed to generate SQL config for database %s for service %s", db.DatabaseName, svc.Name)
			}

			db.ServerID = sqlServers.AddAndGetIndex(server)
			runtimeCfg.SQLDatabases = append(runtimeCfg.SQLDatabases, &db)
		}

		// Configure all the pubsub topics for the service
		for _, topic := range g.topicsBySvc[svc.Name] {
			// Only configure the topic if it hasn't already been configured
			// as we add additional state for the subscriptions
			if _, found := runtimeCfg.PubsubTopics[topic.Name]; !found {
				provider, topicCfg, err := g.InfraManager.PubSubTopicConfig(topic)
				if err != nil {
					return "", errors.Wrapf(err, "failed to generate pubsub config for topic %s for service %s", topic.Name, svc.Name)
				}

				topicCfg.ProviderID = pubsubProviders.AddAndGetIndex(provider)
				runtimeCfg.PubsubTopics[topic.Name] = &topicCfg
			}

			// Configure all the pubsub subscriptions for the topic within this service
			if subsByTopic, found := g.subsBySvcByTopic[svc.Name]; found {
				for _, sub := range subsByTopic[topic.Name] {
					subCfg, err := g.InfraManager.PubSubSubscriptionConfig(topic, sub)
					if err != nil {
						return "", errors.Wrapf(err, "failed to generate pubsub config for subscription %s for service %s", sub.Name, svc.Name)
					}

					runtimeCfg.PubsubTopics[topic.Name].Subscriptions[sub.Name] = &subCfg
				}
			}
		}

		// Configure all the redis databases for the service
		for _, cacheCluster := range g.cachesBySvc[svc.Name] {
			server, db, err := g.InfraManager.RedisConfig(cacheCluster)
			if err != nil {
				return "", errors.Wrapf(err, "failed to generate redis config for cache cluster %s for service %s", cacheCluster.Name, svc.Name)
			}

			db.ServerID = redisServers.AddAndGetIndex(server)
			runtimeCfg.RedisDatabases = append(runtimeCfg.RedisDatabases, &db)
		}
	}

	// Add the infrastructure config to the runtime config
	runtimeCfg.SQLServers = sqlServers.Values()
	runtimeCfg.PubsubProviders = pubsubProviders.Values()
	runtimeCfg.RedisServers = redisServers.Values()

	// Encode the runtime config
	runtimeCfgBytes, _ := json.Marshal(runtimeCfg)
	return base64.RawURLEncoding.EncodeToString(runtimeCfgBytes), nil
}

// secretsForServices generates the secrets for a service to be started with
func (g *RuntimeEnvGenerator) secretsForServices(services []*meta.Service) (string, error) {
	g.init()

	// Shortcut if we want it for all services
	if len(services) == len(g.Meta.Svcs) {
		return encodeSecretsEnv(g.Secrets), nil
	}

	// Otherwise build a map of just the secrets this list of services need
	rtn := make(map[string]string)
	var found bool
	for _, svc := range services {
		for _, pkg := range g.pkgsBySvc[svc.Name] {
			for _, secretName := range pkg.Secrets {
				rtn[secretName], found = g.Secrets[secretName]
				if !found {
					return "", errors.Newf("missing secret %s", secretName)
				}
			}
		}
	}

	return encodeSecretsEnv(rtn), nil
}

// serviceConfigsForServices generates the service configs required for a service
func (g *RuntimeEnvGenerator) serviceConfigsForServices(services []*meta.Service) (map[string]string, error) {
	// Shortcut if we want it for all services
	if len(services) == len(g.Meta.Svcs) {
		return g.SvcConfigs, nil
	}

	// Otherwise build a map of just the configs this list of services need
	rtn := make(map[string]string)
	for _, service := range services {
		if !service.HasConfig {
			continue
		}

		cfg, found := g.SvcConfigs[service.Name]
		if !found {
			return nil, errors.Newf("missing computed concrete config for service %s", service.Name)
		}

		rtn[service.Name] = cfg
	}

	return rtn, nil
}

func (g *RuntimeEnvGenerator) init() {
	g.initOnce.Do(func() {
		g.deployID = fmt.Sprintf("run_%s", xid.New().String())
		g.deployTime = time.Now()
		g.authKey = g.AuthKey.GetOrElseF(genAuthKey)

		g.pkgsBySvc = make(map[string][]*meta.Package)
		for _, pkg := range g.Meta.Pkgs {
			g.pkgsBySvc[pkg.ServiceName] = append(g.pkgsBySvc[pkg.ServiceName], pkg)
		}

		g.dbsBySvc = make(map[string][]*meta.SQLDatabase)
		for _, svc := range g.Meta.Svcs {
			for _, dbName := range svc.Databases {
				for _, db := range g.Meta.SqlDatabases {
					if db.Name == dbName {
						g.dbsBySvc[svc.Name] = append(g.dbsBySvc[svc.Name], db)
					}
				}
			}
		}

		g.topicsBySvc = make(map[string][]*meta.PubSubTopic)
		g.subsBySvcByTopic = make(map[string]map[string][]*meta.PubSubTopic_Subscription)
		for _, topic := range g.Meta.PubsubTopics {
			for _, publisher := range topic.Publishers {
				g.topicsBySvc[publisher.ServiceName] = append(g.topicsBySvc[publisher.ServiceName], topic)
			}

			for _, subscriber := range topic.Subscriptions {
				g.topicsBySvc[subscriber.ServiceName] = append(g.topicsBySvc[subscriber.ServiceName], topic)

				existing, found := g.subsBySvcByTopic[subscriber.ServiceName]
				if !found {
					existing = make(map[string][]*meta.PubSubTopic_Subscription)
					g.subsBySvcByTopic[subscriber.ServiceName] = existing
				}

				g.subsBySvcByTopic[subscriber.ServiceName][topic.Name] = append(g.subsBySvcByTopic[subscriber.ServiceName][topic.Name], subscriber)
			}
		}

		g.cachesBySvc = make(map[string][]*meta.CacheCluster)
		for _, cacheCluster := range g.Meta.CacheClusters {
			for _, keySpace := range cacheCluster.Keyspaces {
				g.cachesBySvc[keySpace.Service] = append(g.cachesBySvc[keySpace.Service], cacheCluster)
			}
		}
	})
}

type SvcNetCfg struct {
	BaseURL    string         // The base URL that other services should use to connect to this service
	ListenAddr netip.AddrPort // The address:port that this service should listen on
}

// ListenAddresses is a list of listen address and port numbers for services to run on
type ListenAddresses struct {
	Gateway  SvcNetCfg            // The entrypoint to the application
	Services map[string]SvcNetCfg // Map from service name to listen address
}

// GenerateListenAddresses generates a list of port numbers for services to run on
// given a list of metadata for an application.
//
// The port numbers will be randomly generated and are guaranteed to be free
// at the time this function is run (which might not be the cause when the
// service starts up!)
func GenerateListenAddresses(proxy *svcproxy.SvcProxy, serviceList []*meta.Service) (*ListenAddresses, error) {
	gatewayPort, err := freeLocalhostAddress()
	if err != nil {
		return nil, errors.Wrap(err, "failed to find free port for gateway")
	}

	portListings := &ListenAddresses{
		Gateway: SvcNetCfg{
			proxy.RegisterGateway("app", gatewayPort),
			gatewayPort,
		},
		Services: map[string]SvcNetCfg{},
	}

	for _, service := range serviceList {
		listen, err := freeLocalhostAddress()
		if err != nil {
			return nil, errors.Wrap(err, "failed to find free port for service")
		}

		portListings.Services[service.Name] = SvcNetCfg{
			BaseURL:    proxy.RegisterService(service.Name, listen),
			ListenAddr: listen,
		}
	}

	return portListings, nil
}

// GenerateServiceDiscoveryMap generates a map of service names to their
// listen addresses
func (la *ListenAddresses) GenerateServiceDiscoveryMap(serviceList []*meta.Service, authMethod string) (map[string]config.Service, error) {
	services := make(map[string]config.Service)

	// Add all the services from the app
	for _, svc := range serviceList {
		svcCfg, found := la.Services[svc.Name]
		if !found {
			return nil, errors.Newf("missing listen address for service %s", svc.Name)
		}

		services[svc.Name] = config.Service{
			Name:        svc.Name,
			URL:         svcCfg.BaseURL,
			Protocol:    config.Http,
			ServiceAuth: config.ServiceAuth{Method: authMethod},
		}
	}

	return services, nil
}

// freeLocalhostAddress returns the first free port number on the system.
func freeLocalhostAddress() (netip.AddrPort, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return netip.AddrPort{}, err
	}
	defer func() { _ = l.Close() }()

	return l.Addr().(*net.TCPAddr).AddrPort(), nil
}

type indexTracker[T comparable] struct {
	lookupSet map[T]int
	list      []*T
}

func newIndexTracker[T comparable]() *indexTracker[T] {
	return &indexTracker[T]{lookupSet: map[T]int{}}
}

func (t *indexTracker[T]) AddAndGetIndex(v T) int {
	idx, found := t.lookupSet[v]
	if found {
		return idx
	}

	idx = len(t.list)
	t.lookupSet[v] = idx
	t.list = append(t.list, &v)
	return idx
}

func (t *indexTracker[T]) Values() []*T {
	return t.list
}