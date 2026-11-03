package exchange

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/currency"
	"github.com/prebid/prebid-server/v4/experiment/adscert"
	"github.com/prebid/prebid-server/v4/hooks/hookexecution"
	metricsConfig "github.com/prebid/prebid-server/v4/metrics/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func timedCall(t *testing.T, client *http.Client, url string) openrtb_ext.ExtBidderCallTiming {
	t.Helper()
	req, err := http.NewRequest("POST", url, nil)
	require.NoError(t, err)
	timer := newCallTimer(time.Now())
	resp, err := client.Do(req.WithContext(timer.trace(context.Background())))
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return timer.snapshot(resp.StatusCode)
}

func TestCallTimerColdThenReused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := srv.Client()

	cold := timedCall(t, client, srv.URL)
	assert.Equal(t, http.StatusNoContent, cold.Status)
	assert.False(t, cold.Reused)
	assert.Positive(t, cold.ConnectUS, "a new connection has a connect phase")
	assert.Positive(t, cold.TLSUS, "a new TLS connection has a handshake")
	assert.GreaterOrEqual(t, cold.ConnWaitUS, cold.TLSUS, "conn wait covers the dial and TLS")

	warm := timedCall(t, client, srv.URL)
	assert.True(t, warm.Reused)
	assert.Zero(t, warm.ConnectUS)
	assert.Zero(t, warm.TLSUS)
	assert.Zero(t, warm.DNSUS)
}

func TestCallTimerServerDelayIsInTTFB(t *testing.T) {
	const delay = 30 * time.Millisecond
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	got := timedCall(t, srv.Client(), srv.URL)
	assert.GreaterOrEqual(t, got.TTFBUS, delay.Microseconds(), "the bidder's think time is in TTFB")
	assert.Less(t, got.ConnectUS, delay.Microseconds(), "and not in connect")
	assert.GreaterOrEqual(t, got.TotalUS, got.TTFBUS)
}

func TestCallTimerMissingPhaseIsZero(t *testing.T) {
	timer := newCallTimer(time.Now())
	got := timer.snapshot(204)
	assert.Zero(t, got.TTFBUS, "no WroteRequest/first byte recorded")
	assert.Zero(t, got.ConnWaitUS)
	assert.GreaterOrEqual(t, got.TotalUS, int64(0), "total is measured from start even with no hooks")
}

// The bidder's real call path returns one timing per call that got a response.
func TestRequestBidReturnsCallTiming(t *testing.T) {
	server := httptest.NewServer(mockHandler(200, "getBody", `{"bid":false}`))
	defer server.Close()

	bidderImpl := &goodSingleBidder{
		httpRequest: &adapters.RequestData{Method: "POST", Uri: server.URL, Body: []byte(`{}`), Headers: http.Header{}},
		bidResponse: &adapters.BidderResponse{},
	}
	bidder := AdaptBidder(bidderImpl, server.Client(), &config.Configuration{}, &metricsConfig.NilMetricsEngine{}, openrtb_ext.BidderAppnexus, &config.DebugInfo{}, "")
	bidderReq := BidderRequest{BidRequest: &openrtb2.BidRequest{Imp: []openrtb2.Imp{{ID: "impId"}}}, BidderName: "test"}
	_, extra, _ := bidder.requestBid(context.Background(), bidderReq, currency.NewRateConverter(&http.Client{}, time.Duration(1), "", time.Duration(0)).Rates(),
		&adapters.ExtraRequestInfo{}, &adscert.NilSigner{}, bidRequestOptions{}, openrtb_ext.ExtAlternateBidderCodes{}, &hookexecution.EmptyHookExecutor{}, nil)

	require.Len(t, extra.callTimings, 1)
	got := extra.callTimings[0]
	assert.Equal(t, 200, got.Status)
	assert.False(t, got.Reused)
	assert.Positive(t, got.ConnectUS)
	assert.Positive(t, got.TotalUS)
}

func TestBidderCallTimingInResponseExt(t *testing.T) {
	timing := openrtb_ext.ExtBidderCallTiming{Status: 204, Reused: true, TTFBUS: 41000, BodyUS: 50, TotalUS: 41200}
	ext := openrtb_ext.ExtBidResponse{
		ResponseTimeMillis: map[openrtb_ext.BidderName]int{"pubmatic": 42},
		BidderCallTiming:   map[openrtb_ext.BidderName][]openrtb_ext.ExtBidderCallTiming{"pubmatic": {timing}},
	}
	raw, err := json.Marshal(ext)
	require.NoError(t, err)
	assert.JSONEq(t, `{"responsetimemillis":{"pubmatic":42},"bidotter_calltiming":{"pubmatic":[
		{"status":204,"reused":true,"conn_wait_us":0,"dns_us":0,"connect_us":0,"tls_us":0,"ttfb_us":41000,"body_us":50,"total_us":41200}]}}`, string(raw))

	empty, err := json.Marshal(openrtb_ext.ExtBidResponse{})
	require.NoError(t, err)
	assert.NotContains(t, string(empty), "bidotter_calltiming", "absent when no call got a response")
}
