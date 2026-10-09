/*
Copyright The Kubernetes Authors.

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
	"crypto/x509"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	clientgofeaturegate "k8s.io/client-go/features"
	"k8s.io/utils/clock"
)

// connectionFailureBackoff is how long a pool member is avoided after it failed
// to connect to the server, unless a request to it succeeds earlier.
const connectionFailureBackoff = 200 * time.Millisecond

// connectionPool spreads requests across a fixed set of independent
// RoundTrippers ("members"), each of which keeps its own connections.
//
// For every request it picks two members at random and uses the one with
// fewer requests in flight ("power of two choices"). A request is in flight
// until its response body is closed or fully read, so long-running requests
// such as watches are taken into account.
//
// If avoidFailingMembers is set, a member that failed to connect is avoided
// for connectionFailureBackoff. Without it, a member whose connections fail
// instantly (for example because they are refused) never has requests in
// flight, so it would get more traffic than the healthy members.
type connectionPool struct {
	members             []*poolMember
	avoidFailingMembers bool
	clock               clock.PassiveClock
	// intn returns a random number in [0, n).
	intn func(n int) int
}

type poolMember struct {
	rt       rotatableCARoundTripper
	inFlight atomic.Int64
	// failedUntil is the time, in Unix nanoseconds, until which the member is
	// avoided after a connection failure, or 0.
	failedUntil atomic.Int64
}

var _ rotatableCARoundTripper = &connectionPool{}
var _ utilnet.RoundTripperWrapper = &connectionPool{}

// newConnectionPool returns a pool of the given RoundTrippers. rts must not be empty.
func newConnectionPool(rts []rotatableCARoundTripper) *connectionPool {
	members := make([]*poolMember, len(rts))
	for i, rt := range rts {
		members[i] = &poolMember{rt: rt}
	}
	return &connectionPool{
		members:             members,
		avoidFailingMembers: clientgofeaturegate.FeatureGates().Enabled(clientgofeaturegate.ClientsAllowConnectionPoolFailureAvoidance),
		clock:               clock.RealClock{},
		intn:                rand.IntN,
	}
}

// pick returns the better of two distinct, randomly chosen members.
func (p *connectionPool) pick() *poolMember {
	n := len(p.members)
	if n == 1 {
		return p.members[0]
	}
	i := p.intn(n)
	// Pick j uniformly from the other n-1 members: draw from [0, n-1) and
	// skip over i.
	j := p.intn(n - 1)
	if j >= i {
		j++
	}
	a, b := p.members[i], p.members[j]
	if p.better(b, a) {
		return b
	}
	return a
}

// better reports whether a should be used rather than b: a member that did not
// recently fail to connect is preferred, then the one with fewer requests in flight.
func (p *connectionPool) better(a, b *poolMember) bool {
	if p.avoidFailingMembers {
		if aFailed, bFailed := p.failedRecently(a), p.failedRecently(b); aFailed != bFailed {
			return bFailed
		}
	}
	return a.inFlight.Load() < b.inFlight.Load()
}

func (p *connectionPool) failedRecently(m *poolMember) bool {
	until := m.failedUntil.Load()
	return until != 0 && p.clock.Now().UnixNano() < until
}

func (p *connectionPool) RoundTrip(req *http.Request) (*http.Response, error) {
	m := p.pick()
	m.inFlight.Add(1)
	resp, err := m.rt.RoundTrip(req)
	if p.avoidFailingMembers {
		if err == nil {
			m.failedUntil.Store(0)
		} else if isConnectionFailure(err) && req.Context().Err() == nil {
			m.failedUntil.Store(p.clock.Now().Add(connectionFailureBackoff).UnixNano())
		}
	}
	if err != nil || resp == nil || resp.Body == nil {
		m.inFlight.Add(-1)
		return resp, err
	}
	resp.Body = newInFlightBody(resp.Body, func() { m.inFlight.Add(-1) })
	return resp, nil
}

// isConnectionFailure reports whether err means that connecting to the server
// failed, for example because the connection was refused or timed out.
func isConnectionFailure(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func (p *connectionPool) CloseIdleConnections() {
	for _, m := range p.members {
		m.rt.CloseIdleConnections()
	}
}

// Clone returns a new pool with clones of all members. The new pool starts
// with no requests in flight.
func (p *connectionPool) Clone() rotatableCARoundTripper {
	rts := make([]rotatableCARoundTripper, len(p.members))
	for i, m := range p.members {
		rts[i] = m.rt.Clone()
	}
	clone := newConnectionPool(rts)
	clone.avoidFailingMembers = p.avoidFailingMembers
	clone.clock = p.clock
	clone.intn = p.intn
	return clone
}

// GetRootCAs returns the root CAs of the members, which all use the same ones.
func (p *connectionPool) GetRootCAs() *x509.CertPool {
	return p.members[0].rt.GetRootCAs()
}

// SetRootCAs sets the root CAs of all members.
func (p *connectionPool) SetRootCAs(pool *x509.CertPool) {
	for _, m := range p.members {
		m.rt.SetRootCAs(pool)
	}
}

func (p *connectionPool) CancelRequest(req *http.Request) {
	for _, m := range p.members {
		tryCancelRequest(m.rt, req)
	}
}

// WrappedRoundTripper returns the first member. All members are configured
// the same way, including their root CAs, which are only ever set for all
// members at once, so it can be used to inspect e.g. the TLS config.
func (p *connectionPool) WrappedRoundTripper() http.RoundTripper {
	return p.members[0].rt
}

// inFlightBody calls done once the body is closed or fully read.
type inFlightBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *inFlightBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.done)
	}
	return n, err
}

func (b *inFlightBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

// inFlightReadWriteCloser keeps the body of "101 Switching Protocols"
// responses writable.
type inFlightReadWriteCloser struct {
	*inFlightBody
	io.Writer
}

func newInFlightBody(body io.ReadCloser, done func()) io.ReadCloser {
	b := &inFlightBody{ReadCloser: body, done: done}
	if w, ok := body.(io.Writer); ok {
		return &inFlightReadWriteCloser{inFlightBody: b, Writer: w}
	}
	return b
}
