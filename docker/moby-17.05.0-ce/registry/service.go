package registry

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/context"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/client/auth"
	"github.com/docker/docker/api/types"
	registrytypes "github.com/docker/docker/api/types/registry"
)

const (
	// DefaultSearchLimit is the default value for maximum number of returned search results.
	DefaultSearchLimit = 25
)

// Service is the interface defining what a registry service should implement.
// 赋值为 DefaultService 结构，见registry.NewService
type Service interface {
	Auth(ctx context.Context, authConfig *types.AuthConfig, userAgent string) (status, token string, err error)
	LookupPullEndpoints(hostname string) (endpoints []APIEndpoint, err error)
	LookupPushEndpoints(hostname string) (endpoints []APIEndpoint, err error)
	ResolveRepository(name reference.Named) (*RepositoryInfo, error)
	Search(ctx context.Context, term string, limit int, authConfig *types.AuthConfig, userAgent string, headers map[string][]string) (*registrytypes.SearchResults, error)
	ServiceConfig() *registrytypes.ServiceConfig
	TLSConfig(hostname string) (*tls.Config, error)
	LoadMirrors([]string) error
	LoadInsecureRegistries([]string) error
}

// DefaultService is a registry service. It tracks configuration data such as a list
// of mirrors.
//registry/service.go 中的 NewService 构造使用该结构，
type DefaultService struct { //也就是endpoint，见lookupEndpoints  new见 ResolveRepository newRepositoryInfo
	//赋值见NewService，赋值为 newServiceConfig 的返回值
	config *serviceConfig
	mu     sync.Mutex
}

// NewService returns a new instance of DefaultService ready to be
// installed into an engine.
// registry/service.go 新建一个default的 registryserver
func NewService(options ServiceOptions) *DefaultService {
	return &DefaultService{
		config: newServiceConfig(options),
	}
}

// ServiceConfig returns the public registry service configuration.
func (s *DefaultService) ServiceConfig() *registrytypes.ServiceConfig {
	s.mu.Lock()
	defer s.mu.Unlock()

	servConfig := registrytypes.ServiceConfig{
		InsecureRegistryCIDRs: make([]*(registrytypes.NetIPNet), 0),
		IndexConfigs:          make(map[string]*(registrytypes.IndexInfo)),
		Mirrors:               make([]string, 0),
	}

	// construct a new ServiceConfig which will not retrieve s.Config directly,
	// and look up items in s.config with mu locked
	servConfig.InsecureRegistryCIDRs = append(servConfig.InsecureRegistryCIDRs, s.config.ServiceConfig.InsecureRegistryCIDRs...)

	for key, value := range s.config.ServiceConfig.IndexConfigs {
		servConfig.IndexConfigs[key] = value
	}

	servConfig.Mirrors = append(servConfig.Mirrors, s.config.ServiceConfig.Mirrors...)

	return &servConfig
}

// LoadMirrors loads registry mirrors for Service
func (s *DefaultService) LoadMirrors(mirrors []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.config.LoadMirrors(mirrors)
}

// LoadInsecureRegistries loads insecure registries for Service
func (s *DefaultService) LoadInsecureRegistries(registries []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.config.LoadInsecureRegistries(registries)
}

// Auth contacts the public registry with the provided credentials,
// and returns OK if authentication was successful.
// It can be used to verify the validity of a client's credentials.
func (s *DefaultService) Auth(ctx context.Context, authConfig *types.AuthConfig, userAgent string) (status, token string, err error) {
	// TODO Use ctx when searching for repositories
	serverAddress := authConfig.ServerAddress
	if serverAddress == "" {
		serverAddress = IndexServer
	}
	if !strings.HasPrefix(serverAddress, "https://") && !strings.HasPrefix(serverAddress, "http://") {
		serverAddress = "https://" + serverAddress
	}
	u, err := url.Parse(serverAddress)
	if err != nil {
		return "", "", fmt.Errorf("unable to parse server address: %v", err)
	}

	endpoints, err := s.LookupPushEndpoints(u.Host)
	if err != nil {
		return "", "", err
	}

	for _, endpoint := range endpoints {
		login := loginV2
		if endpoint.Version == APIVersion1 {
			login = loginV1
		}

		status, token, err = login(authConfig, endpoint, userAgent)
		if err == nil {
			return
		}
		if fErr, ok := err.(fallbackError); ok {
			err = fErr.err
			logrus.Infof("Error logging in to %s endpoint, trying next endpoint: %v", endpoint.Version, err)
			continue
		}
		return "", "", err
	}

	return "", "", err
}

// splitReposSearchTerm breaks a search term into an index name and remote name
func splitReposSearchTerm(reposName string) (string, string) {
	nameParts := strings.SplitN(reposName, "/", 2)
	var indexName, remoteName string
	if len(nameParts) == 1 || (!strings.Contains(nameParts[0], ".") &&
		!strings.Contains(nameParts[0], ":") && nameParts[0] != "localhost") {
		// This is a Docker Index repos (ex: samalba/hipache or ubuntu)
		// 'docker.io'
		indexName = IndexName
		remoteName = reposName
	} else {
		indexName = nameParts[0]
		remoteName = nameParts[1]
	}
	return indexName, remoteName
}

// Search queries the public registry for images matching the specified
// search terms, and returns the results.
func (s *DefaultService) Search(ctx context.Context, term string, limit int, authConfig *types.AuthConfig, userAgent string, headers map[string][]string) (*registrytypes.SearchResults, error) {
	// TODO Use ctx when searching for repositories
	if err := validateNoScheme(term); err != nil {
		return nil, err
	}

	indexName, remoteName := splitReposSearchTerm(term)

	// Search is a long-running operation, just lock s.config to avoid block others.
	s.mu.Lock()
	index, err := newIndexInfo(s.config, indexName)
	s.mu.Unlock()

	if err != nil {
		return nil, err
	}

	// *TODO: Search multiple indexes.
	endpoint, err := NewV1Endpoint(index, userAgent, http.Header(headers))
	if err != nil {
		return nil, err
	}

	var client *http.Client
	if authConfig != nil && authConfig.IdentityToken != "" && authConfig.Username != "" {
		creds := NewStaticCredentialStore(authConfig)
		scopes := []auth.Scope{
			auth.RegistryScope{
				Name:    "catalog",
				Actions: []string{"search"},
			},
		}

		modifiers := DockerHeaders(userAgent, nil)
		v2Client, foundV2, err := v2AuthHTTPClient(endpoint.URL, endpoint.client.Transport, modifiers, creds, scopes)
		if err != nil {
			if fErr, ok := err.(fallbackError); ok {
				logrus.Errorf("Cannot use identity token for search, v2 auth not supported: %v", fErr.err)
			} else {
				return nil, err
			}
		} else if foundV2 {
			// Copy non transport http client features
			v2Client.Timeout = endpoint.client.Timeout
			v2Client.CheckRedirect = endpoint.client.CheckRedirect
			v2Client.Jar = endpoint.client.Jar

			logrus.Debugf("using v2 client for search to %s", endpoint.URL)
			client = v2Client
		}
	}

	if client == nil {
		client = endpoint.client
		if err := authorizeClient(client, authConfig, endpoint); err != nil {
			return nil, err
		}
	}

	r := newSession(client, authConfig, endpoint)

	if index.Official {
		localName := remoteName
		if strings.HasPrefix(localName, "library/") {
			// If pull "library/foo", it's stored locally under "foo"
			localName = strings.SplitN(localName, "/", 2)[1]
		}

		return r.SearchRepositories(localName, limit)
	}
	return r.SearchRepositories(remoteName, limit)
}

// ResolveRepository splits a repository name into its components
// and configuration of the associated registry.
//docker pull  mysql@sha256:8d9cc6ff6a7ac9916c3384e864fb04b8ee9415b572f872a2a4c5b909dbbca81b name对应 docker.io/library/mysql@sha256:8d9cc6ff6a7ac9916c3384e864fb04b8ee9415b572f872a2a4c5b909dbbca81b
//docker pull harbor.intra.XXX.com/XXX/centos:20150101 name对应 harbor.intra.XXX.com/XXX/centos:20150101
//构造仓库地址信息
func (s *DefaultService) ResolveRepository(name reference.Named) (*RepositoryInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return newRepositoryInfo(s.config, name)
}

// APIEndpoint represents a remote API endpoint
//(s *DefaultService) LookupPullEndpoints lookupV2Endpoints 中构造该结构，
// v2Pusher中包含该结构
type APIEndpoint struct {
	Mirror       bool
	URL          *url.URL
	//APIVersion1 或者 APIVersion2
	Version      APIVersion
	//官方的 docker.io 或者 index.docker.io
	Official     bool
	TrimHostname bool
	TLSConfig    *tls.Config
}

// ToV1Endpoint returns a V1 API endpoint based on the APIEndpoint
func (e APIEndpoint) ToV1Endpoint(userAgent string, metaHeaders http.Header) (*V1Endpoint, error) {
	return newV1Endpoint(*e.URL, e.TLSConfig, userAgent, metaHeaders)
}

// TLSConfig constructs a client TLS configuration based on server defaults
func (s *DefaultService) TLSConfig(hostname string) (*tls.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return newTLSConfig(hostname, isSecureIndex(s.config, hostname))
}

// tlsConfig constructs a client TLS configuration based on server defaults
func (s *DefaultService) tlsConfig(hostname string) (*tls.Config, error) {
	return newTLSConfig(hostname, isSecureIndex(s.config, hostname))
}

func (s *DefaultService) tlsConfigForMirror(mirrorURL *url.URL) (*tls.Config, error) {
	return s.tlsConfig(mirrorURL.Host)
}

// LookupPullEndpoints creates a list of endpoints to try to pull from, in order of preference.
// It gives preference to v2 endpoints over v1, mirrors over the actual
// registry, and HTTPS over plain HTTP.
// 通过repositoryInfo找到下载镜像的endpoint   hostname为//docker pull harbor.intra.XXX.com/XXX/centos:20150101 hostname对应 harbor.intra.XXX.com
func (s *DefaultService) LookupPullEndpoints(hostname string) (endpoints []APIEndpoint, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lookupEndpoints(hostname)
}

// LookupPushEndpoints creates a list of endpoints to try to push to, in order of preference.
// It gives preference to v2 endpoints over v1, and HTTPS over plain HTTP.
// Mirrors are not included.
func (s *DefaultService) LookupPushEndpoints(hostname string) (endpoints []APIEndpoint, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	allEndpoints, err := s.lookupEndpoints(hostname)
	if err == nil {
		for _, endpoint := range allEndpoints {
			if !endpoint.Mirror {
				endpoints = append(endpoints, endpoint)
			}
		}
	}
	return endpoints, err
}

/*
[root@newnamespace ~]# cat /etc/docker/daemon.json
{"registry-mirrors": ["http://a9e61d46.m.daocloud.io"]}

例如:docker pull abcdef:456456   如果没有指定url，则默认优先使用daemon.json中的配置registry-mirrors，如果该地址连不上或者无镜像，则从默认的registry-1.docker.io获取，并且都是V2
Trying to pull abcdef from http://a9e61d46.m.daocloud.io/ v2
Trying to pull abcdef from https://registry-1.docker.io v2

例如： docker pull abc.com/xx/abcdef:456456  如果指定有url，则可以通过V2和V1从指定的url获取镜像
Trying to pull abc.com/xx/abcdef from https://abc.com v2
Trying to pull abc.com/xx/abcdef from https://abc.com v1
*/
// hostname为//docker pull harbor.intra.XXX.com/XXX/centos:20150101 hostname对应 harbor.intra.XXX.com
func (s *DefaultService) lookupEndpoints(hostname string) (endpoints []APIEndpoint, err error) {
	endpoints, err = s.lookupV2Endpoints(hostname)
	if err != nil {
		return nil, err
	}

	if s.config.V2Only {
		return endpoints, nil
	}

	legacyEndpoints, err := s.lookupV1Endpoints(hostname)
	if err != nil {
		return nil, err
	}
	endpoints = append(endpoints, legacyEndpoints...)

	return endpoints, nil
}
