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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	clientgofeaturegate "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	testingclock "k8s.io/utils/clock/testing"
)

type fakePoolMemberRT struct {
	calls       int
	idleClosed  int
	err         error
	upgradeBody bool
}

func (f *fakePoolMemberRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.upgradeBody {
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: &fakeReadWriteCloser{}}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func (f *fakePoolMemberRT) CloseIdleConnections() {
	f.idleClosed++
}

type fakeReadWriteCloser struct {
	strings.Builder
}

func (f *fakeReadWriteCloser) Read(p []byte) (int, error) { return 0, io.EOF }
func (f *fakeReadWriteCloser) Close() error               { return nil }

func newFakePool(n int) (*connectionPool, []*fakePoolMemberRT) {
	fakes := make([]*fakePoolMemberRT, n)
	rts := make([]http.RoundTripper, n)
	for i := range n {
		fakes[i] = &fakePoolMemberRT{}
		rts[i] = fakes[i]
	}
	return newConnectionPool(rts), fakes
}

func newPoolTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func requireInFlight(t *testing.T, pool *connectionPool, want ...int64) {
	t.Helper()
	for i, m := range pool.members {
		if got := m.inFlight.Load(); got != want[i] {
			t.Fatalf("member %d: expected %d requests in flight, got %d", i, want[i], got)
		}
	}
}

func TestConnectionPoolPicksLessLoadedMember(t *testing.T) {
	pool, fakes := newFakePool(3)
	pool.members[0].inFlight.Store(2)
	pool.members[1].inFlight.Store(1)
	// intn always returning 0 makes the pool compare members 0 and 1.
	pool.intn = func(int) int { return 0 }

	resp, err := pool.RoundTrip(newPoolTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if fakes[1].calls != 1 {
		t.Fatalf("expected the request to go to member 1, got calls %d/%d/%d", fakes[0].calls, fakes[1].calls, fakes[2].calls)
	}
	requireInFlight(t, pool, 2, 2, 0)
}

func TestConnectionPoolInFlightAccounting(t *testing.T) {
	pool, fakes := newFakePool(2)
	pool.intn = func(int) int { return 0 } // always pick member 0 (ties go to the first candidate)

	// A request is in flight until its body is closed. Closing twice decrements once.
	resp, err := pool.RoundTrip(newPoolTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	requireInFlight(t, pool, 1, 0)
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	requireInFlight(t, pool, 0, 0)

	// Reading the body to EOF also ends the request.
	resp, err = pool.RoundTrip(newPoolTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	requireInFlight(t, pool, 0, 0)
	_ = resp.Body.Close()
	requireInFlight(t, pool, 0, 0)

	// A failed request is not in flight.
	fakes[0].err = errors.New("connection refused")
	if _, err := pool.RoundTrip(newPoolTestRequest(t)); err == nil {
		t.Fatal("expected an error")
	}
	requireInFlight(t, pool, 0, 0)
}

func TestConnectionPoolKeepsUpgradedBodyWritable(t *testing.T) {
	pool, fakes := newFakePool(2)
	fakes[0].upgradeBody = true
	fakes[1].upgradeBody = true

	resp, err := pool.RoundTrip(newPoolTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("expected body to implement io.ReadWriteCloser, got %T", resp.Body)
	}
	if _, err := rwc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = rwc.Close()
	requireInFlight(t, pool, 0, 0)
}

func TestConnectionPoolSpreadsLongRunningRequests(t *testing.T) {
	const members, requests = 4, 400
	pool, _ := newFakePool(members)

	// Keep all responses open, like watches.
	for range requests {
		if _, err := pool.RoundTrip(newPoolTestRequest(t)); err != nil {
			t.Fatal(err)
		}
	}
	for i, m := range pool.members {
		// Power of two choices keeps the members within a few requests of each other.
		if got := m.inFlight.Load(); got < requests/members-10 || got > requests/members+10 {
			t.Errorf("member %d: expected about %d requests in flight, got %d", i, requests/members, got)
		}
	}
}

func TestConnectionPoolCloseIdleConnections(t *testing.T) {
	pool, fakes := newFakePool(3)
	utilnet.CloseIdleConnectionsFor(pool)
	for i, f := range fakes {
		if f.idleClosed != 1 {
			t.Errorf("member %d: expected CloseIdleConnections to be called once, got %d", i, f.idleClosed)
		}
	}
}

func dialError() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func TestConnectionPoolFailureAvoidanceFeatureGate(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			clientfeaturestesting.SetFeatureDuringTest(t, clientgofeaturegate.ClientsAllowConnectionPoolFailureAvoidance, enabled)
			pool, _ := newFakePool(2)
			if pool.avoidFailingMembers != enabled {
				t.Errorf("expected avoidFailingMembers=%v, got %v", enabled, pool.avoidFailingMembers)
			}
		})
	}
}

func TestConnectionPoolAvoidsMembersFailingToConnect(t *testing.T) {
	for _, avoid := range []bool{true, false} {
		t.Run(fmt.Sprintf("avoidFailingMembers=%v", avoid), func(t *testing.T) {
			pool, fakes := newFakePool(2)
			pool.avoidFailingMembers = avoid
			pool.clock = testingclock.NewFakePassiveClock(time.Now())
			fakes[0].err = dialError()
			// Member 1 has a request in flight, so without failure avoidance
			// the failing member 0 is always less loaded.
			pool.members[1].inFlight.Store(1)

			for range 10 {
				if resp, err := pool.RoundTrip(newPoolTestRequest(t)); err == nil {
					_ = resp.Body.Close()
				}
			}
			wantFailed := 10
			if avoid {
				wantFailed = 1
			}
			if fakes[0].calls != wantFailed {
				t.Errorf("expected %d requests to the failing member, got %d", wantFailed, fakes[0].calls)
			}
		})
	}
}

func TestConnectionPoolAvoidsOnlyConnectionFailures(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		err         error
		ctx         context.Context
		wantAvoided bool
	}{
		"connection refused":   {err: dialError(), wantAvoided: true},
		"dial timeout":         {err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, wantAvoided: true},
		"connection reset":     {err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}},
		"request failed":       {err: errors.New("stream error: stream ID 3; INTERNAL_ERROR")},
		"request was canceled": {err: dialError(), ctx: canceled},
	} {
		t.Run(name, func(t *testing.T) {
			pool, fakes := newFakePool(2)
			pool.avoidFailingMembers = true
			pool.clock = testingclock.NewFakePassiveClock(time.Now())
			pool.intn = func(int) int { return 0 } // compare members 0 and 1, ties go to member 0
			fakes[0].err = tc.err
			req := newPoolTestRequest(t)
			if tc.ctx != nil {
				req = req.WithContext(tc.ctx)
			}
			if _, err := pool.RoundTrip(req); err == nil {
				t.Fatal("expected an error")
			}
			if got := pool.failedRecently(pool.members[0]); got != tc.wantAvoided {
				t.Errorf("expected avoided=%v, got %v", tc.wantAvoided, got)
			}
		})
	}
}

func TestConnectionPoolFailureAvoidanceExpires(t *testing.T) {
	pool, fakes := newFakePool(2)
	pool.avoidFailingMembers = true
	clock := testingclock.NewFakePassiveClock(time.Now())
	pool.clock = clock
	pool.intn = func(int) int { return 0 } // compare members 0 and 1, ties go to member 0
	fakes[0].err = dialError()
	if _, err := pool.RoundTrip(newPoolTestRequest(t)); err == nil {
		t.Fatal("expected an error")
	}
	if !pool.failedRecently(pool.members[0]) {
		t.Fatal("expected member 0 to be avoided after a connection failure")
	}

	clock.SetTime(clock.Now().Add(connectionFailureBackoff))
	if pool.failedRecently(pool.members[0]) {
		t.Error("expected member 0 not to be avoided after the backoff")
	}
}

func TestConnectionPoolFailureAvoidanceClearedOnSuccess(t *testing.T) {
	pool, fakes := newFakePool(2)
	pool.avoidFailingMembers = true
	pool.clock = testingclock.NewFakePassiveClock(time.Now())
	pool.intn = func(int) int { return 0 } // compare members 0 and 1, ties go to member 0
	fakes[0].err = dialError()
	if _, err := pool.RoundTrip(newPoolTestRequest(t)); err == nil {
		t.Fatal("expected an error")
	}

	// Make member 1 avoided too, so that member 0 is picked again.
	fakes[0].err = nil
	pool.members[1].failedUntil.Store(pool.clock.Now().Add(time.Hour).UnixNano())
	resp, err := pool.RoundTrip(newPoolTestRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if fakes[0].calls != 2 {
		t.Fatalf("expected the second request to go to member 0, got %d calls", fakes[0].calls)
	}
	if pool.failedRecently(pool.members[0]) {
		t.Error("expected a successful request to clear the connection failure")
	}
}
