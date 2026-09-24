/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package transport

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	clientgofeaturegate "k8s.io/client-go/features"
	"k8s.io/client-go/tools/metrics"
	"k8s.io/klog/v2"
)

// TlsTransportCache caches TLS http.RoundTrippers different configurations. The
// same RoundTripper will be returned for configs with identical TLS options If
// the config has no custom TLS options, http.DefaultTransport is returned.
type tlsTransportCache struct {
	mu               sync.Mutex
	transports       map[tlsCacheKey]weak.Pointer[trackedTransport] // GC-enabled
	strongTransports map[tlsCacheKey]http.RoundTripper              // GC-disabled
}

const idleConnsPerHost = 25

var tlsCache = newTLSCache()

func newTLSCache() *tlsTransportCache {
	return &tlsTransportCache{
		transports:       make(map[tlsCacheKey]weak.Pointer[trackedTransport]),
		strongTransports: make(map[tlsCacheKey]http.RoundTripper),
	}
}

type tlsCacheKey struct {
	insecure           bool
	caData             string
	caFile             string
	certData           string
	keyData            string `datapolicy:"security-key"`
	certFile           string
	keyFile            string
	serverName         string
	nextProtos         string
	disableCompression bool
	// these functions are wrapped to allow them to be used as map keys
	getCert *GetCertHolder
	dial    *DialHolder
}

func (t tlsCacheKey) String() string {
	keyText := "<none>"
	if len(t.keyData) > 0 {
		keyText = "<redacted>"
	}
	return fmt.Sprintf("insecure:%v, caData:%#v, caFile:%s, certData:%#v, keyData:%s, serverName:%s, disableCompression:%t, getCert:%p, dial:%p",
		t.insecure, t.caData, t.caFile, t.certData, keyText, t.serverName, t.disableCompression, t.getCert, t.dial)
}

func (c *tlsTransportCache) get(config *Config) (http.RoundTripper, error) {
	if config.ConnectionPoolSize > 1 {
		if err := loadTLSFiles(config); err != nil {
			return nil, err
		}
		metrics.TransportCreateCalls.Increment("uncacheable")
		transports := make([]http.RoundTripper, 0, config.ConnectionPoolSize)
		cancels := make([]context.CancelFunc, 0, config.ConnectionPoolSize)
		for i := 0; i < config.ConnectionPoolSize; i++ {
			rt, cancel, err := c.createSingleTransport(config, true)
			if err != nil {
				for _, fn := range cancels {
					if fn != nil {
						fn()
					}
				}
				return nil, err
			}
			transports = append(transports, rt)
			cancels = append(cancels, cancel)
		}
		if clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.ClientsAllowTLSCacheGC) {
			for i, cancel := range cancels {
				if cancel != nil {
					transportWithGC := &trackedTransport{rt: transports[i]}
					transportCertRotationGCCalls := metrics.TransportCertRotationGCCalls
					runtime.AddCleanup(transportWithGC, func(_ struct{}) {
						cancel()
						transportCertRotationGCCalls.Increment()
					}, struct{}{})
					transports[i] = transportWithGC
				}
			}
		}
		return newConnectionPoolRoundTripper(transports), nil
	}

	key, canCache, err := tlsConfigKey(config)
	if err != nil {
		return nil, err
	}

	if canCache {
		// Ensure we only create a single transport for the given TLS options
		c.mu.Lock()
		defer c.mu.Unlock()
		defer func() { metrics.TransportCacheEntries.Observe(c.lenLocked()) }()

		// See if we already have a custom transport for this config
		if t, ok := c.getLocked(key); ok {
			if t != nil {
				metrics.TransportCreateCalls.Increment("hit")
				return t, nil
			}
			metrics.TransportCreateCalls.Increment("miss-gc")
		} else {
			metrics.TransportCreateCalls.Increment("miss")
		}
	} else {
		metrics.TransportCreateCalls.Increment("uncacheable")
	}

	transport, cancel, err := c.createSingleTransport(config, false)
	if err != nil {
		return nil, err
	}
	if transport == http.DefaultTransport {
		return http.DefaultTransport, nil
	}

	if !canCache && cancel == nil {
		return transport, nil // uncacheable config with no cert rotation - nothing to GC
	}

	if !clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.ClientsAllowTLSCacheGC) {
		if canCache {
			c.strongTransports[key] = transport
		}
		return transport, nil // cancel is intentionally discarded and the cert rotation go routine leaks
	}

	transportWithGC := &trackedTransport{rt: transport}

	if cancel != nil {
		// capture metric as local var so that cleanups do not influence other tests via globals
		transportCertRotationGCCalls := metrics.TransportCertRotationGCCalls
		runtime.AddCleanup(transportWithGC, func(_ struct{}) {
			cancel()
			transportCertRotationGCCalls.Increment()
		}, struct{}{})
	}

	if canCache {
		wp := weak.Make(transportWithGC)
		c.transports[key] = wp
		// capture metrics as local vars so that cleanups do not influence other tests via globals
		transportCacheGCCalls := metrics.TransportCacheGCCalls
		transportCacheEntries := metrics.TransportCacheEntries
		runtime.AddCleanup(transportWithGC, func(key tlsCacheKey) {
			c.mu.Lock()
			defer c.mu.Unlock()

			// make sure we only delete the weak pointer created by this specific setLocked call
			if c.transports[key] != wp {
				transportCacheGCCalls.Increment("skipped")
				return
			}
			delete(c.transports, key)
			transportCacheGCCalls.Increment("deleted")
			transportCacheEntries.Observe(c.lenLocked())
		}, key)
	}

	return transportWithGC, nil
}

func (c *tlsTransportCache) createSingleTransport(config *Config, forceCustomTransport bool) (http.RoundTripper, context.CancelFunc, error) {
	// Get the TLS options for this client config
	tlsConfig, err := TLSConfigFor(config)
	if err != nil {
		return nil, nil, err
	}
	// The options didn't require a custom TLS config
	if !forceCustomTransport && tlsConfig == nil && config.DialHolder == nil && config.Proxy == nil {
		return http.DefaultTransport, nil, nil
	}

	var dial func(ctx context.Context, network, address string) (net.Conn, error)
	if config.DialHolder != nil {
		dial = config.DialHolder.Dial
	} else {
		dial = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}

	// If we use are reloading files, we need to handle certificate rotation properly
	// TODO(jackkleeman): We can also add rotation here when config.HasCertCallback() is true
	var cancel context.CancelFunc
	if config.TLS.ReloadTLSFiles && tlsConfig != nil && tlsConfig.GetClientCertificate != nil {
		// The TLS cache is a singleton, so sharing the same name for all of its
		// background activity seems okay.
		logger := klog.Background().WithName("tls-transport-cache")
		dynamicCertDialer := certRotatingDialer(logger, tlsConfig.GetClientCertificate, dial)
		tlsConfig.GetClientCertificate = dynamicCertDialer.GetClientCertificate
		dial = dynamicCertDialer.connDialer.DialContext
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		go dynamicCertDialer.run(ctx.Done())
	}

	proxy := http.ProxyFromEnvironment
	if config.Proxy != nil {
		proxy = config.Proxy
	}

	httpTransport := utilnet.SetTransportDefaults(&http.Transport{
		Proxy:               proxy,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     tlsConfig,
		MaxIdleConnsPerHost: idleConnsPerHost,
		DialContext:         dial,
		DisableCompression:  config.DisableCompression,
	})
	var transport http.RoundTripper = httpTransport

	if config.TLS.ReloadCAFiles && tlsConfig != nil && tlsConfig.RootCAs != nil && len(config.TLS.CAFile) > 0 {
		transport = newAtomicTransportHolder(config.TLS.CAFile, config.TLS.CAData, httpTransport)
	}

	return transport, cancel, nil
}

type pooledTransport struct {
	rt       http.RoundTripper
	inFlight atomic.Int64
}

// connectionPoolRoundTripper distributes HTTP requests across a fixed pool of
// independent http.RoundTrippers using Power-of-Two-Choices (P2C) based on the
// number of currently in-flight requests.
type connectionPoolRoundTripper struct {
	mu         sync.Mutex
	transports []*pooledTransport
	randIntn   func(n int) int
}

var _ http.RoundTripper = &connectionPoolRoundTripper{}
var _ utilnet.RoundTripperWrapper = &connectionPoolRoundTripper{}

func newConnectionPoolRoundTripper(transports []http.RoundTripper) *connectionPoolRoundTripper {
	pooled := make([]*pooledTransport, len(transports))
	for i, rt := range transports {
		pooled[i] = &pooledTransport{rt: rt}
	}
	return &connectionPoolRoundTripper{
		transports: pooled,
		randIntn:   rand.IntN,
	}
}

func (p *connectionPoolRoundTripper) pick() *pooledTransport {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.transports)
	if n == 0 {
		return nil
	}
	if n == 1 {
		chosen := p.transports[0]
		chosen.inFlight.Add(1)
		return chosen
	}

	randFn := p.randIntn
	if randFn == nil {
		randFn = rand.IntN
	}

	i := randFn(n)
	j := randFn(n - 1)
	if j >= i {
		j++
	}

	t1 := p.transports[i]
	t2 := p.transports[j]
	chosen := t1
	if t2.inFlight.Load() < t1.inFlight.Load() {
		chosen = t2
	}
	chosen.inFlight.Add(1)
	return chosen
}

func (p *connectionPoolRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	chosen := p.pick()
	if chosen == nil {
		return nil, fmt.Errorf("connectionPoolRoundTripper has no available transports")
	}
	transferredToBody := false
	defer func() {
		if !transferredToBody {
			chosen.inFlight.Add(-1)
		}
	}()
	resp, err := chosen.rt.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	transferredToBody = true
	onClose := func() {
		chosen.inFlight.Add(-1)
	}
	if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
		resp.Body = &inFlightReadWriteCloser{
			inFlightBody: inFlightBody{
				ReadCloser: resp.Body,
				onClose:    onClose,
			},
			Writer: rwc,
		}
	} else {
		resp.Body = &inFlightBody{
			ReadCloser: resp.Body,
			onClose:    onClose,
		}
	}
	return resp, nil
}

func (p *connectionPoolRoundTripper) CloseIdleConnections() {
	for _, t := range p.transports {
		utilnet.CloseIdleConnectionsFor(t.rt)
	}
}

func (p *connectionPoolRoundTripper) WrappedRoundTripper() http.RoundTripper {
	if len(p.transports) == 0 {
		return nil
	}
	return p.transports[0].rt
}

func (p *connectionPoolRoundTripper) CancelRequest(req *http.Request) {
	for _, t := range p.transports {
		tryCancelRequest(t.rt, req)
	}
}

type inFlightBody struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
}

func (b *inFlightBody) Close() error {
	defer b.once.Do(b.onClose)
	return b.ReadCloser.Close()
}

type inFlightReadWriteCloser struct {
	inFlightBody
	io.Writer
}

func (c *tlsTransportCache) getLocked(key tlsCacheKey) (http.RoundTripper, bool) {
	if !clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.ClientsAllowTLSCacheGC) {
		v, ok := c.strongTransports[key]
		return v, ok
	}

	wp, ok := c.transports[key]
	if !ok {
		return nil, false
	}

	v := wp.Value()

	if v == nil { // avoid typed nil
		return nil, true // key exists but value has been garbage collected
	}

	return v, true
}

func (c *tlsTransportCache) lenLocked() int {
	if !clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.ClientsAllowTLSCacheGC) {
		return len(c.strongTransports)
	}
	return len(c.transports)
}

// trackedTransport wraps an http.RoundTripper to serve as the weak.Pointer
// target in the TLS transport cache. Dropping all references to this object
// triggers GC cleanup of the cache entry and any cert rotation goroutine.
type trackedTransport struct {
	rt http.RoundTripper
}

var _ http.RoundTripper = &trackedTransport{}
var _ utilnet.RoundTripperWrapper = &trackedTransport{}

func (v *trackedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return v.rt.RoundTrip(req)
}

func (v *trackedTransport) WrappedRoundTripper() http.RoundTripper {
	return v.rt
}

// tlsConfigKey returns a unique key for tls.Config objects returned from TLSConfigFor
func tlsConfigKey(c *Config) (tlsCacheKey, bool, error) {
	// Make sure ca/key/cert content is loaded
	if err := loadTLSFiles(c); err != nil {
		return tlsCacheKey{}, false, err
	}

	if c.Proxy != nil || c.ConnectionPoolSize > 1 {
		// cannot determine equality for functions or pooled transports
		return tlsCacheKey{}, false, nil
	}

	k := tlsCacheKey{
		insecure:           c.TLS.Insecure,
		serverName:         c.TLS.ServerName,
		nextProtos:         strings.Join(c.TLS.NextProtos, ","),
		disableCompression: c.DisableCompression,
		getCert:            c.TLS.GetCertHolder,
		dial:               c.DialHolder,
	}

	if c.TLS.ReloadTLSFiles {
		k.certFile = c.TLS.CertFile
		k.keyFile = c.TLS.KeyFile
	} else {
		k.certData = string(c.TLS.CertData)
		k.keyData = string(c.TLS.KeyData)
	}

	if c.TLS.ReloadCAFiles {
		// When reloading CA files, include CA file path in cache key instead of CA data
		// This allows the CA to be reloaded from disk on each transport creation
		k.caFile = c.TLS.CAFile
	} else {
		// When not reloading, cache the CA data directly
		k.caData = string(c.TLS.CAData)
	}

	return k, true, nil
}
