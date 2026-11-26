package exchange

import (
	"context"
	"encoding/json"
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

// TestDebugResponseHeaders drives a real HTTP call (an empty 204 that explains itself
// only in a header, the case this fork field exists for) and checks the bidder's
// response headers reach ext.debug.httpcalls only when debug is on.
func TestDebugResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Nobid-Reason", "floor")
		w.Header().Add("X-Nobid-Reason", "geo")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	for _, tc := range []struct {
		name        string
		debug       bool
		wantHeaders bool
	}{
		{name: "debug on", debug: true, wantHeaders: true},
		{name: "debug off", debug: false, wantHeaders: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bidderImpl := &goodSingleBidder{
				httpRequest: &adapters.RequestData{
					Method:  "POST",
					Uri:     server.URL,
					Body:    []byte(`{"key":"val"}`),
					Headers: http.Header{},
				},
				bidResponse: &adapters.BidderResponse{},
			}
			bidder := AdaptBidder(bidderImpl, server.Client(), &config.Configuration{}, &metricsConfig.NilMetricsEngine{}, openrtb_ext.BidderAppnexus, &config.DebugInfo{Allow: true}, "")
			rates := currency.NewRateConverter(&http.Client{}, time.Duration(1), "", time.Duration(0)).Rates()

			seatBids, _, _ := bidder.requestBid(context.Background(),
				BidderRequest{BidRequest: &openrtb2.BidRequest{Imp: []openrtb2.Imp{{ID: "impId"}}}, BidderName: "test"},
				rates, &adapters.ExtraRequestInfo{}, &adscert.NilSigner{},
				bidRequestOptions{accountDebugAllowed: tc.debug, headerDebugAllowed: false},
				openrtb_ext.ExtAlternateBidderCodes{}, &hookexecution.EmptyHookExecutor{}, nil)
			require.Len(t, seatBids, 1)

			if !tc.wantHeaders {
				assert.Empty(t, seatBids[0].HttpCalls)
				return
			}
			require.Len(t, seatBids[0].HttpCalls, 1)
			call := seatBids[0].HttpCalls[0]
			assert.Equal(t, http.StatusNoContent, call.Status)
			assert.Equal(t, []string{"floor", "geo"}, call.ResponseHeaders["X-Nobid-Reason"])

			// The wire name the exchange reads, next to requestheaders.
			raw, err := json.Marshal(call)
			require.NoError(t, err)
			var wire map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &wire))
			assert.Contains(t, wire, "responseheaders")
		})
	}
}

func TestMakeExtResponseHeaders(t *testing.T) {
	respHeaders := http.Header{"X-Reason": {"no-fill"}}
	info := &httpCallInfo{
		request:  &adapters.RequestData{Uri: "u", Headers: http.Header{"Key1": {"v"}}},
		response: &adapters.ResponseData{StatusCode: 204, Headers: respHeaders},
	}
	ext := makeExt(info)
	assert.Equal(t, map[string][]string{"X-Reason": {"no-fill"}}, ext.ResponseHeaders)

	// A clone: the debug copy must not alias the adapter's header map.
	respHeaders.Set("X-Reason", "changed")
	assert.Equal(t, []string{"no-fill"}, ext.ResponseHeaders["X-Reason"])

	// An error status (doRequestImpl returns the response plus a BadServerResponse)
	// keeps the headers: a 429's Retry-After is the point. Status/body stay upstream's.
	info.err = assert.AnError
	info.response.StatusCode = http.StatusTooManyRequests
	info.response.Headers = http.Header{"Retry-After": {"30"}}
	onErr := makeExt(info)
	assert.Equal(t, map[string][]string{"Retry-After": {"30"}}, onErr.ResponseHeaders)
	assert.Zero(t, onErr.Status)

	// No response at all (timeout, transport error) leaves the key out of the JSON.
	info.response = nil
	raw, err := json.Marshal(makeExt(info))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "responseheaders")
}

// withoutResponseHeaders lets the upstream tests that compare whole ExtHttpCalls from a
// live httptest server keep their literals: the server's own headers (Date varies per
// run) now ride along. It checks they are there, then drops them for the comparison.
func withoutResponseHeaders(t *testing.T, calls []*openrtb_ext.ExtHttpCall) []*openrtb_ext.ExtHttpCall {
	t.Helper()
	out := make([]*openrtb_ext.ExtHttpCall, 0, len(calls))
	for _, c := range calls {
		assert.NotEmpty(t, c.ResponseHeaders["Content-Length"], "a real response should carry its headers")
		cp := *c
		cp.ResponseHeaders = nil
		out = append(out, &cp)
	}
	return out
}
