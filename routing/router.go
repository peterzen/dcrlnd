package routing

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/decred/dcrd/dcrutil/v2"
	"github.com/decred/dcrd/wire"
	sphinx "github.com/decred/lightning-onion/v2"
	"github.com/go-errors/errors"
	bolt "go.etcd.io/bbolt"

	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/htlcswitch"
	"github.com/decred/dcrlnd/input"
	"github.com/decred/dcrlnd/lntypes"
	"github.com/decred/dcrlnd/lnwallet"
	"github.com/decred/dcrlnd/lnwallet/chanvalidate"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/multimutex"
	"github.com/decred/dcrlnd/routing/chainview"
	"github.com/decred/dcrlnd/routing/route"
	"github.com/decred/dcrlnd/ticker"
	"github.com/decred/dcrlnd/tlv"
	"github.com/decred/dcrlnd/zpay32"
)

const (
	// DefaultPayAttemptTimeout is the default payment attempt timeout. The
	// payment attempt timeout defines the duration after which we stop
	// trying more routes for a payment.
	DefaultPayAttemptTimeout = time.Second * 60

	// DefaultChannelPruneExpiry is the default duration used to determine
	// if a channel should be pruned or not.
	DefaultChannelPruneExpiry = time.Hour * 24 * 14

	// defaultStatInterval governs how often the router will log non-empty
	// stats related to processing new channels, updates, or node
	// announcements.
	defaultStatInterval = time.Minute
)

var (
	// ErrRouterShuttingDown is returned if the router is in the process of
	// shutting down.
	ErrRouterShuttingDown = fmt.Errorf("router shutting down")
)

// ChannelGraphSource represents the source of information about the topology
// of the lightning network. It's responsible for the addition of nodes, edges,
// applying edge updates, and returning the current block height with which the
// topology is synchronized.
type ChannelGraphSource interface {
	// AddNode is used to add information about a node to the router
	// database. If the node with this pubkey is not present in an existing
	// channel, it will be ignored.
	AddNode(node *channeldb.LightningNode) error

	// AddEdge is used to add edge/channel to the topology of the router,
	// after all information about channel will be gathered this
	// edge/channel might be used in construction of payment path.
	AddEdge(edge *channeldb.ChannelEdgeInfo) error

	// AddProof updates the channel edge info with proof which is needed to
	// properly announce the edge to the rest of the network.
	AddProof(chanID lnwire.ShortChannelID, proof *channeldb.ChannelAuthProof) error

	// UpdateEdge is used to update edge information, without this message
	// edge considered as not fully constructed.
	UpdateEdge(policy *channeldb.ChannelEdgePolicy) error

	// IsStaleNode returns true if the graph source has a node announcement
	// for the target node with a more recent timestamp. This method will
	// also return true if we don't have an active channel announcement for
	// the target node.
	IsStaleNode(node route.Vertex, timestamp time.Time) bool

	// IsPublicNode determines whether the given vertex is seen as a public
	// node in the graph from the graph's source node's point of view.
	IsPublicNode(node route.Vertex) (bool, error)

	// IsKnownEdge returns true if the graph source already knows of the
	// passed channel ID either as a live or zombie edge.
	IsKnownEdge(chanID lnwire.ShortChannelID) bool

	// IsStaleEdgePolicy returns true if the graph source has a channel
	// edge for the passed channel ID (and flags) that have a more recent
	// timestamp.
	IsStaleEdgePolicy(chanID lnwire.ShortChannelID, timestamp time.Time,
		flags lnwire.ChanUpdateChanFlags) bool

	// MarkEdgeLive clears an edge from our zombie index, deeming it as
	// live.
	MarkEdgeLive(chanID lnwire.ShortChannelID) error

	// ForAllOutgoingChannels is used to iterate over all channels
	// emanating from the "source" node which is the center of the
	// star-graph.
	ForAllOutgoingChannels(cb func(c *channeldb.ChannelEdgeInfo,
		e *channeldb.ChannelEdgePolicy) error) error

	// CurrentBlockHeight returns the block height from POV of the router
	// subsystem.
	CurrentBlockHeight() (uint32, error)

	// GetChannelByID return the channel by the channel id.
	GetChannelByID(chanID lnwire.ShortChannelID) (*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy, *channeldb.ChannelEdgePolicy, error)

	// FetchLightningNode attempts to look up a target node by its identity
	// public key. channeldb.ErrGraphNodeNotFound is returned if the node
	// doesn't exist within the graph.
	FetchLightningNode(route.Vertex) (*channeldb.LightningNode, error)

	// ForEachNode is used to iterate over every node in the known graph.
	ForEachNode(func(node *channeldb.LightningNode) error) error

	// ForEachChannel is used to iterate over every channel in the known
	// graph.
	ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error) error
}

// PaymentAttemptDispatcher is used by the router to send payment attempts onto
// the network, and receive their results.
type PaymentAttemptDispatcher interface {
	// SendHTLC is a function that directs a link-layer switch to
	// forward a fully encoded payment to the first hop in the route
	// denoted by its public key. A non-nil error is to be returned if the
	// payment was unsuccessful.
	SendHTLC(firstHop lnwire.ShortChannelID,
		paymentID uint64,
		htlcAdd *lnwire.UpdateAddHTLC) error

	// GetPaymentResult returns the the result of the payment attempt with
	// the given paymentID. The method returns a channel where the payment
	// result will be sent when available, or an error is encountered
	// during forwarding. When a result is received on the channel, the
	// HTLC is guaranteed to no longer be in flight. The switch shutting
	// down is signaled by closing the channel. If the paymentID is
	// unknown, ErrPaymentIDNotFound will be returned.
	GetPaymentResult(paymentID uint64, paymentHash lntypes.Hash,
		deobfuscator htlcswitch.ErrorDecrypter) (
		<-chan *htlcswitch.PaymentResult, error)
}

// PaymentSessionSource is an interface that defines a source for the router to
// retrive new payment sessions.
type PaymentSessionSource interface {
	// NewPaymentSession creates a new payment session that will produce
	// routes to the given target. An optional set of routing hints can be
	// provided in order to populate additional edges to explore when
	// finding a path to the payment's destination.
	NewPaymentSession(routeHints [][]zpay32.HopHint,
		target route.Vertex) (PaymentSession, error)

	// NewPaymentSessionForRoute creates a new paymentSession instance that
	// is just used for failure reporting to missioncontrol, and will only
	// attempt the given route.
	NewPaymentSessionForRoute(preBuiltRoute *route.Route) PaymentSession

	// NewPaymentSessionEmpty creates a new paymentSession instance that is
	// empty, and will be exhausted immediately. Used for failure reporting
	// to missioncontrol for resumed payment we don't want to make more
	// attempts for.
	NewPaymentSessionEmpty() PaymentSession
}

// MissionController is an interface that exposes failure reporting and
// probability estimation.
type MissionController interface {
	// ReportPaymentFail reports a failed payment to mission control as
	// input for future probability estimates. It returns a bool indicating
	// whether this error is a final error and no further payment attempts
	// need to be made.
	ReportPaymentFail(paymentID uint64, rt *route.Route,
		failureSourceIdx *int, failure lnwire.FailureMessage) (
		*channeldb.FailureReason, error)

	// ReportPaymentSuccess reports a successful payment to mission control as input
	// for future probability estimates.
	ReportPaymentSuccess(paymentID uint64, rt *route.Route) error

	// GetProbability is expected to return the success probability of a
	// payment from fromNode along edge.
	GetProbability(fromNode, toNode route.Vertex,
		amt lnwire.MilliAtom) float64
}

// FeeSchema is the set fee configuration for a Lightning Node on the network.
// Using the coefficients described within the schema, the required fee to
// forward outgoing payments can be derived.
type FeeSchema struct {
	// BaseFee is the base amount of milli-atoms that will be chained
	// for ANY payment forwarded.
	BaseFee lnwire.MilliAtom

	// FeeRate is the rate that will be charged for forwarding payments.
	// This value should be interpreted as the numerator for a fraction
	// (fixed point arithmetic) whose denominator is 1 million. As a result
	// the effective fee rate charged per milli-atoms will be: (amount *
	// FeeRate/1,000,000).
	FeeRate uint32
}

// ChannelPolicy holds the parameters that determine the policy we enforce
// when forwarding payments on a channel. These parameters are communicated
// to the rest of the network in ChannelUpdate messages.
type ChannelPolicy struct {
	// FeeSchema holds the fee configuration for a channel.
	FeeSchema

	// TimeLockDelta is the required HTLC timelock delta to be used
	// when forwarding payments.
	TimeLockDelta uint32

	// MaxHTLC is the maximum HTLC size including fees we are allowed to
	// forward over this channel.
	MaxHTLC lnwire.MilliAtom
}

// Config defines the configuration for the ChannelRouter. ALL elements within
// the configuration MUST be non-nil for the ChannelRouter to carry out its
// duties.
type Config struct {
	// Graph is the channel graph that the ChannelRouter will use to gather
	// metrics from and also to carry out path finding queries.
	// TODO(roasbeef): make into an interface
	Graph *channeldb.ChannelGraph

	// Chain is the router's source to the most up-to-date blockchain data.
	// All incoming advertised channels will be checked against the chain
	// to ensure that the channels advertised are still open.
	Chain lnwallet.BlockChainIO

	// ChainView is an instance of a FilteredChainView which is used to
	// watch the sub-set of the UTXO set (the set of active channels) that
	// we need in order to properly maintain the channel graph.
	ChainView chainview.FilteredChainView

	// Payer is an instance of a PaymentAttemptDispatcher and is used by
	// the router to send payment attempts onto the network, and receive
	// their results.
	Payer PaymentAttemptDispatcher

	// Control keeps track of the status of ongoing payments, ensuring we
	// can properly resume them across restarts.
	Control ControlTower

	// MissionControl is a shared memory of sorts that executions of
	// payment path finding use in order to remember which vertexes/edges
	// were pruned from prior attempts. During SendPayment execution,
	// errors sent by nodes are mapped into a vertex or edge to be pruned.
	// Each run will then take into account this set of pruned
	// vertexes/edges to reduce route failure and pass on graph information
	// gained to the next execution.
	MissionControl MissionController

	// SessionSource defines a source for the router to retrieve new payment
	// sessions.
	SessionSource PaymentSessionSource

	// ChannelPruneExpiry is the duration used to determine if a channel
	// should be pruned or not. If the delta between now and when the
	// channel was last updated is greater than ChannelPruneExpiry, then
	// the channel is marked as a zombie channel eligible for pruning.
	ChannelPruneExpiry time.Duration

	// GraphPruneInterval is used as an interval to determine how often we
	// should examine the channel graph to garbage collect zombie channels.
	GraphPruneInterval time.Duration

	// QueryBandwidth is a method that allows the router to query the lower
	// link layer to determine the up to date available bandwidth at a
	// prospective link to be traversed. If the  link isn't available, then
	// a value of zero should be returned. Otherwise, the current up to
	// date knowledge of the available bandwidth of the link should be
	// returned.
	QueryBandwidth func(edge *channeldb.ChannelEdgeInfo) lnwire.MilliAtom

	// NextPaymentID is a method that guarantees to return a new, unique ID
	// each time it is called. This is used by the router to generate a
	// unique payment ID for each payment it attempts to send, such that
	// the switch can properly handle the HTLC.
	NextPaymentID func() (uint64, error)

	// AssumeChannelValid toggles whether or not the router will check for
	// spentness of channel outpoints. For neutrino, this saves long rescans
	// from blocking initial usage of the daemon.
	AssumeChannelValid bool

	// PathFindingConfig defines global path finding parameters.
	PathFindingConfig PathFindingConfig
}

// EdgeLocator is a struct used to identify a specific edge.
type EdgeLocator struct {
	// ChannelID is the channel of this edge.
	ChannelID uint64

	// Direction takes the value of 0 or 1 and is identical in definition to
	// the channel direction flag. A value of 0 means the direction from the
	// lower node pubkey to the higher.
	Direction uint8
}

// String returns a human readable version of the edgeLocator values.
func (e *EdgeLocator) String() string {
	return fmt.Sprintf("%v:%v", e.ChannelID, e.Direction)
}

// ChannelRouter is the layer 3 router within the Lightning stack. Below the
// ChannelRouter is the HtlcSwitch, and below that is the Bitcoin blockchain
// itself. The primary role of the ChannelRouter is to respond to queries for
// potential routes that can support a payment amount, and also general graph
// reachability questions. The router will prune the channel graph
// automatically as new blocks are discovered which spend certain known funding
// outpoints, thereby closing their respective channels.
type ChannelRouter struct {
	ntfnClientCounter uint64 // To be used atomically.

	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	bestHeight uint32 // To be used atomically.

	// cfg is a copy of the configuration struct that the ChannelRouter was
	// initialized with.
	cfg *Config

	// selfNode is the center of the star-graph centered around the
	// ChannelRouter. The ChannelRouter uses this node as a starting point
	// when doing any path finding.
	selfNode *channeldb.LightningNode

	// newBlocks is a channel in which new blocks connected to the end of
	// the main chain are sent over, and blocks updated after a call to
	// UpdateFilter.
	newBlocks <-chan *chainview.FilteredBlock

	// staleBlocks is a channel in which blocks disconnected fromt the end
	// of our currently known best chain are sent over.
	staleBlocks <-chan *chainview.FilteredBlock

	// networkUpdates is a channel that carries new topology updates
	// messages from outside the ChannelRouter to be processed by the
	// networkHandler.
	networkUpdates chan *routingMsg

	// topologyClients maps a client's unique notification ID to a
	// topologyClient client that contains its notification dispatch
	// channel.
	topologyClients map[uint64]*topologyClient

	// ntfnClientUpdates is a channel that's used to send new updates to
	// topology notification clients to the ChannelRouter. Updates either
	// add a new notification client, or cancel notifications for an
	// existing client.
	ntfnClientUpdates chan *topologyClientUpdate

	// channelEdgeMtx is a mutex we use to make sure we process only one
	// ChannelEdgePolicy at a time for a given channelID, to ensure
	// consistency between the various database accesses.
	channelEdgeMtx *multimutex.Mutex

	// statTicker is a resumable ticker that logs the router's progress as
	// it discovers channels or receives updates.
	statTicker ticker.Ticker

	// stats tracks newly processed channels, updates, and node
	// announcements over a window of defaultStatInterval.
	stats *routerStats

	sync.RWMutex

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure ChannelRouter implements the
// ChannelGraphSource interface.
var _ ChannelGraphSource = (*ChannelRouter)(nil)

// New creates a new instance of the ChannelRouter with the specified
// configuration parameters. As part of initialization, if the router detects
// that the channel graph isn't fully in sync with the latest UTXO (since the
// channel graph is a subset of the UTXO set) set, then the router will proceed
// to fully sync to the latest state of the UTXO set.
func New(cfg Config) (*ChannelRouter, error) {

	selfNode, err := cfg.Graph.SourceNode()
	if err != nil {
		return nil, err
	}

	r := &ChannelRouter{
		cfg:               &cfg,
		networkUpdates:    make(chan *routingMsg),
		topologyClients:   make(map[uint64]*topologyClient),
		ntfnClientUpdates: make(chan *topologyClientUpdate),
		channelEdgeMtx:    multimutex.NewMutex(),
		selfNode:          selfNode,
		statTicker:        ticker.New(defaultStatInterval),
		stats:             new(routerStats),
		quit:              make(chan struct{}),
	}

	return r, nil
}

// Start launches all the goroutines the ChannelRouter requires to carry out
// its duties. If the router has already been started, then this method is a
// noop.
func (r *ChannelRouter) Start() error {
	if !atomic.CompareAndSwapUint32(&r.started, 0, 1) {
		return nil
	}

	log.Tracef("Channel Router starting")

	bestHash, bestHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return err
	}

	// If the graph has never been pruned, or hasn't fully been created yet,
	// then we don't treat this as an explicit error.
	if _, _, err := r.cfg.Graph.PruneTip(); err != nil {
		switch {
		case err == channeldb.ErrGraphNeverPruned:
			fallthrough
		case err == channeldb.ErrGraphNotFound:
			// If the graph has never been pruned, then we'll set
			// the prune height to the current best height of the
			// chain backend.
			_, err = r.cfg.Graph.PruneGraph(
				nil, bestHash, uint32(bestHeight),
			)
			if err != nil {
				return err
			}
		default:
			return err
		}
	}

	// If AssumeChannelValid is present, then we won't rely on pruning
	// channels from the graph based on their spentness, but whether they
	// are considered zombies or not.
	if r.cfg.AssumeChannelValid {
		if err := r.pruneZombieChans(); err != nil {
			return err
		}
	} else {
		// Otherwise, we'll use our filtered chain view to prune
		// channels as soon as they are detected as spent on-chain.
		if err := r.cfg.ChainView.Start(); err != nil {
			return err
		}

		// Once the instance is active, we'll fetch the channel we'll
		// receive notifications over.
		r.newBlocks = r.cfg.ChainView.FilteredBlocks()
		r.staleBlocks = r.cfg.ChainView.DisconnectedBlocks()

		// Before we perform our manual block pruning, we'll construct
		// and apply a fresh chain filter to the active
		// FilteredChainView instance.  We do this before, as otherwise
		// we may miss on-chain events as the filter hasn't properly
		// been applied.
		channelView, err := r.cfg.Graph.ChannelView()
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			return err
		}

		log.Infof("Filtering chain using %v channels active",
			len(channelView))

		if len(channelView) != 0 {
			err = r.cfg.ChainView.UpdateFilter(
				channelView, int64(bestHeight),
			)
			if err != nil {
				return err
			}
		}

		// Before we begin normal operation of the router, we first need
		// to synchronize the channel graph to the latest state of the
		// UTXO set.
		if err := r.syncGraphWithChain(); err != nil {
			return err
		}

		// Finally, before we proceed, we'll prune any unconnected nodes
		// from the graph in order to ensure we maintain a tight graph
		// of "useful" nodes.
		err = r.cfg.Graph.PruneGraphNodes()
		if err != nil && err != channeldb.ErrGraphNodesNotFound {
			return err
		}
	}

	// If any payments are still in flight, we resume, to make sure their
	// results are properly handled.
	payments, err := r.cfg.Control.FetchInFlightPayments()
	if err != nil {
		return err
	}

	for _, payment := range payments {
		log.Infof("Resuming payment with hash %v", payment.Info.PaymentHash)
		r.wg.Add(1)
		go func(payment *channeldb.InFlightPayment) {
			defer r.wg.Done()

			// We create a dummy, empty payment session such that
			// we won't make another payment attempt when the
			// result for the in-flight attempt is received.
			//
			// PayAttemptTime doesn't need to be set, as there is
			// only a single attempt.
			paySession := r.cfg.SessionSource.NewPaymentSessionEmpty()

			lPayment := &LightningPayment{
				PaymentHash: payment.Info.PaymentHash,
			}

			_, _, err = r.sendPayment(payment.Attempt, lPayment, paySession)
			if err != nil {
				log.Errorf("Resuming payment with hash %v "+
					"failed: %v.", payment.Info.PaymentHash, err)
				return
			}

			log.Infof("Resumed payment with hash %v completed.",
				payment.Info.PaymentHash)
		}(payment)
	}

	r.wg.Add(1)
	go r.networkHandler()

	return nil
}

// Stop signals the ChannelRouter to gracefully halt all routines. This method
// will *block* until all goroutines have excited. If the channel router has
// already stopped then this method will return immediately.
func (r *ChannelRouter) Stop() error {
	if !atomic.CompareAndSwapUint32(&r.stopped, 0, 1) {
		return nil
	}

	log.Tracef("Channel Router shutting down")

	// Our filtered chain view could've only been started if
	// AssumeChannelValid isn't present.
	if !r.cfg.AssumeChannelValid {
		if err := r.cfg.ChainView.Stop(); err != nil {
			return err
		}
	}

	close(r.quit)
	r.wg.Wait()

	return nil
}

// syncGraphWithChain attempts to synchronize the current channel graph with
// the latest UTXO set state. This process involves pruning from the channel
// graph any channels which have been closed by spending their funding output
// since we've been down.
func (r *ChannelRouter) syncGraphWithChain() error {
	// First, we'll need to check to see if we're already in sync with the
	// latest state of the UTXO set.
	bestHash, bestHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return err
	}
	r.bestHeight = uint32(bestHeight)

	pruneHash, pruneHeight, err := r.cfg.Graph.PruneTip()
	if err != nil {
		switch {
		// If the graph has never been pruned, or hasn't fully been
		// created yet, then we don't treat this as an explicit error.
		case err == channeldb.ErrGraphNeverPruned:
		case err == channeldb.ErrGraphNotFound:
		default:
			return err
		}
	}

	log.Infof("Prune tip for Channel Graph: height=%v, hash=%v", pruneHeight,
		pruneHash)

	switch {

	// If the graph has never been pruned, then we can exit early as this
	// entails it's being created for the first time and hasn't seen any
	// block or created channels.
	case pruneHeight == 0 || pruneHash == nil:
		return nil

	// If the block hashes and heights match exactly, then we don't need to
	// prune the channel graph as we're already fully in sync.
	case bestHash.IsEqual(pruneHash) && uint32(bestHeight) == pruneHeight:
		return nil
	}

	// If the main chain blockhash at prune height is different from the
	// prune hash, this might indicate the database is on a stale branch.
	mainBlockHash, err := r.cfg.Chain.GetBlockHash(int64(pruneHeight))
	if err != nil {
		return err
	}

	// While we are on a stale branch of the chain, walk backwards to find
	// first common block.
	for !pruneHash.IsEqual(mainBlockHash) {
		log.Infof("channel graph is stale. Disconnecting block %v "+
			"(hash=%v)", pruneHeight, pruneHash)
		// Prune the graph for every channel that was opened at height
		// >= pruneHeight.
		_, err := r.cfg.Graph.DisconnectBlockAtHeight(pruneHeight)
		if err != nil {
			return err
		}

		pruneHash, pruneHeight, err = r.cfg.Graph.PruneTip()
		if err != nil {
			switch {
			// If at this point the graph has never been pruned, we
			// can exit as this entails we are back to the point
			// where it hasn't seen any block or created channels,
			// alas there's nothing left to prune.
			case err == channeldb.ErrGraphNeverPruned:
				return nil
			case err == channeldb.ErrGraphNotFound:
				return nil
			default:
				return err
			}
		}
		mainBlockHash, err = r.cfg.Chain.GetBlockHash(int64(pruneHeight))
		if err != nil {
			return err
		}
	}

	log.Infof("Syncing channel graph from height=%v (hash=%v) to height=%v "+
		"(hash=%v)", pruneHeight, pruneHash, bestHeight, bestHash)

	// If we're not yet caught up, then we'll walk forward in the chain
	// pruning the channel graph with each new block that hasn't yet been
	// consumed by the channel graph.
	var spentOutputs []*wire.OutPoint
	for nextHeight := pruneHeight + 1; nextHeight <= uint32(bestHeight); nextHeight++ {
		// Break out of the rescan early if a shutdown has been
		// requested, otherwise long rescans will block the daemon from
		// shutting down promptly.
		select {
		case <-r.quit:
			return ErrRouterShuttingDown
		default:
		}

		// Using the next height, request a manual block pruning from
		// the chainview for the particular block hash.
		nextHash, err := r.cfg.Chain.GetBlockHash(int64(nextHeight))
		if err != nil {
			return err
		}
		filterBlock, err := r.cfg.ChainView.FilterBlock(nextHash)
		if err != nil {
			return err
		}

		// We're only interested in all prior outputs that have been
		// spent in the block, so collate all the referenced previous
		// outpoints within each tx and input.
		for _, tx := range filterBlock.Transactions {
			for _, txIn := range tx.TxIn {
				spentOutputs = append(spentOutputs,
					&txIn.PreviousOutPoint)
			}
		}
	}

	// With the spent outputs gathered, attempt to prune the channel graph,
	// also passing in the best hash+height so the prune tip can be updated.
	closedChans, err := r.cfg.Graph.PruneGraph(
		spentOutputs, bestHash, uint32(bestHeight),
	)
	if err != nil {
		return err
	}

	log.Infof("Graph pruning complete: %v channels were closed since "+
		"height %v", len(closedChans), pruneHeight)
	return nil
}

// pruneZombieChans is a method that will be called periodically to prune out
// any "zombie" channels. We consider channels zombies if *both* edges haven't
// been updated since our zombie horizon. If AssumeChannelValid is present,
// we'll also consider channels zombies if *both* edges are disabled. This
// usually signals that a channel has been closed on-chain. We do this
// periodically to keep a healthy, lively routing table.
func (r *ChannelRouter) pruneZombieChans() error {
	chansToPrune := make(map[uint64]struct{})
	chanExpiry := r.cfg.ChannelPruneExpiry

	log.Infof("Examining channel graph for zombie channels")

	// A helper method to detect if the channel belongs to this node
	isSelfChannelEdge := func(info *channeldb.ChannelEdgeInfo) bool {
		return info.NodeKey1Bytes == r.selfNode.PubKeyBytes ||
			info.NodeKey2Bytes == r.selfNode.PubKeyBytes
	}

	// First, we'll collect all the channels which are eligible for garbage
	// collection due to being zombies.
	filterPruneChans := func(info *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error {

		// Exit early in case this channel is already marked to be pruned
		if _, markedToPrune := chansToPrune[info.ChannelID]; markedToPrune {
			return nil
		}

		// We'll ensure that we don't attempt to prune our *own*
		// channels from the graph, as in any case this should be
		// re-advertised by the sub-system above us.
		if isSelfChannelEdge(info) {
			return nil
		}

		// If *both* edges haven't been updated for a period of
		// chanExpiry, then we'll mark the channel itself as eligible
		// for graph pruning.
		var e1Zombie, e2Zombie bool
		if e1 != nil {
			e1Zombie = time.Since(e1.LastUpdate) >= chanExpiry
			if e1Zombie {
				log.Tracef("Edge #1 of ChannelID(%v) last "+
					"update: %v", info.ChannelID,
					e1.LastUpdate)
			}
		}
		if e2 != nil {
			e2Zombie = time.Since(e2.LastUpdate) >= chanExpiry
			if e2Zombie {
				log.Tracef("Edge #2 of ChannelID(%v) last "+
					"update: %v", info.ChannelID,
					e2.LastUpdate)
			}
		}

		// If the channel is not considered zombie, we can move on to
		// the next.
		if !e1Zombie || !e2Zombie {
			return nil
		}

		log.Debugf("ChannelID(%v) is a zombie, collecting to prune",
			info.ChannelID)

		// TODO(roasbeef): add ability to delete single directional edge
		chansToPrune[info.ChannelID] = struct{}{}

		return nil
	}

	// If AssumeChannelValid is present we'll look at the disabled bit for both
	// edges. If they're both disabled, then we can interpret this as the
	// channel being closed and can prune it from our graph.
	if r.cfg.AssumeChannelValid {
		disabledChanIDs, err := r.cfg.Graph.DisabledChannelIDs()
		if err != nil {
			return fmt.Errorf("unable to get disabled channels ids "+
				"chans: %v", err)
		}

		disabledEdges, err := r.cfg.Graph.FetchChanInfos(disabledChanIDs)
		if err != nil {
			return fmt.Errorf("unable to fetch disabled channels edges "+
				"chans: %v", err)
		}

		// Ensuring we won't prune our own channel from the graph.
		for _, disabledEdge := range disabledEdges {
			if !isSelfChannelEdge(disabledEdge.Info) {
				chansToPrune[disabledEdge.Info.ChannelID] = struct{}{}
			}
		}
	}

	startTime := time.Unix(0, 0)
	endTime := time.Now().Add(-1 * chanExpiry)
	oldEdges, err := r.cfg.Graph.ChanUpdatesInHorizon(startTime, endTime)
	if err != nil {
		return fmt.Errorf("unable to fetch expired channel updates "+
			"chans: %v", err)
	}

	for _, u := range oldEdges {
		filterPruneChans(u.Info, u.Policy1, u.Policy2)
	}

	log.Infof("Pruning %v zombie channels", len(chansToPrune))

	// With the set of zombie-like channels obtained, we'll do another pass
	// to delete them from the channel graph.
	toPrune := make([]uint64, 0, len(chansToPrune))
	for chanID := range chansToPrune {
		toPrune = append(toPrune, chanID)
		log.Tracef("Pruning zombie channel with ChannelID(%v)", chanID)
	}
	if err := r.cfg.Graph.DeleteChannelEdges(toPrune...); err != nil {
		return fmt.Errorf("unable to delete zombie channels: %v", err)
	}

	// With the channels pruned, we'll also attempt to prune any nodes that
	// were a part of them.
	err = r.cfg.Graph.PruneGraphNodes()
	if err != nil && err != channeldb.ErrGraphNodesNotFound {
		return fmt.Errorf("unable to prune graph nodes: %v", err)
	}

	return nil
}

// networkHandler is the primary goroutine for the ChannelRouter. The roles of
// this goroutine include answering queries related to the state of the
// network, pruning the graph on new block notification, applying network
// updates, and registering new topology clients.
//
// NOTE: This MUST be run as a goroutine.
func (r *ChannelRouter) networkHandler() {
	defer r.wg.Done()

	graphPruneTicker := time.NewTicker(r.cfg.GraphPruneInterval)
	defer graphPruneTicker.Stop()

	r.statTicker.Resume()
	defer r.statTicker.Stop()

	r.stats.Reset()

	// We'll use this validation barrier to ensure that we process all jobs
	// in the proper order during parallel validation.
	validationBarrier := NewValidationBarrier(runtime.NumCPU()*4, r.quit)

	for {
		select {
		// A new fully validated network update has just arrived. As a
		// result we'll modify the channel graph accordingly depending
		// on the exact type of the message.
		case update := <-r.networkUpdates:
			// We'll set up any dependants, and wait until a free
			// slot for this job opens up, this allow us to not
			// have thousands of goroutines active.
			validationBarrier.InitJobDependencies(update.msg)

			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				defer validationBarrier.CompleteJob()

				// If this message has an existing dependency,
				// then we'll wait until that has been fully
				// validated before we proceed.
				err := validationBarrier.WaitForDependants(
					update.msg,
				)
				if err != nil {
					if err != ErrVBarrierShuttingDown {
						log.Warnf("unexpected error "+
							"during validation "+
							"barrier shutdown: %v",
							err)
					}
					return
				}

				// Process the routing update to determine if
				// this is either a new update from our PoV or
				// an update to a prior vertex/edge we
				// previously accepted.
				err = r.processUpdate(update.msg)
				update.err <- err

				// If this message had any dependencies, then
				// we can now signal them to continue.
				validationBarrier.SignalDependants(update.msg)
				if err != nil {
					return
				}

				// Send off a new notification for the newly
				// accepted update.
				topChange := &TopologyChange{}
				err = addToTopologyChange(
					r.cfg.Graph, topChange, update.msg,
				)
				if err != nil {
					log.Errorf("unable to update topology "+
						"change notification: %v", err)
					return
				}

				if !topChange.isEmpty() {
					r.notifyTopologyChange(topChange)
				}
			}()

			// TODO(roasbeef): remove all unconnected vertexes
			// after N blocks pass with no corresponding
			// announcements.

		case chainUpdate, ok := <-r.staleBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// Since this block is stale, we update our best height
			// to the previous block.
			blockHeight := uint32(chainUpdate.Height)
			atomic.StoreUint32(&r.bestHeight, blockHeight-1)

			// Update the channel graph to reflect that this block
			// was disconnected.
			_, err := r.cfg.Graph.DisconnectBlockAtHeight(blockHeight)
			if err != nil {
				log.Errorf("unable to prune graph with stale "+
					"block: %v", err)
				continue
			}

			// TODO(halseth): notify client about the reorg?

		// A new block has arrived, so we can prune the channel graph
		// of any channels which were closed in the block.
		case chainUpdate, ok := <-r.newBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// We'll ensure that any new blocks received attach
			// directly to the end of our main chain. If not, then
			// we've somehow missed some blocks. We don't process
			// this block as otherwise, we may miss on-chain
			// events.
			currentHeight := atomic.LoadUint32(&r.bestHeight)
			if chainUpdate.Height != int64(currentHeight+1) {
				log.Errorf("out of order block: expecting "+
					"height=%v, got height=%v", currentHeight+1,
					chainUpdate.Height)
				continue
			}

			// Once a new block arrives, we update our running
			// track of the height of the chain tip.
			blockHeight := uint32(chainUpdate.Height)
			atomic.StoreUint32(&r.bestHeight, blockHeight)
			log.Infof("Pruning channel graph using block %v (height=%v)",
				chainUpdate.Hash, blockHeight)

			// We're only interested in all prior outputs that have
			// been spent in the block, so collate all the
			// referenced previous outpoints within each tx and
			// input.
			var spentOutputs []*wire.OutPoint
			for _, tx := range chainUpdate.Transactions {
				for _, txIn := range tx.TxIn {
					spentOutputs = append(spentOutputs,
						&txIn.PreviousOutPoint)
				}
			}

			// With the spent outputs gathered, attempt to prune
			// the channel graph, also passing in the hash+height
			// of the block being pruned so the prune tip can be
			// updated.
			chansClosed, err := r.cfg.Graph.PruneGraph(spentOutputs,
				&chainUpdate.Hash, uint32(chainUpdate.Height))
			if err != nil {
				log.Errorf("unable to prune routing table: %v", err)
				continue
			}

			log.Infof("Block %v (height=%v) closed %v channels",
				chainUpdate.Hash, blockHeight, len(chansClosed))

			if len(chansClosed) == 0 {
				continue
			}

			// Notify all currently registered clients of the newly
			// closed channels.
			closeSummaries := createCloseSummaries(blockHeight, chansClosed...)
			r.notifyTopologyChange(&TopologyChange{
				ClosedChannels: closeSummaries,
			})

		// A new notification client update has arrived. We're either
		// gaining a new client, or cancelling notifications for an
		// existing client.
		case ntfnUpdate := <-r.ntfnClientUpdates:
			clientID := ntfnUpdate.clientID

			if ntfnUpdate.cancel {
				r.RLock()
				client, ok := r.topologyClients[ntfnUpdate.clientID]
				r.RUnlock()
				if ok {
					r.Lock()
					delete(r.topologyClients, clientID)
					r.Unlock()

					close(client.exit)
					client.wg.Wait()

					close(client.ntfnChan)
				}

				continue
			}

			r.Lock()
			r.topologyClients[ntfnUpdate.clientID] = &topologyClient{
				ntfnChan: ntfnUpdate.ntfnChan,
				exit:     make(chan struct{}),
			}
			r.Unlock()

		// The graph prune ticker has ticked, so we'll examine the
		// state of the known graph to filter out any zombie channels
		// for pruning.
		case <-graphPruneTicker.C:
			if err := r.pruneZombieChans(); err != nil {
				log.Errorf("Unable to prune zombies: %v", err)
			}

		// Log any stats if we've processed a non-empty number of
		// channels, updates, or nodes. We'll only pause the ticker if
		// the last window contained no updates to avoid resuming and
		// pausing while consecutive windows contain new info.
		case <-r.statTicker.Ticks():
			if !r.stats.Empty() {
				log.Infof(r.stats.String())
			} else {
				r.statTicker.Pause()
			}
			r.stats.Reset()

		// The router has been signalled to exit, to we exit our main
		// loop so the wait group can be decremented.
		case <-r.quit:
			return
		}
	}
}

// assertNodeAnnFreshness returns a non-nil error if we have an announcement in
// the database for the passed node with a timestamp newer than the passed
// timestamp. ErrIgnored will be returned if we already have the node, and
// ErrOutdated will be returned if we have a timestamp that's after the new
// timestamp.
func (r *ChannelRouter) assertNodeAnnFreshness(node route.Vertex,
	msgTimestamp time.Time) error {

	// If we are not already aware of this node, it means that we don't
	// know about any channel using this node. To avoid a DoS attack by
	// node announcements, we will ignore such nodes. If we do know about
	// this node, check that this update brings info newer than what we
	// already have.
	lastUpdate, exists, err := r.cfg.Graph.HasLightningNode(node)
	if err != nil {
		return errors.Errorf("unable to query for the "+
			"existence of node: %v", err)
	}
	if !exists {
		return newErrf(ErrIgnored, "Ignoring node announcement"+
			" for node not found in channel graph (%x)",
			node[:])
	}

	// If we've reached this point then we're aware of the vertex being
	// advertised. So we now check if the new message has a new time stamp,
	// if not then we won't accept the new data as it would override newer
	// data.
	if !lastUpdate.Before(msgTimestamp) {
		return newErrf(ErrOutdated, "Ignoring outdated "+
			"announcement for %x", node[:])
	}

	return nil
}

// processUpdate processes a new relate authenticated channel/edge, node or
// channel/edge update network update. If the update didn't affect the internal
// state of the draft due to either being out of date, invalid, or redundant,
// then error is returned.
func (r *ChannelRouter) processUpdate(msg interface{}) error {
	switch msg := msg.(type) {
	case *channeldb.LightningNode:
		// Before we add the node to the database, we'll check to see
		// if the announcement is "fresh" or not. If it isn't, then
		// we'll return an error.
		err := r.assertNodeAnnFreshness(msg.PubKeyBytes, msg.LastUpdate)
		if err != nil {
			return err
		}

		if err := r.cfg.Graph.AddLightningNode(msg); err != nil {
			return errors.Errorf("unable to add node %v to the "+
				"graph: %v", msg.PubKeyBytes, err)
		}

		log.Tracef("Updated vertex data for node=%x", msg.PubKeyBytes)
		r.stats.incNumNodeUpdates()

	case *channeldb.ChannelEdgeInfo:
		// Prior to processing the announcement we first check if we
		// already know of this channel, if so, then we can exit early.
		_, _, exists, isZombie, err := r.cfg.Graph.HasChannelEdge(
			msg.ChannelID,
		)
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			return errors.Errorf("unable to check for edge "+
				"existence: %v", err)
		}
		if isZombie {
			return newErrf(ErrIgnored, "ignoring msg for zombie "+
				"chan_id=%v", msg.ChannelID)
		}
		if exists {
			return newErrf(ErrIgnored, "ignoring msg for known "+
				"chan_id=%v", msg.ChannelID)
		}

		// If AssumeChannelValid is present, then we are unable to
		// perform any of the expensive checks below, so we'll
		// short-circuit our path straight to adding the edge to our
		// graph.
		if r.cfg.AssumeChannelValid {
			if err := r.cfg.Graph.AddChannelEdge(msg); err != nil {
				return fmt.Errorf("unable to add edge: %v", err)
			}
			log.Tracef("New channel discovered! Link "+
				"connects %x and %x with ChannelID(%v)",
				msg.NodeKey1Bytes, msg.NodeKey2Bytes,
				msg.ChannelID)
			r.stats.incNumEdgesDiscovered()

			break
		}

		// Before we can add the channel to the channel graph, we need
		// to obtain the full funding outpoint that's encoded within
		// the channel ID.
		channelID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		fundingTx, err := r.fetchFundingTx(&channelID)
		if err != nil {
			return errors.Errorf("unable to fetch funding tx for "+
				"chan_id=%v: %v", msg.ChannelID, err)
		}

		// Recreate witness output to be sure that declared in channel
		// edge bitcoin keys and channel value corresponds to the
		// reality.
		witnessScript, err := input.GenMultiSigScript(
			msg.DecredKey1Bytes[:], msg.DecredKey2Bytes[:],
		)
		if err != nil {
			return err
		}
		pkScript, err := input.ScriptHashPkScript(witnessScript)
		if err != nil {
			return err
		}

		// Next we'll validate that this channel is actually well
		// formed. If this check fails, then this channel either
		// doesn't exist, or isn't the one that was meant to be created
		// according to the passed channel proofs.
		fundingPoint, err := chanvalidate.Validate(&chanvalidate.Context{
			Locator: &chanvalidate.ShortChanIDChanLocator{
				ID: channelID,
			},
			MultiSigPkScript: pkScript,
			FundingTx:        fundingTx,
		})
		if err != nil {
			return err
		}

		// Now that we have the funding outpoint of the channel, ensure
		// that it hasn't yet been spent. If so, then this channel has
		// been closed so we'll ignore it.
		fundingPkScript, err := input.ScriptHashPkScript(witnessScript)
		if err != nil {
			return err
		}
		chanUtxo, err := r.cfg.Chain.GetUtxo(
			fundingPoint, fundingPkScript, channelID.BlockHeight,
			r.quit,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch utxo "+
				"for chan_id=%v, chan_point=%v: %v",
				msg.ChannelID, fundingPoint, err)
		}

		// TODO(roasbeef): this is a hack, needs to be removed
		// after commitment fees are dynamic.
		msg.Capacity = dcrutil.Amount(chanUtxo.Value)
		msg.ChannelPoint = *fundingPoint
		if err := r.cfg.Graph.AddChannelEdge(msg); err != nil {
			return errors.Errorf("unable to add edge: %v", err)
		}

		log.Tracef("New channel discovered! Link "+
			"connects %x and %x with ChannelPoint(%v): "+
			"chan_id=%v, capacity=%v",
			msg.NodeKey1Bytes, msg.NodeKey2Bytes,
			fundingPoint, msg.ChannelID, msg.Capacity)
		r.stats.incNumEdgesDiscovered()

		// As a new edge has been added to the channel graph, we'll
		// update the current UTXO filter within our active
		// FilteredChainView so we are notified if/when this channel is
		// closed.
		filterUpdate := []channeldb.EdgePoint{
			{
				FundingPkScript: fundingPkScript,
				OutPoint:        *fundingPoint,
			},
		}
		err = r.cfg.ChainView.UpdateFilter(
			filterUpdate, int64(atomic.LoadUint32(&r.bestHeight)),
		)
		if err != nil {
			return errors.Errorf("unable to update chain "+
				"view: %v", err)
		}

	case *channeldb.ChannelEdgePolicy:
		// We make sure to hold the mutex for this channel ID,
		// such that no other goroutine is concurrently doing
		// database accesses for the same channel ID.
		r.channelEdgeMtx.Lock(msg.ChannelID)
		defer r.channelEdgeMtx.Unlock(msg.ChannelID)

		edge1Timestamp, edge2Timestamp, exists, isZombie, err :=
			r.cfg.Graph.HasChannelEdge(msg.ChannelID)
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			return errors.Errorf("unable to check for edge "+
				"existence: %v", err)

		}

		// If the channel is marked as a zombie in our database, and
		// we consider this a stale update, then we should not apply the
		// policy.
		isStaleUpdate := time.Since(msg.LastUpdate) > r.cfg.ChannelPruneExpiry
		if isZombie && isStaleUpdate {
			return newErrf(ErrIgnored, "ignoring stale update "+
				"(flags=%v|%v) for zombie chan_id=%v",
				msg.MessageFlags, msg.ChannelFlags,
				msg.ChannelID)
		}

		// If the channel doesn't exist in our database, we cannot
		// apply the updated policy.
		if !exists {
			return newErrf(ErrIgnored, "ignoring update "+
				"(flags=%v|%v) for unknown chan_id=%v",
				msg.MessageFlags, msg.ChannelFlags,
				msg.ChannelID)
		}

		// As edges are directional edge node has a unique policy for
		// the direction of the edge they control. Therefore we first
		// check if we already have the most up to date information for
		// that edge. If this message has a timestamp not strictly
		// newer than what we already know of we can exit early.
		switch {

		// A flag set of 0 indicates this is an announcement for the
		// "first" node in the channel.
		case msg.ChannelFlags&lnwire.ChanUpdateDirection == 0:

			// Ignore outdated message.
			if !edge1Timestamp.Before(msg.LastUpdate) {
				return newErrf(ErrOutdated, "Ignoring "+
					"outdated update (flags=%v|%v) for "+
					"known chan_id=%v", msg.MessageFlags,
					msg.ChannelFlags, msg.ChannelID)
			}

		// Similarly, a flag set of 1 indicates this is an announcement
		// for the "second" node in the channel.
		case msg.ChannelFlags&lnwire.ChanUpdateDirection == 1:

			// Ignore outdated message.
			if !edge2Timestamp.Before(msg.LastUpdate) {
				return newErrf(ErrOutdated, "Ignoring "+
					"outdated update (flags=%v|%v) for "+
					"known chan_id=%v", msg.MessageFlags,
					msg.ChannelFlags, msg.ChannelID)
			}
		}

		// Now that we know this isn't a stale update, we'll apply the
		// new edge policy to the proper directional edge within the
		// channel graph.
		if err = r.cfg.Graph.UpdateEdgePolicy(msg); err != nil {
			err := errors.Errorf("unable to add channel: %v", err)
			log.Error(err)
			return err
		}

		log.Tracef("New channel update applied: %v",
			newLogClosure(func() string { return spew.Sdump(msg) }))
		r.stats.incNumChannelUpdates()

	default:
		return errors.Errorf("wrong routing update message type")
	}

	r.statTicker.Resume()

	return nil
}

// fetchFundingTx returns the funding transaction identified by the passed
// short channel ID.
//
// TODO(roasbeef): replace with call to GetBlockTransaction? (would allow to
// later use getblocktxn)
func (r *ChannelRouter) fetchFundingTx(
	chanID *lnwire.ShortChannelID) (*wire.MsgTx, error) {

	// First fetch the block hash by the block number encoded, then use
	// that hash to fetch the block itself.
	blockNum := int64(chanID.BlockHeight)
	blockHash, err := r.cfg.Chain.GetBlockHash(blockNum)
	if err != nil {
		return nil, err
	}
	fundingBlock, err := r.cfg.Chain.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	// As a sanity check, ensure that the advertised transaction index is
	// within the bounds of the total number of transactions within a
	// block.
	numTxns := uint32(len(fundingBlock.Transactions))
	if chanID.TxIndex > numTxns-1 {
		return nil, fmt.Errorf("tx_index=#%v is out of range "+
			"(max_index=%v), network_chan_id=%v", chanID.TxIndex,
			numTxns-1, chanID)
	}

	return fundingBlock.Transactions[chanID.TxIndex], nil
}

// routingMsg couples a routing related routing topology update to the
// error channel.
type routingMsg struct {
	msg interface{}
	err chan error
}

// FindRoute attempts to query the ChannelRouter for the optimum path to a
// particular target destination to which it is able to send `amt` after
// factoring in channel capacities and cumulative fees along the route.
func (r *ChannelRouter) FindRoute(source, target route.Vertex,
	amt lnwire.MilliAtom, restrictions *RestrictParams,
	destTlvRecords []tlv.Record,
	finalExpiry ...uint16) (*route.Route, error) {

	var finalCLTVDelta uint16
	if len(finalExpiry) == 0 {
		finalCLTVDelta = zpay32.DefaultFinalCLTVDelta
	} else {
		finalCLTVDelta = finalExpiry[0]
	}

	log.Debugf("Searching for path to %x, sending %v", target, amt)

	// We can short circuit the routing by opportunistically checking to
	// see if the target vertex event exists in the current graph.
	if _, exists, err := r.cfg.Graph.HasLightningNode(target); err != nil {
		return nil, err
	} else if !exists {
		log.Debugf("Target %x is not in known graph", target)
		return nil, newErrf(ErrTargetNotInNetwork, "target not found")
	}

	// We'll attempt to obtain a set of bandwidth hints that can help us
	// eliminate certain routes early on in the path finding process.
	bandwidthHints, err := generateBandwidthHints(
		r.selfNode, r.cfg.QueryBandwidth,
	)
	if err != nil {
		return nil, err
	}

	// Now that we know the destination is reachable within the graph, we'll
	// execute our path finding algorithm.
	path, err := findPath(
		&graphParams{
			graph:          r.cfg.Graph,
			bandwidthHints: bandwidthHints,
		},
		restrictions, &r.cfg.PathFindingConfig,
		source, target, amt,
	)
	if err != nil {
		return nil, err
	}

	// We'll fetch the current block height so we can properly calculate the
	// required HTLC time locks within the route.
	_, currentHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// Create the route with absolute time lock values.
	route, err := newRoute(
		amt, source, path, uint32(currentHeight), finalCLTVDelta,
		destTlvRecords,
	)
	if err != nil {
		return nil, err
	}

	go log.Tracef("Obtained path to send %v to %x: %v",
		amt, target, newLogClosure(func() string {
			return spew.Sdump(route)
		}),
	)

	return route, nil
}

// generateNewSessionKey generates a new ephemeral private key to be used for a
// payment attempt.
func generateNewSessionKey() (*secp256k1.PrivateKey, error) {
	// Generate a new random session key to ensure that we don't trigger
	// any replay.
	//
	// TODO(roasbeef): add more sources of randomness?
	return secp256k1.GeneratePrivateKey()
}

// generateSphinxPacket generates then encodes a sphinx packet which encodes
// the onion route specified by the passed layer 3 route. The blob returned
// from this function can immediately be included within an HTLC add packet to
// be sent to the first hop within the route.
func generateSphinxPacket(rt *route.Route, paymentHash []byte,
	sessionKey *secp256k1.PrivateKey) ([]byte, *sphinx.Circuit, error) {

	// As a sanity check, we'll ensure that the set of hops has been
	// properly filled in, otherwise, we won't actually be able to
	// construct a route.
	if len(rt.Hops) == 0 {
		return nil, nil, route.ErrNoRouteHopsProvided
	}

	// Now that we know we have an actual route, we'll map the route into a
	// sphinx payument path which includes per-hop paylods for each hop
	// that give each node within the route the necessary information
	// (fees, CLTV value, etc) to properly forward the payment.
	sphinxPath, err := rt.ToSphinxPath()
	if err != nil {
		return nil, nil, err
	}

	log.Tracef("Constructed per-hop payloads for payment_hash=%x: %v",
		paymentHash[:], newLogClosure(func() string {
			path := make([]sphinx.OnionHop, sphinxPath.TrueRouteLength())
			for i := range path {
				hopCopy := sphinxPath[i]
				hopCopy.NodePub.Curve = nil
				path[i] = hopCopy
			}
			return spew.Sdump(path)
		}),
	)

	// Next generate the onion routing packet which allows us to perform
	// privacy preserving source routing across the network.
	sphinxPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, paymentHash,
	)
	if err != nil {
		return nil, nil, err
	}

	// Finally, encode Sphinx packet using its wire representation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := sphinxPacket.Encode(&onionBlob); err != nil {
		return nil, nil, err
	}

	log.Tracef("Generated sphinx packet: %v",
		newLogClosure(func() string {
			// We make a copy of the ephemeral key and unset the
			// internal curve here in order to keep the logs from
			// getting noisy.
			key := *sphinxPacket.EphemeralKey
			key.Curve = nil
			packetCopy := *sphinxPacket
			packetCopy.EphemeralKey = &key
			return spew.Sdump(packetCopy)
		}),
	)

	return onionBlob.Bytes(), &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: sphinxPath.NodeKeys(),
	}, nil
}

// LightningPayment describes a payment to be sent through the network to the
// final destination.
type LightningPayment struct {
	// Target is the node in which the payment should be routed towards.
	Target route.Vertex

	// Amount is the value of the payment to send through the network in
	// milli-atoms.
	Amount lnwire.MilliAtom

	// FeeLimit is the maximum fee in MilliAtoms that the payment should
	// accept when sending it through the network. The payment will fail
	// if there isn't a route with lower fees than this limit.
	FeeLimit lnwire.MilliAtom

	// CltvLimit is the maximum time lock that is allowed for attempts to
	// complete this payment.
	CltvLimit uint32

	// PaymentHash is the r-hash value to use within the HTLC extended to
	// the first hop.
	PaymentHash [32]byte

	// FinalCLTVDelta is the CTLV expiry delta to use for the _final_ hop
	// in the route. This means that the final hop will have a CLTV delta
	// of at least: currentHeight + FinalCLTVDelta.
	FinalCLTVDelta uint16

	// PayAttemptTimeout is a timeout value that we'll use to determine
	// when we should should abandon the payment attempt after consecutive
	// payment failure. This prevents us from attempting to send a payment
	// indefinitely. A zero value means the payment will never time out.
	//
	// TODO(halseth): make wallclock time to allow resume after startup.
	PayAttemptTimeout time.Duration

	// RouteHints represents the different routing hints that can be used to
	// assist a payment in reaching its destination successfully. These
	// hints will act as intermediate hops along the route.
	//
	// NOTE: This is optional unless required by the payment. When providing
	// multiple routes, ensure the hop hints within each route are chained
	// together and sorted in forward order in order to reach the
	// destination successfully.
	RouteHints [][]zpay32.HopHint

	// OutgoingChannelID is the channel that needs to be taken to the first
	// hop. If nil, any channel may be used.
	OutgoingChannelID *uint64

	// PaymentRequest is an optional payment request that this payment is
	// attempting to complete.
	PaymentRequest []byte

	// FinalDestRecords are TLV records that are to be sent to the final
	// hop in the new onion payload format. If the destination does not
	// understand this new onion payload format, then the payment will
	// fail.
	FinalDestRecords []tlv.Record
}

// SendPayment attempts to send a payment as described within the passed
// LightningPayment. This function is blocking and will return either: when the
// payment is successful, or all candidates routes have been attempted and
// resulted in a failed payment. If the payment succeeds, then a non-nil Route
// will be returned which describes the path the successful payment traversed
// within the network to reach the destination. Additionally, the payment
// preimage will also be returned.
func (r *ChannelRouter) SendPayment(payment *LightningPayment) ([32]byte,
	*route.Route, error) {

	paySession, err := r.preparePayment(payment)
	if err != nil {
		return [32]byte{}, nil, err
	}

	// Since this is the first time this payment is being made, we pass nil
	// for the existing attempt.
	return r.sendPayment(nil, payment, paySession)
}

// SendPaymentAsync is the non-blocking version of SendPayment. The payment
// result needs to be retrieved via the control tower.
func (r *ChannelRouter) SendPaymentAsync(payment *LightningPayment) error {
	paySession, err := r.preparePayment(payment)
	if err != nil {
		return err
	}

	// Since this is the first time this payment is being made, we pass nil
	// for the existing attempt.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		_, _, err := r.sendPayment(nil, payment, paySession)
		if err != nil {
			log.Errorf("Payment with hash %x failed: %v",
				payment.PaymentHash, err)
		}
	}()

	return nil
}

// preparePayment creates the payment session and registers the payment with the
// control tower.
func (r *ChannelRouter) preparePayment(payment *LightningPayment) (
	PaymentSession, error) {

	// Before starting the HTLC routing attempt, we'll create a fresh
	// payment session which will report our errors back to mission
	// control.
	paySession, err := r.cfg.SessionSource.NewPaymentSession(
		payment.RouteHints, payment.Target,
	)
	if err != nil {
		return nil, err
	}

	// Record this payment hash with the ControlTower, ensuring it is not
	// already in-flight.
	//
	// TODO(roasbeef): store records as part of creation info?
	info := &channeldb.PaymentCreationInfo{
		PaymentHash:    payment.PaymentHash,
		Value:          payment.Amount,
		CreationDate:   time.Now(),
		PaymentRequest: payment.PaymentRequest,
	}

	err = r.cfg.Control.InitPayment(payment.PaymentHash, info)
	if err != nil {
		return nil, err
	}

	return paySession, nil
}

// SendToRoute attempts to send a payment with the given hash through the
// provided route. This function is blocking and will return the obtained
// preimage if the payment is successful or the full error in case of a failure.
func (r *ChannelRouter) SendToRoute(hash lntypes.Hash, route *route.Route) (
	lntypes.Preimage, error) {

	// Create a payment session for just this route.
	paySession := r.cfg.SessionSource.NewPaymentSessionForRoute(route)

	// Calculate amount paid to receiver.
	amt := route.TotalAmount - route.TotalFees()

	// Record this payment hash with the ControlTower, ensuring it is not
	// already in-flight.
	info := &channeldb.PaymentCreationInfo{
		PaymentHash:    hash,
		Value:          amt,
		CreationDate:   time.Now(),
		PaymentRequest: nil,
	}

	err := r.cfg.Control.InitPayment(hash, info)
	if err != nil {
		return [32]byte{}, err
	}

	// Create a (mostly) dummy payment, as the created payment session is
	// not going to do path finding.
	// TODO(halseth): sendPayment doesn't really need LightningPayment, make
	// it take just needed fields instead.
	//
	// PayAttemptTime doesn't need to be set, as there is only a single
	// attempt.
	payment := &LightningPayment{
		PaymentHash: hash,
	}

	// Since this is the first time this payment is being made, we pass nil
	// for the existing attempt.
	preimage, _, err := r.sendPayment(nil, payment, paySession)
	if err != nil {
		// SendToRoute should return a structured error. In case the
		// provided route fails, payment lifecycle will return a
		// noRouteError with the structured error embedded.
		if noRouteError, ok := err.(errNoRoute); ok {
			if noRouteError.lastError == nil {
				return lntypes.Preimage{},
					errors.New("failure message missing")
			}

			return lntypes.Preimage{}, noRouteError.lastError
		}

		return lntypes.Preimage{}, err
	}

	return preimage, nil
}

// sendPayment attempts to send a payment as described within the passed
// LightningPayment. This function is blocking and will return either: when the
// payment is successful, or all candidates routes have been attempted and
// resulted in a failed payment. If the payment succeeds, then a non-nil Route
// will be returned which describes the path the successful payment traversed
// within the network to reach the destination. Additionally, the payment
// preimage will also be returned.
//
// The existing attempt argument should be set to nil if this is a payment that
// haven't had any payment attempt sent to the switch yet. If it has had an
// attempt already, it should be passed such that the result can be retrieved.
//
// This method relies on the ControlTower's internal payment state machine to
// carry out its execution. After restarts it is safe, and assumed, that the
// router will call this method for every payment still in-flight according to
// the ControlTower.
func (r *ChannelRouter) sendPayment(
	existingAttempt *channeldb.PaymentAttemptInfo,
	payment *LightningPayment, paySession PaymentSession) (
	[32]byte, *route.Route, error) {

	log.Tracef("Dispatching route for lightning payment: %v",
		newLogClosure(func() string {
			// Make a copy of the payment with a nilled Curve
			// before spewing.
			var routeHints [][]zpay32.HopHint
			for _, routeHint := range payment.RouteHints {
				var hopHints []zpay32.HopHint
				for _, hopHint := range routeHint {
					h := hopHint.Copy()
					h.NodeID.Curve = nil
					hopHints = append(hopHints, h)
				}
				routeHints = append(routeHints, hopHints)
			}
			p := *payment
			p.RouteHints = routeHints
			return spew.Sdump(p)
		}),
	)

	// We'll also fetch the current block height so we can properly
	// calculate the required HTLC time locks within the route.
	_, currentHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return [32]byte{}, nil, err
	}

	// Now set up a paymentLifecycle struct with these params, such that we
	// can resume the payment from the current state.
	p := &paymentLifecycle{
		router:         r,
		payment:        payment,
		paySession:     paySession,
		currentHeight:  currentHeight,
		finalCLTVDelta: payment.FinalCLTVDelta,
		attempt:        existingAttempt,
		circuit:        nil,
		lastError:      nil,
	}

	// If a timeout is specified, create a timeout channel. If no timeout is
	// specified, the channel is left nil and will never abort the payment
	// loop.
	if payment.PayAttemptTimeout != 0 {
		p.timeoutChan = time.After(payment.PayAttemptTimeout)
	}

	return p.resumePayment()

}

// tryApplyChannelUpdate tries to apply a channel update present in the failure
// message if any.
func (r *ChannelRouter) tryApplyChannelUpdate(rt *route.Route,
	errorSourceIdx int, failure lnwire.FailureMessage) error {

	// It makes no sense to apply our own channel updates.
	if errorSourceIdx == 0 {
		log.Errorf("Channel update of ourselves received")

		return nil
	}

	// Extract channel update if the error contains one.
	update := r.extractChannelUpdate(failure)
	if update == nil {
		return nil
	}

	// Parse pubkey to allow validation of the channel update. This should
	// always succeed, otherwise there is something wrong in our
	// implementation. Therefore return an error.
	errVertex := rt.Hops[errorSourceIdx-1].PubKeyBytes
	errSource, err := secp256k1.ParsePubKey(
		errVertex[:],
	)
	if err != nil {
		log.Errorf("Cannot parse pubkey: idx=%v, pubkey=%v",
			errorSourceIdx, errVertex)

		return err
	}

	// Apply channel update.
	if !r.applyChannelUpdate(update, errSource) {
		log.Debugf("Invalid channel update received: node=%x",
			errVertex)
	}

	return nil
}

// processSendError analyzes the error for the payment attempt received from the
// switch and updates mission control and/or channel policies. Depending on the
// error type, this error is either the final outcome of the payment or we need
// to continue with an alternative route. This is indicated by the boolean
// return value.
func (r *ChannelRouter) processSendError(paymentID uint64, rt *route.Route,
	sendErr error) *channeldb.FailureReason {

	internalErrorReason := channeldb.FailureReasonError

	reportFail := func(srcIdx *int,
		msg lnwire.FailureMessage) *channeldb.FailureReason {

		// Report outcome to mission control.
		reason, err := r.cfg.MissionControl.ReportPaymentFail(
			paymentID, rt, srcIdx, msg,
		)
		if err != nil {
			log.Errorf("Error reporting payment result to mc: %v",
				err)

			return &internalErrorReason
		}

		return reason
	}

	if sendErr == htlcswitch.ErrUnreadableFailureMessage {
		log.Tracef("Unreadable failure when sending htlc")

		return reportFail(nil, nil)
	}
	// If an internal, non-forwarding error occurred, we can stop
	// trying.
	fErr, ok := sendErr.(*htlcswitch.ForwardingError)
	if !ok {
		return &internalErrorReason
	}

	failureMessage := fErr.FailureMessage
	failureSourceIdx := fErr.FailureSourceIdx

	// Apply channel update if the error contains one. For unknown
	// failures, failureMessage is nil.
	if failureMessage != nil {
		err := r.tryApplyChannelUpdate(
			rt, failureSourceIdx, failureMessage,
		)
		if err != nil {
			return &internalErrorReason
		}
	}

	log.Tracef("Node=%v reported failure when sending htlc",
		failureSourceIdx)

	return reportFail(&failureSourceIdx, failureMessage)
}

// extractChannelUpdate examines the error and extracts the channel update.
func (r *ChannelRouter) extractChannelUpdate(
	failure lnwire.FailureMessage) *lnwire.ChannelUpdate {

	var update *lnwire.ChannelUpdate
	switch onionErr := failure.(type) {
	case *lnwire.FailExpiryTooSoon:
		update = &onionErr.Update
	case *lnwire.FailAmountBelowMinimum:
		update = &onionErr.Update
	case *lnwire.FailFeeInsufficient:
		update = &onionErr.Update
	case *lnwire.FailIncorrectCltvExpiry:
		update = &onionErr.Update
	case *lnwire.FailChannelDisabled:
		update = &onionErr.Update
	case *lnwire.FailTemporaryChannelFailure:
		update = onionErr.Update
	}

	return update
}

// applyChannelUpdate validates a channel update and if valid, applies it to
// the database. It returns a bool indicating whether the updates was
// successful.
func (r *ChannelRouter) applyChannelUpdate(msg *lnwire.ChannelUpdate,
	pubKey *secp256k1.PublicKey) bool {

	ch, _, _, err := r.GetChannelByID(msg.ShortChannelID)
	if err != nil {
		log.Errorf("Unable to retrieve channel by id: %v", err)
		return false
	}

	if err := ValidateChannelUpdateAnn(pubKey, ch.Capacity, msg); err != nil {
		log.Errorf("Unable to validate channel update: %v", err)
		return false
	}

	err = r.UpdateEdge(&channeldb.ChannelEdgePolicy{
		SigBytes:                  msg.Signature.ToSignatureBytes(),
		ChannelID:                 msg.ShortChannelID.ToUint64(),
		LastUpdate:                time.Unix(int64(msg.Timestamp), 0),
		MessageFlags:              msg.MessageFlags,
		ChannelFlags:              msg.ChannelFlags,
		TimeLockDelta:             msg.TimeLockDelta,
		MinHTLC:                   msg.HtlcMinimumMAtoms,
		MaxHTLC:                   msg.HtlcMaximumMAtoms,
		FeeBaseMAtoms:             lnwire.MilliAtom(msg.BaseFee),
		FeeProportionalMillionths: lnwire.MilliAtom(msg.FeeRate),
	})
	if err != nil && !IsError(err, ErrIgnored, ErrOutdated) {
		log.Errorf("Unable to apply channel update: %v", err)
		return false
	}

	return true
}

// AddNode is used to add information about a node to the router database. If
// the node with this pubkey is not present in an existing channel, it will
// be ignored.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddNode(node *channeldb.LightningNode) error {
	rMsg := &routingMsg{
		msg: node,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		select {
		case err := <-rMsg.err:
			return err
		case <-r.quit:
			return ErrRouterShuttingDown
		}
	case <-r.quit:
		return ErrRouterShuttingDown
	}
}

// AddEdge is used to add edge/channel to the topology of the router, after all
// information about channel will be gathered this edge/channel might be used
// in construction of payment path.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddEdge(edge *channeldb.ChannelEdgeInfo) error {
	rMsg := &routingMsg{
		msg: edge,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		select {
		case err := <-rMsg.err:
			return err
		case <-r.quit:
			return ErrRouterShuttingDown
		}
	case <-r.quit:
		return ErrRouterShuttingDown
	}
}

// UpdateEdge is used to update edge information, without this message edge
// considered as not fully constructed.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) UpdateEdge(update *channeldb.ChannelEdgePolicy) error {
	rMsg := &routingMsg{
		msg: update,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		select {
		case err := <-rMsg.err:
			return err
		case <-r.quit:
			return ErrRouterShuttingDown
		}
	case <-r.quit:
		return ErrRouterShuttingDown
	}
}

// CurrentBlockHeight returns the block height from POV of the router subsystem.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) CurrentBlockHeight() (uint32, error) {
	_, height, err := r.cfg.Chain.GetBestBlock()
	return uint32(height), err
}

// GetChannelByID return the channel by the channel id.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) GetChannelByID(chanID lnwire.ShortChannelID) (
	*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy,
	*channeldb.ChannelEdgePolicy, error) {

	return r.cfg.Graph.FetchChannelEdgesByID(chanID.ToUint64())
}

// FetchLightningNode attempts to look up a target node by its identity public
// key. channeldb.ErrGraphNodeNotFound is returned if the node doesn't exist
// within the graph.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) FetchLightningNode(node route.Vertex) (*channeldb.LightningNode, error) {
	pubKey, err := secp256k1.ParsePubKey(node[:])
	if err != nil {
		return nil, fmt.Errorf("unable to parse raw public key: %v", err)
	}
	return r.cfg.Graph.FetchLightningNode(pubKey)
}

// ForEachNode is used to iterate over every node in router topology.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForEachNode(cb func(*channeldb.LightningNode) error) error {
	return r.cfg.Graph.ForEachNode(nil, func(_ *bolt.Tx, n *channeldb.LightningNode) error {
		return cb(n)
	})
}

// ForAllOutgoingChannels is used to iterate over all outgoing channels owned by
// the router.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForAllOutgoingChannels(cb func(*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy) error) error {
	return r.selfNode.ForEachChannel(nil,
		func(_ *bolt.Tx, c *channeldb.ChannelEdgeInfo, e,
			_ *channeldb.ChannelEdgePolicy) error {
			if e == nil {
				return fmt.Errorf("channel from self node has no policy")
			}

			return cb(c, e)
		})
}

// ForEachChannel is used to iterate over every known edge (channel) within our
// view of the channel graph.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForEachChannel(cb func(chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) error) error {

	return r.cfg.Graph.ForEachChannel(cb)
}

// AddProof updates the channel edge info with proof which is needed to
// properly announce the edge to the rest of the network.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddProof(chanID lnwire.ShortChannelID,
	proof *channeldb.ChannelAuthProof) error {

	info, _, _, err := r.cfg.Graph.FetchChannelEdgesByID(chanID.ToUint64())
	if err != nil {
		return err
	}

	info.AuthProof = proof
	return r.cfg.Graph.UpdateChannelEdge(info)
}

// IsStaleNode returns true if the graph source has a node announcement for the
// target node with a more recent timestamp.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) IsStaleNode(node route.Vertex, timestamp time.Time) bool {
	// If our attempt to assert that the node announcement is fresh fails,
	// then we know that this is actually a stale announcement.
	return r.assertNodeAnnFreshness(node, timestamp) != nil
}

// IsPublicNode determines whether the given vertex is seen as a public node in
// the graph from the graph's source node's point of view.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) IsPublicNode(node route.Vertex) (bool, error) {
	return r.cfg.Graph.IsPublicNode(node)
}

// IsKnownEdge returns true if the graph source already knows of the passed
// channel ID either as a live or zombie edge.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) IsKnownEdge(chanID lnwire.ShortChannelID) bool {
	_, _, exists, isZombie, _ := r.cfg.Graph.HasChannelEdge(chanID.ToUint64())
	return exists || isZombie
}

// IsStaleEdgePolicy returns true if the graph source has a channel edge for
// the passed channel ID (and flags) that have a more recent timestamp.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) IsStaleEdgePolicy(chanID lnwire.ShortChannelID,
	timestamp time.Time, flags lnwire.ChanUpdateChanFlags) bool {

	edge1Timestamp, edge2Timestamp, exists, isZombie, err :=
		r.cfg.Graph.HasChannelEdge(chanID.ToUint64())
	if err != nil {
		return false

	}

	// If we know of the edge as a zombie, then we'll make some additional
	// checks to determine if the new policy is fresh.
	if isZombie {
		// When running with AssumeChannelValid, we also prune channels
		// if both of their edges are disabled. We'll mark the new
		// policy as stale if it remains disabled.
		if r.cfg.AssumeChannelValid {
			isDisabled := flags&lnwire.ChanUpdateDisabled ==
				lnwire.ChanUpdateDisabled
			if isDisabled {
				return true
			}
		}

		// Otherwise, we'll fall back to our usual ChannelPruneExpiry.
		return time.Since(timestamp) > r.cfg.ChannelPruneExpiry
	}

	// If we don't know of the edge, then it means it's fresh (thus not
	// stale).
	if !exists {
		return false
	}

	// As edges are directional edge node has a unique policy for the
	// direction of the edge they control. Therefore we first check if we
	// already have the most up to date information for that edge. If so,
	// then we can exit early.
	switch {
	// A flag set of 0 indicates this is an announcement for the "first"
	// node in the channel.
	case flags&lnwire.ChanUpdateDirection == 0:
		return !edge1Timestamp.Before(timestamp)

	// Similarly, a flag set of 1 indicates this is an announcement for the
	// "second" node in the channel.
	case flags&lnwire.ChanUpdateDirection == 1:
		return !edge2Timestamp.Before(timestamp)
	}

	return false
}

// MarkEdgeLive clears an edge from our zombie index, deeming it as live.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) MarkEdgeLive(chanID lnwire.ShortChannelID) error {
	return r.cfg.Graph.MarkEdgeLive(chanID.ToUint64())
}

// generateBandwidthHints is a helper function that's utilized the main
// findPath function in order to obtain hints from the lower layer w.r.t to the
// available bandwidth of edges on the network. Currently, we'll only obtain
// bandwidth hints for the edges we directly have open ourselves. Obtaining
// these hints allows us to reduce the number of extraneous attempts as we can
// skip channels that are inactive, or just don't have enough bandwidth to
// carry the payment.
func generateBandwidthHints(sourceNode *channeldb.LightningNode,
	queryBandwidth func(*channeldb.ChannelEdgeInfo) lnwire.MilliAtom) (map[uint64]lnwire.MilliAtom, error) {

	// First, we'll collect the set of outbound edges from the target
	// source node.
	var localChans []*channeldb.ChannelEdgeInfo
	err := sourceNode.ForEachChannel(nil, func(tx *bolt.Tx,
		edgeInfo *channeldb.ChannelEdgeInfo,
		_, _ *channeldb.ChannelEdgePolicy) error {

		localChans = append(localChans, edgeInfo)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Now that we have all of our outbound edges, we'll populate the set
	// of bandwidth hints, querying the lower switch layer for the most up
	// to date values.
	bandwidthHints := make(map[uint64]lnwire.MilliAtom)
	for _, localChan := range localChans {
		bandwidthHints[localChan.ChannelID] = queryBandwidth(localChan)
	}

	return bandwidthHints, nil
}

// runningAmounts keeps running amounts while the route is traversed.
type runningAmounts struct {
	// amt is the intended amount to send via the route.
	amt lnwire.MilliAtom

	// max is the running maximum that the route can carry.
	max lnwire.MilliAtom
}

// prependChannel returns a new set of running amounts that would result from
// prepending the given channel to the route. If canIncreaseAmt is set, the
// amount may be increased if it is too small to satisfy the channel's minimum
// htlc amount.
func (r *runningAmounts) prependChannel(policy *channeldb.ChannelEdgePolicy,
	capacity dcrutil.Amount, localChan bool, canIncreaseAmt bool) (
	runningAmounts, error) {

	// Determine max htlc value.
	maxHtlc := lnwire.NewMAtomsFromAtoms(capacity)
	if policy.MessageFlags.HasMaxHtlc() {
		maxHtlc = policy.MaxHTLC
	}

	amt := r.amt

	// If we have a specific amount for which we are building the route,
	// validate it against the channel constraints and return the new
	// running amount.
	if !canIncreaseAmt {
		if amt < policy.MinHTLC || amt > maxHtlc {
			return runningAmounts{}, fmt.Errorf("channel htlc "+
				"constraints [%v - %v] violated with amt %v",
				policy.MinHTLC, maxHtlc, amt)
		}

		// Update running amount by adding the fee for non-local
		// channels.
		if !localChan {
			amt += policy.ComputeFee(amt)
		}

		return runningAmounts{
			amt: amt,
		}, nil
	}

	// Adapt the minimum amount to what this channel allows.
	if policy.MinHTLC > r.amt {
		amt = policy.MinHTLC
	}

	// Update the maximum amount too to be able to detect incompatible
	// channels.
	max := r.max
	if maxHtlc < r.max {
		max = maxHtlc
	}

	// If we get in the situation that the minimum amount exceeds the
	// maximum amount (enforced further down stream), we have incompatible
	// channel policies.
	//
	// There is possibility with pubkey addressing that we should have
	// selected a different channel downstream, but we don't backtrack to
	// try to fix that. It would complicate path finding while we expect
	// this situation to be rare. The spec recommends to keep all policies
	// towards a peer identical. If that is the case, there isn't a better
	// channel that we should have selected.
	if amt > max {
		return runningAmounts{},
			fmt.Errorf("incompatible channel policies: %v "+
				"exceeds %v", amt, max)
	}

	// Add fees to the running amounts. Skip the source node fees as
	// those do not need to be paid.
	if !localChan {
		amt += policy.ComputeFee(amt)
		max += policy.ComputeFee(max)
	}

	return runningAmounts{amt: amt, max: max}, nil
}

// ErrNoChannel is returned when a route cannot be built because there are no
// channels that satisfy all requirements.
type ErrNoChannel struct {
	position int
	fromNode route.Vertex
}

// Error returns a human readable string describing the error.
func (e ErrNoChannel) Error() string {
	return fmt.Sprintf("no matching outgoing channel available for "+
		"node %v (%v)", e.position, e.fromNode)
}

// BuildRoute returns a fully specified route based on a list of pubkeys. If
// amount is nil, the minimum routable amount is used. To force a specific
// outgoing channel, use the outgoingChan parameter.
func (r *ChannelRouter) BuildRoute(amt *lnwire.MilliAtom,
	hops []route.Vertex, outgoingChan *uint64,
	finalCltvDelta int32) (*route.Route, error) {

	log.Tracef("BuildRoute called: hopsCount=%v, amt=%v",
		len(hops), amt)

	// If no amount is specified, we need to build a route for the minimum
	// amount that this route can carry.
	useMinAmt := amt == nil

	// We'll attempt to obtain a set of bandwidth hints that helps us select
	// the best outgoing channel to use in case no outgoing channel is set.
	bandwidthHints, err := generateBandwidthHints(
		r.selfNode, r.cfg.QueryBandwidth,
	)
	if err != nil {
		return nil, err
	}

	// Allocate a list that will contain the selected channels for this
	// route.
	edges := make([]*channeldb.ChannelEdgePolicy, len(hops))

	// Keep a running amount and the maximum for this route.
	amts := runningAmounts{
		max: lnwire.MilliAtom(^uint64(0)),
	}
	if useMinAmt {
		// For minimum amount routes, aim to deliver at least 1 msat to
		// the destination. There are nodes in the wild that have a
		// min_htlc channel policy of zero, which could lead to a zero
		// amount payment being made.
		amts.amt = 1
	} else {
		// If an amount is specified, we need to build a route that
		// delivers exactly this amount to the final destination.
		amts.amt = *amt
	}

	// Traverse hops backwards to accumulate fees in the running amounts.
	source := r.selfNode.PubKeyBytes
	for i := len(hops) - 1; i >= 0; i-- {
		toNode := hops[i]

		var fromNode route.Vertex
		if i == 0 {
			fromNode = source
		} else {
			fromNode = hops[i-1]
		}

		localChan := i == 0

		// Iterate over candidate channels to select the channel
		// to use for the final route.
		var (
			bestEdge      *channeldb.ChannelEdgePolicy
			bestAmts      *runningAmounts
			bestBandwidth lnwire.MilliAtom
		)

		cb := func(tx *bolt.Tx,
			edgeInfo *channeldb.ChannelEdgeInfo,
			_, inEdge *channeldb.ChannelEdgePolicy) error {

			chanID := edgeInfo.ChannelID

			// Apply outgoing channel restriction is active.
			if localChan && outgoingChan != nil &&
				chanID != *outgoingChan {

				return nil
			}

			// No unknown policy channels.
			if inEdge == nil {
				return nil
			}

			// Before we can process the edge, we'll need to
			// fetch the node on the _other_ end of this
			// channel as we may later need to iterate over
			// the incoming edges of this node if we explore
			// it further.
			chanFromNode, err := edgeInfo.FetchOtherNode(
				tx, toNode[:],
			)
			if err != nil {
				return err
			}

			// Continue searching if this channel doesn't
			// connect with the previous hop.
			if chanFromNode.PubKeyBytes != fromNode {
				return nil
			}

			// Validate whether this channel's policy is satisfied
			// and obtain the new running amounts if this channel
			// was to be selected.
			newAmts, err := amts.prependChannel(
				inEdge, edgeInfo.Capacity, localChan,
				useMinAmt,
			)
			if err != nil {
				log.Tracef("Skipping chan %v: %v",
					inEdge.ChannelID, err)

				return nil
			}

			// If we already have a best edge, check whether this
			// edge is better.
			bandwidth := bandwidthHints[chanID]
			if bestEdge != nil {
				if localChan {
					// For local channels, better is defined
					// as having more bandwidth. We try to
					// maximize the chance that the returned
					// route succeeds.
					if bandwidth < bestBandwidth {
						return nil
					}
				} else {
					// For other channels, better is defined
					// as lower fees for the amount to send.
					// Normally all channels between two
					// nodes should have the same policy,
					// but in case not we minimize our cost
					// here. Regular path finding would do
					// the same.
					if newAmts.amt > bestAmts.amt {
						return nil
					}
				}
			}

			// If we get here, the current edge is better. Replace
			// the best.
			bestEdge = inEdge
			bestAmts = &newAmts
			bestBandwidth = bandwidth

			return nil
		}

		err := r.cfg.Graph.ForEachNodeChannel(nil, toNode[:], cb)
		if err != nil {
			return nil, err
		}

		// There is no matching channel. Stop building the route here.
		if bestEdge == nil {
			return nil, ErrNoChannel{
				fromNode: fromNode,
				position: i,
			}
		}

		log.Tracef("Select channel %v at position %v", bestEdge.ChannelID, i)

		edges[i] = bestEdge
		amts = *bestAmts
	}

	_, height, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, err
	}

	var receiverAmt lnwire.MilliAtom
	if useMinAmt {
		// We've calculated the minimum amount for the htlc that the
		// source node hands out. The newRoute call below expects the
		// amount that must reach the receiver after subtraction of fees
		// along the way. Iterate over all edges to calculate the
		// receiver amount.
		receiverAmt = amts.amt
		for _, edge := range edges[1:] {
			receiverAmt -= edge.ComputeFeeFromIncoming(receiverAmt)
		}
	} else {
		// Deliver the specified amount to the receiver.
		receiverAmt = *amt
	}

	// Build and return the final route.
	return newRoute(
		receiverAmt, source, edges, uint32(height),
		uint16(finalCltvDelta), nil,
	)
}
