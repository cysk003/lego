// Package infoblox implements a DNS provider for solving the DNS-01 challenge using on prem infoblox DNS.
package infoblox

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/platform/config/env"
	"github.com/go-acme/lego/v4/providers/dns/internal/useragent"
	infoblox "github.com/infobloxopen/infoblox-go-client/v2"
)

// Environment variables names.
const (
	envNamespace = "INFOBLOX_"

	EnvHost          = envNamespace + "HOST"
	EnvPort          = envNamespace + "PORT"
	EnvUsername      = envNamespace + "USERNAME"
	EnvPassword      = envNamespace + "PASSWORD"
	EnvDNSView       = envNamespace + "DNS_VIEW"
	EnvWApiVersion   = envNamespace + "WAPI_VERSION"
	EnvSSLVerify     = envNamespace + "SSL_VERIFY"
	EnvCACertificate = envNamespace + "CA_CERTIFICATE"

	EnvTTL                = envNamespace + "TTL"
	EnvPropagationTimeout = envNamespace + "PROPAGATION_TIMEOUT"
	EnvPollingInterval    = envNamespace + "POLLING_INTERVAL"
	EnvHTTPTimeout        = envNamespace + "HTTP_TIMEOUT"
)

const defaultPoolConnections = 10

var _ challenge.ProviderTimeout = (*DNSProvider)(nil)

// Config is used to configure the creation of the DNSProvider.
type Config struct {
	// Host is the URL of the grid manager.
	Host string
	// Port is the Port for the grid manager.
	Port string

	// Username the user for accessing API.
	Username string
	// Password the password for accessing API.
	Password string

	// DNSView is the dns view to put new records and search from.
	DNSView string
	// WapiVersion is the version of web api used.
	WapiVersion string

	// SSLVerify is whether or not to verify the ssl of the server being hit.
	SSLVerify bool

	// CACertificate is the path to the CA certificate (PEM encoded).
	CACertificate string

	PropagationTimeout time.Duration
	PollingInterval    time.Duration
	TTL                int
	HTTPTimeout        int
}

// NewDefaultConfig returns a default configuration for the DNSProvider.
func NewDefaultConfig() *Config {
	return &Config{
		DNSView:       env.GetOrDefaultString(EnvDNSView, "External"),
		WapiVersion:   env.GetOrDefaultString(EnvWApiVersion, "2.11"),
		Port:          env.GetOrDefaultString(EnvPort, "443"),
		SSLVerify:     env.GetOrDefaultBool(EnvSSLVerify, true),
		CACertificate: env.GetOrDefaultString(EnvCACertificate, ""),

		TTL:                env.GetOrDefaultInt(EnvTTL, dns01.DefaultTTL),
		PropagationTimeout: env.GetOrDefaultSecond(EnvPropagationTimeout, dns01.DefaultPropagationTimeout),
		PollingInterval:    env.GetOrDefaultSecond(EnvPollingInterval, dns01.DefaultPollingInterval),
		HTTPTimeout:        env.GetOrDefaultInt(EnvHTTPTimeout, 30),
	}
}

// DNSProvider implements the challenge.Provider interface.
type DNSProvider struct {
	config          *Config
	transportConfig infoblox.TransportConfig
	ibConfig        infoblox.HostConfig
	ibAuth          infoblox.AuthConfig

	recordRefs   map[string]string
	recordRefsMu sync.Mutex
}

// NewDNSProvider returns a DNSProvider instance configured for Infoblox.
// Credentials must be passed in the environment variables:
// INFOBLOX_USERNAME, INFOBLOX_PASSWORD
// INFOBLOX_HOST, INFOBLOX_PORT
// INFOBLOX_DNS_VIEW, INFOBLOX_WAPI_VERSION
// INFOBLOX_SSL_VERIFY.
func NewDNSProvider() (*DNSProvider, error) {
	values, err := env.Get(EnvHost, EnvUsername, EnvPassword)
	if err != nil {
		return nil, fmt.Errorf("infoblox: %w", err)
	}

	config := NewDefaultConfig()
	config.Host = values[EnvHost]
	config.Username = values[EnvUsername]
	config.Password = values[EnvPassword]

	return NewDNSProviderConfig(config)
}

// NewDNSProviderConfig return a DNSProvider instance configured for HyperOne.
func NewDNSProviderConfig(config *Config) (*DNSProvider, error) {
	if config == nil {
		return nil, errors.New("infoblox: the configuration of the DNS provider is nil")
	}

	if config.Host == "" {
		return nil, errors.New("infoblox: missing host")
	}

	if config.Username == "" || config.Password == "" {
		return nil, errors.New("infoblox: missing credentials")
	}

	var sslVerify string
	if config.CACertificate != "" {
		sslVerify = config.CACertificate
	} else {
		sslVerify = strconv.FormatBool(config.SSLVerify)
	}

	return &DNSProvider{
		config:          config,
		transportConfig: infoblox.NewTransportConfig(sslVerify, config.HTTPTimeout, defaultPoolConnections),
		ibConfig: infoblox.HostConfig{
			Host:    config.Host,
			Version: config.WapiVersion,
			Port:    config.Port,
		},
		ibAuth: infoblox.AuthConfig{
			Username: config.Username,
			Password: config.Password,
		},
		recordRefs: make(map[string]string),
	}, nil
}

// Timeout returns the timeout and interval to use when checking for DNS propagation.
func (d *DNSProvider) Timeout() (timeout, interval time.Duration) {
	return d.config.PropagationTimeout, d.config.PollingInterval
}

// Present creates a TXT record to fulfill the dns-01 challenge.
func (d *DNSProvider) Present(domain, token, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)

	connector, err := infoblox.NewConnector(d.ibConfig, d.ibAuth, d.transportConfig, &infoblox.WapiRequestBuilder{}, &infoblox.WapiHttpRequestor{})
	if err != nil {
		return fmt.Errorf("infoblox: %w", err)
	}

	defer func() { _ = connector.Logout() }()

	objectManager := infoblox.NewObjectManager(connector, useragent.Get(), "")

	record, err := objectManager.CreateTXTRecord(d.config.DNSView, dns01.UnFqdn(info.EffectiveFQDN), info.Value, uint32(d.config.TTL), true, "lego", nil)
	if err != nil {
		return fmt.Errorf("infoblox: could not create TXT record for %s: %w", domain, err)
	}

	d.recordRefsMu.Lock()
	d.recordRefs[token] = record.Ref
	d.recordRefsMu.Unlock()

	return nil
}

// CleanUp removes the TXT record matching the specified parameters.
func (d *DNSProvider) CleanUp(domain, token, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)

	connector, err := infoblox.NewConnector(d.ibConfig, d.ibAuth, d.transportConfig, &infoblox.WapiRequestBuilder{}, &infoblox.WapiHttpRequestor{})
	if err != nil {
		return fmt.Errorf("infoblox: %w", err)
	}

	defer func() { _ = connector.Logout() }()

	objectManager := infoblox.NewObjectManager(connector, useragent.Get(), "")

	// gets the record's unique ref from when we created it
	d.recordRefsMu.Lock()
	recordRef, ok := d.recordRefs[token]
	d.recordRefsMu.Unlock()
	if !ok {
		return fmt.Errorf("infoblox: unknown record ID for '%s' '%s'", info.EffectiveFQDN, token)
	}

	_, err = objectManager.DeleteTXTRecord(recordRef)
	if err != nil {
		return fmt.Errorf("infoblox: could not delete TXT record for %s: %w", domain, err)
	}

	// Delete record ref from map
	d.recordRefsMu.Lock()
	delete(d.recordRefs, token)
	d.recordRefsMu.Unlock()

	return nil
}
