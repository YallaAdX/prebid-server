package exchange

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// callTimer records when each phase of one bidder HTTP call happened, for
// bidresponse.ext.bidotter_calltiming. It is separate from addClientTrace,
// which only feeds metrics and is skipped when connection metrics are off.
//
// Every field is an offset in nanoseconds from start, plus 1 so 0 means "did
// not happen". They are atomics because the transport can call a hook after
// the call has returned: a dial for a cancelled request keeps going so the
// connection can be pooled, and its ConnectDone and TLSHandshakeDone arrive
// late. A timer is never reused, so a late write lands in a value nobody reads.
type callTimer struct {
	start time.Time

	getConn, gotConn, wrote, firstByte, done atomic.Int64
	dnsStart, dnsDone                        atomic.Int64
	connStart, connDone                      atomic.Int64
	tlsStart, tlsDone                        atomic.Int64
	reused                                   atomic.Bool
}

func newCallTimer(start time.Time) *callTimer { return &callTimer{start: start} }

func (t *callTimer) mark(v *atomic.Int64) { v.Store(int64(time.Since(t.start)) + 1) }

// markFirst keeps the first time only. A new connection that races a pooled
// one can call ConnectStart or DNSStart for a dial this request does not use.
func (t *callTimer) markFirst(v *atomic.Int64) {
	v.CompareAndSwap(0, int64(time.Since(t.start))+1)
}

// trace returns the hooks, to compose with any trace already on ctx.
func (t *callTimer) trace(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) { t.markFirst(&t.getConn) },
		GotConn: func(info httptrace.GotConnInfo) {
			t.reused.Store(info.Reused)
			t.markFirst(&t.gotConn)
		},
		DNSStart:             func(httptrace.DNSStartInfo) { t.markFirst(&t.dnsStart) },
		DNSDone:              func(httptrace.DNSDoneInfo) { t.markFirst(&t.dnsDone) },
		ConnectStart:         func(string, string) { t.markFirst(&t.connStart) },
		ConnectDone:          func(string, string, error) { t.markFirst(&t.connDone) },
		TLSHandshakeStart:    func() { t.markFirst(&t.tlsStart) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.markFirst(&t.tlsDone) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { t.markFirst(&t.wrote) },
		GotFirstResponseByte: func() { t.markFirst(&t.firstByte) },
	})
}

// span is b-a in microseconds, or 0 when either end did not happen or the two
// are out of order.
func span(a, b *atomic.Int64) int64 {
	x, y := a.Load(), b.Load()
	if x == 0 || y == 0 || y < x {
		return 0
	}
	return (y - x) / int64(time.Microsecond)
}

// snapshot reads the timer once the body is read. A connect or TLS phase still
// running then (a late dial, see callTimer) has no end yet and reads as 0.
func (t *callTimer) snapshot(status int) openrtb_ext.ExtBidderCallTiming {
	t.mark(&t.done)
	var zero atomic.Int64
	zero.Store(1)
	return openrtb_ext.ExtBidderCallTiming{
		Status:     status,
		Reused:     t.reused.Load(),
		ConnWaitUS: span(&t.getConn, &t.gotConn),
		DNSUS:      span(&t.dnsStart, &t.dnsDone),
		ConnectUS:  span(&t.connStart, &t.connDone),
		TLSUS:      span(&t.tlsStart, &t.tlsDone),
		TTFBUS:     span(&t.wrote, &t.firstByte),
		BodyUS:     span(&t.firstByte, &t.done),
		TotalUS:    span(&zero, &t.done),
	}
}
