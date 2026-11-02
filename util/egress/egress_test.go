package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkFromHeader(t *testing.T) {
	for v, want := range map[string]int{"": 0, "1": 1, "2": 2, "63": 63} {
		h := http.Header{}
		if v != "" {
			h.Set(Header, v)
		}
		got, err := MarkFromHeader(h)
		assert.NoError(t, err, v)
		assert.Equal(t, want, got, v)
	}
	for _, v := range []string{"0", "64", "-1", "x", "2 "} {
		h := http.Header{}
		h.Set(Header, v)
		_, err := MarkFromHeader(h)
		assert.Error(t, err, v)
	}
}

func TestDefaultMark(t *testing.T) {
	t.Setenv(DefaultMarkEnv, "")
	m, err := DefaultMark()
	assert.NoError(t, err)
	assert.Zero(t, m)

	t.Setenv(DefaultMarkEnv, "1")
	m, err = DefaultMark()
	assert.NoError(t, err)
	assert.Equal(t, 1, m)

	t.Setenv(DefaultMarkEnv, "nope")
	_, err = DefaultMark()
	assert.Error(t, err)
}

func TestControl(t *testing.T) {
	assert.Nil(t, Control(0), "no mark must leave the dialer untouched")
	assert.True(t, errors.Is(Control(5)("tcp6", "[::1]:1", nil), ErrNotIPv4))
	assert.NoError(t, Control(1)("tcp6", "[::1]:1", nil))

	l, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	d := net.Dialer{Timeout: 5 * time.Second, Control: Control(7)}
	c, err := d.Dial("tcp4", l.Addr().String())
	require.NoError(t, err)
	defer c.Close()

	rc, err := c.(*net.TCPConn).SyscallConn()
	require.NoError(t, err)
	var tos int
	require.NoError(t, rc.Control(func(fd uintptr) {
		tos, err = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS)
	}))
	require.NoError(t, err)
	assert.Equal(t, 7<<2, tos)
}

// A marked request uses a pool of its own, and an unmarked one the base transport.
func TestTransportPoolsPerMark(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = true
		mu.Unlock()
	}))
	defer srv.Close()

	base := &http.Transport{}
	dial := func(d *net.Dialer) func(context.Context, string, string) (net.Conn, error) { return d.DialContext }
	d := net.Dialer{Timeout: 5 * time.Second}
	base.DialContext = dial(&d)
	tr := NewTransport(base, d, dial)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	get := func(ctx context.Context) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
	}
	bg := context.Background()
	for i := 0; i < 2; i++ {
		get(bg)
		get(WithMark(bg, 2))
		get(WithMark(bg, 3))
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, conns, 3, "each mark reuses one connection of its own")
	assert.Same(t, tr.forMark(2), tr.forMark(2))
	assert.NotSame(t, tr.forMark(2), tr.forMark(3))
}
