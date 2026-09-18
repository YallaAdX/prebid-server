package bidotter

import (
	"fmt"
	"net/http"
	"text/template"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/macros"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

type adapter struct {
	endpointTemplate *template.Template
}

// Builder builds a new instance of the BidOtter adapter for the given bidder with the given config.
func Builder(bidderName openrtb_ext.BidderName, config config.Adapter, server config.Server) (adapters.Bidder, error) {
	endpointTemplate, err := template.New("endpointTemplate").Parse(config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("unable to parse endpoint url template: %v", err)
	}

	return &adapter{
		endpointTemplate: endpointTemplate,
	}, nil
}

func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	// The BidOtter exchange speaks OpenRTB 2.6 natively, so the request is
	// forwarded as-is; the placementId param only selects the endpoint path.
	var bidderExt adapters.ExtImpBidder
	if err := jsonutil.Unmarshal(request.Imp[0].Ext, &bidderExt); err != nil {
		return nil, []error{&errortypes.BadInput{
			Message: fmt.Sprintf("Invalid imp.ext for impression index 0. Error Information: %s", err.Error()),
		}}
	}
	var impExt openrtb_ext.ImpExtBidotter
	if err := jsonutil.Unmarshal(bidderExt.Bidder, &impExt); err != nil {
		return nil, []error{&errortypes.BadInput{
			Message: fmt.Sprintf("Invalid imp.ext.bidder for impression index 0. Error Information: %s", err.Error()),
		}}
	}
	if impExt.PlacementID == "" {
		return nil, []error{&errortypes.BadInput{
			Message: "Missing required parameter placementId",
		}}
	}

	endpoint, err := a.buildEndpointURL(&impExt)
	if err != nil {
		return nil, []error{err}
	}

	requestJSON, err := jsonutil.Marshal(request)
	if err != nil {
		return nil, []error{err}
	}

	headers := http.Header{}
	headers.Add("Content-Type", "application/json;charset=utf-8")
	headers.Add("Accept", "application/json")

	requestData := &adapters.RequestData{
		Method:  "POST",
		Uri:     endpoint,
		Body:    requestJSON,
		Headers: headers,
		ImpIDs:  openrtb_ext.GetImpIDs(request.Imp),
	}

	return []*adapters.RequestData{requestData}, nil
}

func (a *adapter) buildEndpointURL(params *openrtb_ext.ImpExtBidotter) (string, error) {
	endpointParams := macros.EndpointTemplateParams{PlacementID: params.PlacementID}
	return macros.ResolveMacros(a.endpointTemplate, endpointParams)
}

func (a *adapter) MakeBids(request *openrtb2.BidRequest, requestData *adapters.RequestData, responseData *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if adapters.IsResponseStatusCodeNoContent(responseData) {
		return nil, nil
	}

	if err := adapters.CheckResponseStatusCodeForErrors(responseData); err != nil {
		return nil, []error{err}
	}

	var response openrtb2.BidResponse
	if err := jsonutil.Unmarshal(responseData.Body, &response); err != nil {
		return nil, []error{err}
	}

	bidResponse := adapters.NewBidderResponseWithBidsCapacity(len(request.Imp))
	bidResponse.Currency = response.Cur

	var errors []error
	for _, seatBid := range response.SeatBid {
		for i := range seatBid.Bid {
			bidType, err := getMediaTypeForBid(seatBid.Bid[i], request.Imp)
			if err != nil {
				errors = append(errors, err)
				continue
			}
			bidResponse.Bids = append(bidResponse.Bids, &adapters.TypedBid{
				Bid:     &seatBid.Bid[i],
				BidType: bidType,
			})
		}
	}
	return bidResponse, errors
}

func getMediaTypeForBid(bid openrtb2.Bid, requestImps []openrtb2.Imp) (openrtb_ext.BidType, error) {
	// The exchange stamps mtype on every bid; this is the authoritative signal.
	switch bid.MType {
	case openrtb2.MarkupBanner:
		return openrtb_ext.BidTypeBanner, nil
	case openrtb2.MarkupVideo:
		return openrtb_ext.BidTypeVideo, nil
	case openrtb2.MarkupAudio:
		return openrtb_ext.BidTypeAudio, nil
	case openrtb2.MarkupNative:
		return openrtb_ext.BidTypeNative, nil
	}

	// Fallback for legacy responses without mtype: infer from the imp the bid
	// answers, in the same media-type precedence the exchange itself uses.
	for _, imp := range requestImps {
		if imp.ID == bid.ImpID {
			switch {
			case imp.Video != nil:
				return openrtb_ext.BidTypeVideo, nil
			case imp.Banner != nil:
				return openrtb_ext.BidTypeBanner, nil
			case imp.Audio != nil:
				return openrtb_ext.BidTypeAudio, nil
			case imp.Native != nil:
				return openrtb_ext.BidTypeNative, nil
			}
		}
	}
	return "", &errortypes.BadServerResponse{
		Message: fmt.Sprintf("Unable to determine media type for bid with impid: \"%s\"", bid.ImpID),
	}
}
