package routerrpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	math "math"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"

	"github.com/decred/dcrd/chaincfg/v2"
	"github.com/decred/dcrd/dcrutil/v2"
	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/routing"
	"github.com/decred/dcrlnd/routing/route"
	"github.com/decred/dcrlnd/tlv"
	"github.com/decred/dcrlnd/zpay32"
)

// RouterBackend contains the backend implementation of the router rpc sub
// server calls.
type RouterBackend struct {
	// MaxPaymentMAtoms is the largest payment permitted by the backend.
	MaxPaymentMAtoms lnwire.MilliAtom

	// SelfNode is the vertex of the node sending the payment.
	SelfNode route.Vertex

	// FetchChannelCapacity is a closure that we'll use the fetch the total
	// capacity of a channel to populate in responses.
	FetchChannelCapacity func(chanID uint64) (dcrutil.Amount, error)

	// FetchChannelEndpoints returns the pubkeys of both endpoints of the
	// given channel id.
	FetchChannelEndpoints func(chanID uint64) (route.Vertex,
		route.Vertex, error)

	// FindRoute is a closure that abstracts away how we locate/query for
	// routes.
	FindRoute func(source, target route.Vertex,
		amt lnwire.MilliAtom, restrictions *routing.RestrictParams,
		destTlvRecords []tlv.Record,
		finalExpiry ...uint16) (*route.Route, error)

	MissionControl MissionControl

	// ActiveNetParams are the network parameters of the primary network
	// that the route is operating on. This is necessary so we can ensure
	// that we receive payment requests that send to destinations on our
	// network.
	ActiveNetParams *chaincfg.Params

	// Tower is the ControlTower instance that is used to track pending
	// payments.
	Tower routing.ControlTower

	// MaxTotalTimelock is the maximum total time lock a route is allowed to
	// have.
	MaxTotalTimelock uint32
}

// MissionControl defines the mission control dependencies of routerrpc.
type MissionControl interface {
	// GetProbability is expected to return the success probability of a
	// payment from fromNode to toNode.
	GetProbability(fromNode, toNode route.Vertex,
		amt lnwire.MilliAtom) float64

	// ResetHistory resets the history of MissionControl returning it to a
	// state as if no payment attempts have been made.
	ResetHistory() error

	// GetHistorySnapshot takes a snapshot from the current mission control
	// state and actual probability estimates.
	GetHistorySnapshot() *routing.MissionControlSnapshot
}

// QueryRoutes attempts to query the daemons' Channel Router for a possible
// route to a target destination capable of carrying a specific amount of
// satoshis within the route's flow. The retuned route contains the full
// details required to craft and send an HTLC, also including the necessary
// information that should be present within the Sphinx packet encapsulated
// within the HTLC.
//
// TODO(roasbeef): should return a slice of routes in reality * create separate
// PR to send based on well formatted route
func (r *RouterBackend) QueryRoutes(ctx context.Context,
	in *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse, error) {

	parsePubKey := func(key string) (route.Vertex, error) {
		pubKeyBytes, err := hex.DecodeString(key)
		if err != nil {
			return route.Vertex{}, err
		}

		return route.NewVertexFromBytes(pubKeyBytes)
	}

	// Parse the hex-encoded source and target public keys into full public
	// key objects we can properly manipulate.
	targetPubKey, err := parsePubKey(in.PubKey)
	if err != nil {
		return nil, err
	}

	var sourcePubKey route.Vertex
	if in.SourcePubKey != "" {
		var err error
		sourcePubKey, err = parsePubKey(in.SourcePubKey)
		if err != nil {
			return nil, err
		}
	} else {
		// If no source is specified, use self.
		sourcePubKey = r.SelfNode
	}

	// Currently, within the bootstrap phase of the network, we limit the
	// largest payment size allotted to (2^32) - 1 mSAT or 4.29 million
	// satoshis.
	amt := dcrutil.Amount(in.Amt)
	amtMSat := lnwire.NewMAtomsFromAtoms(amt)
	if amtMSat > r.MaxPaymentMAtoms {
		return nil, fmt.Errorf("payment of %v is too large, max payment "+
			"allowed is %v", amt, r.MaxPaymentMAtoms.ToAtoms())
	}

	// Unmarshall restrictions from request.
	feeLimit := calculateFeeLimit(in.FeeLimit, amtMSat)

	ignoredNodes := make(map[route.Vertex]struct{})
	for _, ignorePubKey := range in.IgnoredNodes {
		ignoreVertex, err := route.NewVertexFromBytes(ignorePubKey)
		if err != nil {
			return nil, err
		}
		ignoredNodes[ignoreVertex] = struct{}{}
	}

	ignoredPairs := make(map[routing.DirectedNodePair]struct{})

	// Convert deprecated ignoredEdges to pairs.
	for _, ignoredEdge := range in.IgnoredEdges {
		pair, err := r.rpcEdgeToPair(ignoredEdge)
		if err != nil {
			log.Warnf("Ignore channel %v skipped: %v",
				ignoredEdge.ChannelId, err)

			continue
		}
		ignoredPairs[pair] = struct{}{}
	}

	// Add ignored pairs to set.
	for _, ignorePair := range in.IgnoredPairs {
		from, err := route.NewVertexFromBytes(ignorePair.From)
		if err != nil {
			return nil, err
		}

		to, err := route.NewVertexFromBytes(ignorePair.To)
		if err != nil {
			return nil, err
		}

		pair := routing.NewDirectedNodePair(from, to)
		ignoredPairs[pair] = struct{}{}
	}

	// Since QueryRoutes allows having a different source other than
	// ourselves, we'll only apply our max time lock if we are the source.
	maxTotalTimelock := r.MaxTotalTimelock
	if sourcePubKey != r.SelfNode {
		maxTotalTimelock = math.MaxUint32
	}
	cltvLimit, err := ValidateCLTVLimit(in.CltvLimit, maxTotalTimelock)
	if err != nil {
		return nil, err
	}

	// We need to subtract the final delta before passing it into path
	// finding. The optimal path is independent of the final cltv delta and
	// the path finding algorithm is unaware of this value.
	finalCLTVDelta := uint16(zpay32.DefaultFinalCLTVDelta)
	if in.FinalCltvDelta != 0 {
		finalCLTVDelta = uint16(in.FinalCltvDelta)
	}
	cltvLimit -= uint32(finalCLTVDelta)

	var destTLV map[uint64][]byte
	restrictions := &routing.RestrictParams{
		FeeLimit: feeLimit,
		ProbabilitySource: func(fromNode, toNode route.Vertex,
			amt lnwire.MilliAtom) float64 {

			if _, ok := ignoredNodes[fromNode]; ok {
				return 0
			}

			pair := routing.DirectedNodePair{
				From: fromNode,
				To:   toNode,
			}
			if _, ok := ignoredPairs[pair]; ok {
				return 0
			}

			if !in.UseMissionControl {
				return 1
			}

			return r.MissionControl.GetProbability(
				fromNode, toNode, amt,
			)
		},
		DestPayloadTLV: len(destTLV) != 0,
		CltvLimit:      cltvLimit,
	}

	// If we have any TLV records destined for the final hop, then we'll
	// attempt to decode them now into a form that the router can more
	// easily manipulate.
	destTlvRecords, err := tlv.MapToRecords(destTLV)
	if err != nil {
		return nil, err
	}

	// Query the channel router for a possible path to the destination that
	// can carry `in.Amt` satoshis _including_ the total fee required on
	// the route.
	route, err := r.FindRoute(
		sourcePubKey, targetPubKey, amtMSat, restrictions,
		destTlvRecords, finalCLTVDelta,
	)
	if err != nil {
		return nil, err
	}

	// For each valid route, we'll convert the result into the format
	// required by the RPC system.
	rpcRoute, err := r.MarshallRoute(route)
	if err != nil {
		return nil, err
	}

	// Calculate route success probability. Do not rely on a probability
	// that could have been returned from path finding, because mission
	// control may have been disabled in the provided ProbabilitySource.
	successProb := r.getSuccessProbability(route)

	routeResp := &lnrpc.QueryRoutesResponse{
		Routes:      []*lnrpc.Route{rpcRoute},
		SuccessProb: successProb,
	}

	return routeResp, nil
}

// getSuccessProbability returns the success probability for the given route
// based on the current state of mission control.
func (r *RouterBackend) getSuccessProbability(rt *route.Route) float64 {
	fromNode := rt.SourcePubKey
	amtToFwd := rt.TotalAmount
	successProb := 1.0
	for _, hop := range rt.Hops {
		toNode := hop.PubKeyBytes

		probability := r.MissionControl.GetProbability(
			fromNode, toNode, amtToFwd,
		)

		successProb *= probability

		amtToFwd = hop.AmtToForward
		fromNode = toNode
	}

	return successProb
}

// rpcEdgeToPair looks up the provided channel and returns the channel endpoints
// as a directed pair.
func (r *RouterBackend) rpcEdgeToPair(e *lnrpc.EdgeLocator) (
	routing.DirectedNodePair, error) {

	a, b, err := r.FetchChannelEndpoints(e.ChannelId)
	if err != nil {
		return routing.DirectedNodePair{}, err
	}

	var pair routing.DirectedNodePair
	if e.DirectionReverse {
		pair.From, pair.To = b, a
	} else {
		pair.From, pair.To = a, b
	}

	return pair, nil
}

// calculateFeeLimit returns the fee limit in millisatoshis. If a percentage
// based fee limit has been requested, we'll factor in the ratio provided with
// the amount of the payment.
func calculateFeeLimit(feeLimit *lnrpc.FeeLimit,
	amount lnwire.MilliAtom) lnwire.MilliAtom {

	switch feeLimit.GetLimit().(type) {
	case *lnrpc.FeeLimit_Fixed:
		return lnwire.NewMAtomsFromAtoms(
			dcrutil.Amount(feeLimit.GetFixed()),
		)
	case *lnrpc.FeeLimit_Percent:
		return amount * lnwire.MilliAtom(feeLimit.GetPercent()) / 100
	default:
		// If a fee limit was not specified, we'll use the payment's
		// amount as an upper bound in order to avoid payment attempts
		// from incurring fees higher than the payment amount itself.
		return amount
	}
}

// MarshallRoute marshalls an internal route to an rpc route struct.
func (r *RouterBackend) MarshallRoute(route *route.Route) (*lnrpc.Route, error) {
	resp := &lnrpc.Route{
		TotalTimeLock:   route.TotalTimeLock,
		TotalFees:       int64(route.TotalFees().ToAtoms()),
		TotalFeesMAtoms: int64(route.TotalFees()),
		TotalAmt:        int64(route.TotalAmount.ToAtoms()),
		TotalAmtMAtoms:  int64(route.TotalAmount),
		Hops:            make([]*lnrpc.Hop, len(route.Hops)),
	}
	incomingAmt := route.TotalAmount
	for i, hop := range route.Hops {
		fee := route.HopFee(i)

		// Channel capacity is not a defining property of a route. For
		// backwards RPC compatibility, we retrieve it here from the
		// graph.
		chanCapacity, err := r.FetchChannelCapacity(hop.ChannelID)
		if err != nil {
			// If capacity cannot be retrieved, this may be a
			// not-yet-received or private channel. Then report
			// amount that is sent through the channel as capacity.
			chanCapacity = incomingAmt.ToAtoms()
		}

		resp.Hops[i] = &lnrpc.Hop{
			ChanId:             hop.ChannelID,
			ChanCapacity:       int64(chanCapacity),
			AmtToForward:       int64(hop.AmtToForward.ToAtoms()),
			AmtToForwardMAtoms: int64(hop.AmtToForward),
			Fee:                int64(fee.ToAtoms()),
			FeeMAtoms:          int64(fee),
			Expiry:             hop.OutgoingTimeLock,
			PubKey: hex.EncodeToString(
				hop.PubKeyBytes[:],
			),
			TlvPayload: !hop.LegacyPayload,
		}
		incomingAmt = hop.AmtToForward
	}

	return resp, nil
}

// UnmarshallHopByChannelLookup unmarshalls an rpc hop for which the pub key is
// not known. This function will query the channel graph with channel id to
// retrieve both endpoints and determine the hop pubkey using the previous hop
// pubkey. If the channel is unknown, an error is returned.
func (r *RouterBackend) UnmarshallHopByChannelLookup(hop *lnrpc.Hop,
	prevPubKeyBytes [33]byte) (*route.Hop, error) {

	// Discard edge policies, because they may be nil.
	node1, node2, err := r.FetchChannelEndpoints(hop.ChanId)
	if err != nil {
		return nil, err
	}

	var pubKeyBytes [33]byte
	switch {
	case prevPubKeyBytes == node1:
		pubKeyBytes = node2
	case prevPubKeyBytes == node2:
		pubKeyBytes = node1
	default:
		return nil, fmt.Errorf("channel edge does not match expected node")
	}

	var tlvRecords []tlv.Record

	return &route.Hop{
		OutgoingTimeLock: hop.Expiry,
		AmtToForward:     lnwire.MilliAtom(hop.AmtToForwardMAtoms),
		PubKeyBytes:      pubKeyBytes,
		ChannelID:        hop.ChanId,
		TLVRecords:       tlvRecords,
		LegacyPayload:    !hop.TlvPayload,
	}, nil
}

// UnmarshallKnownPubkeyHop unmarshalls an rpc hop that contains the hop pubkey.
// The channel graph doesn't need to be queried because all information required
// for sending the payment is present.
func UnmarshallKnownPubkeyHop(hop *lnrpc.Hop) (*route.Hop, error) {
	pubKey, err := hex.DecodeString(hop.PubKey)
	if err != nil {
		return nil, fmt.Errorf("cannot decode pubkey %s", hop.PubKey)
	}

	var pubKeyBytes [33]byte
	copy(pubKeyBytes[:], pubKey)

	var tlvRecords []tlv.Record

	return &route.Hop{
		OutgoingTimeLock: hop.Expiry,
		AmtToForward:     lnwire.MilliAtom(hop.AmtToForwardMAtoms),
		PubKeyBytes:      pubKeyBytes,
		ChannelID:        hop.ChanId,
		TLVRecords:       tlvRecords,
		LegacyPayload:    !hop.TlvPayload,
	}, nil
}

// UnmarshallHop unmarshalls an rpc hop that may or may not contain a node
// pubkey.
func (r *RouterBackend) UnmarshallHop(hop *lnrpc.Hop,
	prevNodePubKey [33]byte) (*route.Hop, error) {

	if hop.PubKey == "" {
		// If no pub key is given of the hop, the local channel
		// graph needs to be queried to complete the information
		// necessary for routing.
		return r.UnmarshallHopByChannelLookup(hop, prevNodePubKey)
	}

	return UnmarshallKnownPubkeyHop(hop)
}

// UnmarshallRoute unmarshalls an rpc route. For hops that don't specify a
// pubkey, the channel graph is queried.
func (r *RouterBackend) UnmarshallRoute(rpcroute *lnrpc.Route) (
	*route.Route, error) {

	prevNodePubKey := r.SelfNode

	hops := make([]*route.Hop, len(rpcroute.Hops))
	for i, hop := range rpcroute.Hops {
		routeHop, err := r.UnmarshallHop(hop, prevNodePubKey)
		if err != nil {
			return nil, err
		}

		hops[i] = routeHop

		prevNodePubKey = routeHop.PubKeyBytes
	}

	route, err := route.NewRouteFromHops(
		lnwire.MilliAtom(rpcroute.TotalAmtMAtoms),
		rpcroute.TotalTimeLock,
		r.SelfNode,
		hops,
	)
	if err != nil {
		return nil, err
	}

	return route, nil
}

// extractIntentFromSendRequest attempts to parse the SendRequest details
// required to dispatch a client from the information presented by an RPC
// client.
func (r *RouterBackend) extractIntentFromSendRequest(
	rpcPayReq *SendPaymentRequest) (*routing.LightningPayment, error) {

	payIntent := &routing.LightningPayment{}

	// Pass along an outgoing channel restriction if specified.
	if rpcPayReq.OutgoingChanId != 0 {
		payIntent.OutgoingChannelID = &rpcPayReq.OutgoingChanId
	}

	// Take the CLTV limit from the request if set, otherwise use the max.
	cltvLimit, err := ValidateCLTVLimit(
		uint32(rpcPayReq.CltvLimit), r.MaxTotalTimelock,
	)
	if err != nil {
		return nil, err
	}
	payIntent.CltvLimit = cltvLimit

	// Take fee limit from request.
	payIntent.FeeLimit = lnwire.NewMAtomsFromAtoms(
		dcrutil.Amount(rpcPayReq.FeeLimitAtoms),
	)

	// Set payment attempt timeout.
	if rpcPayReq.TimeoutSeconds == 0 {
		return nil, errors.New("timeout_seconds must be specified")
	}

	var destTLV map[uint64][]byte
	if len(destTLV) != 0 {
		var err error
		payIntent.FinalDestRecords, err = tlv.MapToRecords(destTLV)
		if err != nil {
			return nil, err
		}
	}

	payIntent.PayAttemptTimeout = time.Second *
		time.Duration(rpcPayReq.TimeoutSeconds)

	// Route hints.
	routeHints, err := unmarshallRouteHints(
		rpcPayReq.RouteHints,
	)
	if err != nil {
		return nil, err
	}
	payIntent.RouteHints = routeHints

	// If the payment request field isn't blank, then the details of the
	// invoice are encoded entirely within the encoded payReq.  So we'll
	// attempt to decode it, populating the payment accordingly.
	if rpcPayReq.PaymentRequest != "" {
		switch {

		case len(rpcPayReq.Dest) > 0:
			return nil, errors.New("dest and payment_request " +
				"cannot appear together")

		case len(rpcPayReq.PaymentHash) > 0:
			return nil, errors.New("dest and payment_hash " +
				"cannot appear together")

		case rpcPayReq.FinalCltvDelta != 0:
			return nil, errors.New("dest and final_cltv_delta " +
				"cannot appear together")
		}

		payReq, err := zpay32.Decode(
			rpcPayReq.PaymentRequest, r.ActiveNetParams,
		)
		if err != nil {
			return nil, err
		}

		// Next, we'll ensure that this payreq hasn't already expired.
		err = ValidatePayReqExpiry(payReq)
		if err != nil {
			return nil, err
		}

		// If the amount was not included in the invoice, then we let
		// the payee specify the amount of satoshis they wish to send.
		// We override the amount to pay with the amount provided from
		// the payment request.
		if payReq.MilliAt == nil {
			if rpcPayReq.Amt == 0 {
				return nil, errors.New("amount must be " +
					"specified when paying a zero amount " +
					"invoice")
			}

			payIntent.Amount = lnwire.NewMAtomsFromAtoms(
				dcrutil.Amount(rpcPayReq.Amt),
			)
		} else {
			if rpcPayReq.Amt != 0 {
				return nil, errors.New("amount must not be " +
					"specified when paying a non-zero " +
					" amount invoice")
			}

			payIntent.Amount = *payReq.MilliAt
		}

		copy(payIntent.PaymentHash[:], payReq.PaymentHash[:])
		destKey := payReq.Destination.SerializeCompressed()
		copy(payIntent.Target[:], destKey)

		payIntent.FinalCLTVDelta = uint16(payReq.MinFinalCLTVExpiry())
		payIntent.RouteHints = append(
			payIntent.RouteHints, payReq.RouteHints...,
		)
	} else {
		// Otherwise, If the payment request field was not specified
		// (and a custom route wasn't specified), construct the payment
		// from the other fields.

		// Payment destination.
		target, err := route.NewVertexFromBytes(rpcPayReq.Dest)
		if err != nil {
			return nil, err
		}
		payIntent.Target = target

		// Final payment CLTV delta.
		if rpcPayReq.FinalCltvDelta != 0 {
			payIntent.FinalCLTVDelta =
				uint16(rpcPayReq.FinalCltvDelta)
		} else {
			payIntent.FinalCLTVDelta = zpay32.DefaultFinalCLTVDelta
		}

		// Amount.
		if rpcPayReq.Amt == 0 {
			return nil, errors.New("amount must be specified")
		}

		payIntent.Amount = lnwire.NewMAtomsFromAtoms(
			dcrutil.Amount(rpcPayReq.Amt),
		)

		// Payment hash.
		copy(payIntent.PaymentHash[:], rpcPayReq.PaymentHash)
	}

	// Currently, within the bootstrap phase of the network, we limit the
	// largest payment size allotted to (2^32) - 1 mSAT or 4.29 million
	// satoshis.
	if payIntent.Amount > r.MaxPaymentMAtoms {
		// In this case, we'll send an error to the caller, but
		// continue our loop for the next payment.
		return payIntent, fmt.Errorf("payment of %v is too large, "+
			"max payment allowed is %v", payIntent.Amount,
			r.MaxPaymentMAtoms)

	}

	return payIntent, nil
}

// unmarshallRouteHints unmarshalls a list of route hints.
func unmarshallRouteHints(rpcRouteHints []*lnrpc.RouteHint) (
	[][]zpay32.HopHint, error) {

	routeHints := make([][]zpay32.HopHint, 0, len(rpcRouteHints))
	for _, rpcRouteHint := range rpcRouteHints {
		routeHint := make(
			[]zpay32.HopHint, 0, len(rpcRouteHint.HopHints),
		)
		for _, rpcHint := range rpcRouteHint.HopHints {
			hint, err := unmarshallHopHint(rpcHint)
			if err != nil {
				return nil, err
			}

			routeHint = append(routeHint, hint)
		}
		routeHints = append(routeHints, routeHint)
	}

	return routeHints, nil
}

// unmarshallHopHint unmarshalls a single hop hint.
func unmarshallHopHint(rpcHint *lnrpc.HopHint) (zpay32.HopHint, error) {
	pubBytes, err := hex.DecodeString(rpcHint.NodeId)
	if err != nil {
		return zpay32.HopHint{}, err
	}

	pubkey, err := secp256k1.ParsePubKey(pubBytes)
	if err != nil {
		return zpay32.HopHint{}, err
	}

	return zpay32.HopHint{
		NodeID:                    pubkey,
		ChannelID:                 rpcHint.ChanId,
		FeeBaseMAtoms:             rpcHint.FeeBaseMAtoms,
		FeeProportionalMillionths: rpcHint.FeeProportionalMillionths,
		CLTVExpiryDelta:           uint16(rpcHint.CltvExpiryDelta),
	}, nil
}

// ValidatePayReqExpiry checks if the passed payment request has expired. In
// the case it has expired, an error will be returned.
func ValidatePayReqExpiry(payReq *zpay32.Invoice) error {
	expiry := payReq.Expiry()
	validUntil := payReq.Timestamp.Add(expiry)
	if time.Now().After(validUntil) {
		return fmt.Errorf("invoice expired. Valid until %v", validUntil)
	}

	return nil
}

// ValidateCLTVLimit returns a valid CLTV limit given a value and a maximum. If
// the value exceeds the maximum, then an error is returned. If the value is 0,
// then the maximum is used.
func ValidateCLTVLimit(val, max uint32) (uint32, error) {
	switch {
	case val == 0:
		return max, nil
	case val > max:
		return 0, fmt.Errorf("total time lock of %v exceeds max "+
			"allowed %v", val, max)
	default:
		return val, nil
	}
}
