// Package cloudflare implements a DNS provider for solving the DNS-01 challenge using cloudflare DNS.
package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/log"
	"github.com/go-acme/lego/v4/platform/config/env"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare/internal"
)

// Environment variables names.
const (
	envNamespace = "CLOUDFLARE_"

	EnvEmail  = envNamespace + "EMAIL"
	EnvAPIKey = envNamespace + "API_KEY"

	EnvDNSAPIToken  = envNamespace + "DNS_API_TOKEN"
	EnvZoneAPIToken = envNamespace + "ZONE_API_TOKEN"

	EnvBaseURL = envNamespace + "BASE_URL"

	EnvTTL                = envNamespace + "TTL"
	EnvPropagationTimeout = envNamespace + "PROPAGATION_TIMEOUT"
	EnvPollingInterval    = envNamespace + "POLLING_INTERVAL"
	EnvHTTPTimeout        = envNamespace + "HTTP_TIMEOUT"
)

const (
	altEnvNamespace = "CF_"

	altEnvEmail = altEnvNamespace + "API_EMAIL"
)

const (
	minTTL = 120
)

var _ challenge.ProviderTimeout = (*DNSProvider)(nil)

// Config is used to configure the creation of the DNSProvider.
type Config struct {
	AuthEmail string
	AuthKey   string

	AuthToken string
	ZoneToken string

	BaseURL string

	TTL                int
	PropagationTimeout time.Duration
	PollingInterval    time.Duration
	HTTPClient         *http.Client
}

// NewDefaultConfig returns a default configuration for the DNSProvider.
func NewDefaultConfig() *Config {
	return &Config{
		TTL:                env.GetOneWithFallback(EnvTTL, minTTL, strconv.Atoi, altEnvName(EnvTTL)),
		PropagationTimeout: env.GetOneWithFallback(EnvPropagationTimeout, 2*time.Minute, env.ParseSecond, altEnvName(EnvPropagationTimeout)),
		PollingInterval:    env.GetOneWithFallback(EnvPollingInterval, dns01.DefaultPollingInterval, env.ParseSecond, altEnvName(EnvPollingInterval)),
		HTTPClient: &http.Client{
			Timeout: env.GetOneWithFallback(EnvHTTPTimeout, 30*time.Second, env.ParseSecond, altEnvName(EnvHTTPTimeout)),
		},
	}
}

// DNSProvider implements the challenge.Provider interface.
type DNSProvider struct {
	client *metaClient
	config *Config

	recordIDs   map[string]string
	recordIDsMu sync.Mutex
}

// NewDNSProvider returns a DNSProvider instance configured for Cloudflare.
// Credentials must be passed in as environment variables:
//
// Either provide CLOUDFLARE_EMAIL and CLOUDFLARE_API_KEY,
// or a CLOUDFLARE_DNS_API_TOKEN.
//
// For a more paranoid setup, provide CLOUDFLARE_DNS_API_TOKEN and CLOUDFLARE_ZONE_API_TOKEN.
//
// The email and API key should be avoided, if possible.
// Instead, set up an API token with both Zone:Read and DNS:Edit permission, and pass the CLOUDFLARE_DNS_API_TOKEN environment variable.
// You can split the Zone:Read and DNS:Edit permissions across multiple API tokens:
// in this case pass both CLOUDFLARE_ZONE_API_TOKEN and CLOUDFLARE_DNS_API_TOKEN accordingly.
func NewDNSProvider() (*DNSProvider, error) {
	values, err := env.GetWithFallback(
		[]string{EnvEmail, altEnvEmail},
		[]string{EnvAPIKey, altEnvName(EnvAPIKey)},
	)
	if err != nil {
		var errT error
		values, errT = env.GetWithFallback(
			[]string{EnvDNSAPIToken, altEnvName(EnvDNSAPIToken)},
			[]string{EnvZoneAPIToken, altEnvName(EnvZoneAPIToken), EnvDNSAPIToken, altEnvName(EnvDNSAPIToken)},
		)
		if errT != nil {
			//nolint:errorlint
			return nil, fmt.Errorf("cloudflare: %v or %v", err, errT)
		}
	}

	config := NewDefaultConfig()
	config.AuthEmail = values[EnvEmail]
	config.AuthKey = values[EnvAPIKey]
	config.AuthToken = values[EnvDNSAPIToken]
	config.ZoneToken = values[EnvZoneAPIToken]
	config.BaseURL = env.GetOrFile(EnvBaseURL)

	return NewDNSProviderConfig(config)
}

// NewDNSProviderConfig return a DNSProvider instance configured for Cloudflare.
func NewDNSProviderConfig(config *Config) (*DNSProvider, error) {
	if config == nil {
		return nil, errors.New("cloudflare: the configuration of the DNS provider is nil")
	}

	if config.TTL < minTTL {
		return nil, fmt.Errorf("cloudflare: invalid TTL, TTL (%d) must be greater than %d", config.TTL, minTTL)
	}

	client, err := newClient(config)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %w", err)
	}

	return &DNSProvider{
		client:    client,
		config:    config,
		recordIDs: make(map[string]string),
	}, nil
}

// Timeout returns the timeout and interval to use when checking for DNS propagation.
// Adjusting here to cope with spikes in propagation times.
func (d *DNSProvider) Timeout() (timeout, interval time.Duration) {
	return d.config.PropagationTimeout, d.config.PollingInterval
}

// Present creates a TXT record to fulfill the dns-01 challenge.
func (d *DNSProvider) Present(domain, token, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)

	ctx := context.Background()

	authZone, err := dns01.FindZoneByFqdn(info.EffectiveFQDN)
	if err != nil {
		return fmt.Errorf("cloudflare: could not find zone for domain %q: %w", domain, err)
	}

	zoneID, err := d.client.ZoneIDByName(ctx, authZone)
	if err != nil {
		return fmt.Errorf("cloudflare: failed to find zone %s: %w", authZone, err)
	}

	dnsRecord := internal.Record{
		Type:    "TXT",
		Name:    dns01.UnFqdn(info.EffectiveFQDN),
		Content: `"` + info.Value + `"`,
		TTL:     d.config.TTL,
	}

	response, err := d.client.CreateDNSRecord(ctx, zoneID, dnsRecord)
	if err != nil {
		return fmt.Errorf("cloudflare: failed to create TXT record: %w", err)
	}

	d.recordIDsMu.Lock()
	d.recordIDs[token] = response.ID
	d.recordIDsMu.Unlock()

	log.Infof("cloudflare: new record for %s, ID %s", domain, response.ID)

	return nil
}

// CleanUp removes the TXT record matching the specified parameters.
func (d *DNSProvider) CleanUp(domain, token, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)

	authZone, err := dns01.FindZoneByFqdn(info.EffectiveFQDN)
	if err != nil {
		return fmt.Errorf("cloudflare: could not find zone for domain %q: %w", domain, err)
	}

	zoneID, err := d.client.ZoneIDByName(context.Background(), authZone)
	if err != nil {
		return fmt.Errorf("cloudflare: failed to find zone %s: %w", authZone, err)
	}

	// get the record's unique ID from when we created it
	d.recordIDsMu.Lock()
	recordID, ok := d.recordIDs[token]
	d.recordIDsMu.Unlock()
	if !ok {
		return fmt.Errorf("cloudflare: unknown record ID for '%s'", info.EffectiveFQDN)
	}

	err = d.client.DeleteDNSRecord(context.Background(), zoneID, recordID)
	if err != nil {
		log.Printf("cloudflare: failed to delete TXT record: %v", err)
	}

	// Delete record ID from map
	d.recordIDsMu.Lock()
	delete(d.recordIDs, token)
	d.recordIDsMu.Unlock()

	return nil
}

func altEnvName(v string) string {
	return strings.ReplaceAll(v, envNamespace, altEnvNamespace)
}
