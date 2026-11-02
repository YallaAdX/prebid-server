// Package egress lets a host choose the source of a request's outbound connections.
//
// A connection is tagged with a DSCP value on its socket, and a rule on the host maps
// that value to a source IP. The value comes from the X-Egress-Mark request header. A
// host that uses this must strip the header from untrusted traffic at its edge.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"
)

const (
	// Header carries the mark of an auction's bidder calls.
	Header = "X-Egress-Mark"
	// DefaultMarkEnv sets the mark of connections that carry none. 1 is the only mark
	// that may be applied to a connection over IPv6, where it is simply left off.
	DefaultMarkEnv = "EGRESS_DEFAULT_MARK"

	maxMark = 63
)

// ErrNotIPv4 refuses a dedicated connection over IPv6: host rules are IPv4 only, and
// the connection would leave from a shared address.
var ErrNotIPv4 = errors.New("egress: a dedicated mark is IPv4 only")

type markKey struct{}

// WithMark returns a context whose bidder calls carry mark. Zero returns ctx.
func WithMark(ctx context.Context, mark int) context.Context {
	if mark == 0 {
		return ctx
	}
	return context.WithValue(ctx, markKey{}, mark)
}

// MarkFrom returns the mark set by WithMark, or zero.
func MarkFrom(ctx context.Context) int {
	m, _ := ctx.Value(markKey{}).(int)
	return m
}

// MarkFromHeader reads Header. An absent header is zero. A bad one is an error, never
// a silent zero: that would send a dedicated auction from a shared address.
func MarkFromHeader(h http.Header) (int, error) {
	v := h.Get(Header)
	if v == "" {
		return 0, nil
	}
	m, err := strconv.Atoi(v)
	if err != nil || m < 1 || m > maxMark {
		return 0, fmt.Errorf("invalid %s header", Header)
	}
	return m, nil
}

// DefaultMark reads DefaultMarkEnv. Unset is zero, which leaves dialers untouched.
func DefaultMark() (int, error) {
	v := os.Getenv(DefaultMarkEnv)
	if v == "" {
		return 0, nil
	}
	m, err := strconv.Atoi(v)
	if err != nil || m < 0 || m > maxMark {
		return 0, fmt.Errorf("invalid %s", DefaultMarkEnv)
	}
	return m, nil
}

// Control returns a net.Dialer.Control applying mark, or nil for zero.
func Control(mark int) func(network, address string, c syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(network, _ string, c syscall.RawConn) error {
		if network != "tcp4" {
			if mark > 1 {
				return ErrNotIPv4
			}
			return nil
		}
		var serr error
		if err := c.Control(func(fd uintptr) {
			// IP_TOS is DSCP in its top six bits.
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, mark<<2)
		}); err != nil {
			return err
		}
		return serr
	}
}

// Transport sends a request with a mark in its context over a transport of that mark.
// A connection's source is fixed when dialled and http.Transport pools by host alone,
// so each mark needs its own pool. Requests with no mark use base, unchanged.
type Transport struct {
	base   *http.Transport
	dialer net.Dialer
	wrap   func(*net.Dialer) func(context.Context, string, string) (net.Conn, error)
	marked sync.Map // int -> *http.Transport
}

// NewTransport wraps base. dialer is base's dialer, and wrap is how base's DialContext
// was built from it.
func NewTransport(base *http.Transport, dialer net.Dialer, wrap func(*net.Dialer) func(context.Context, string, string) (net.Conn, error)) *Transport {
	return &Transport{base: base, dialer: dialer, wrap: wrap}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	m := MarkFrom(req.Context())
	if m == 0 {
		return t.base.RoundTrip(req)
	}
	return t.forMark(m).RoundTrip(req)
}

func (t *Transport) forMark(m int) *http.Transport {
	if tr, ok := t.marked.Load(m); ok {
		return tr.(*http.Transport)
	}
	d := t.dialer
	d.Control = Control(m)
	tr := t.base.Clone()
	tr.DialContext = t.wrap(&d)
	actual, _ := t.marked.LoadOrStore(m, tr)
	return actual.(*http.Transport)
}

// CloseIdleConnections closes idle connections of every pool.
func (t *Transport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
	t.marked.Range(func(_, tr any) bool {
		tr.(*http.Transport).CloseIdleConnections()
		return true
	})
}
