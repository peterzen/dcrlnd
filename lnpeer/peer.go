package lnpeer

import (
	"net"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/lnwire"
)

// Peer is an interface which represents the remote lightning node inside our
// system.
type Peer interface {
	// SendMessage sends a variadic number of high-priority message to
	// remote peer.  The first argument denotes if the method should block
	// until the messages have been sent to the remote peer or an error is
	// returned, otherwise it returns immediately after queuing.
	SendMessage(sync bool, msgs ...lnwire.Message) error

	// SendMessageLazy sends a variadic number of low-priority message to
	// remote peer. The first argument denotes if the method should block
	// until the messages have been sent to the remote peer or an error is
	// returned, otherwise it returns immediately after queueing.
	SendMessageLazy(sync bool, msgs ...lnwire.Message) error

	// AddNewChannel adds a new channel to the peer. The channel should fail
	// to be added if the cancel channel is closed.
	AddNewChannel(channel *channeldb.OpenChannel, cancel <-chan struct{}) error

	// WipeChannel removes the channel uniquely identified by its channel
	// point from all indexes associated with the peer.
	WipeChannel(*wire.OutPoint) error

	// PubKey returns the serialized public key of the remote peer.
	PubKey() [33]byte

	// IdentityKey returns the public key of the remote peer.
	IdentityKey() *secp256k1.PublicKey

	// Address returns the network address of the remote peer.
	Address() net.Addr

	// Inbound returns whether the remote peer connected to one of our
	// listening endpoints or if we initiated the connection.
	Inbound() bool

	// QuitSignal is a method that should return a channel which will be
	// sent upon or closed once the backing peer exits. This allows callers
	// using the interface to cancel any processing in the event the backing
	// implementation exits.
	QuitSignal() <-chan struct{}

	// LocalGlobalFeatures returns the set of global features that has been
	// advertised by the local peer. This allows sub-systems that use this
	// interface to gate their behavior off the set of negotiated feature
	// bits.
	LocalGlobalFeatures() *lnwire.FeatureVector

	// RemoteGlobalFeatures returns the set of global features that has
	// been advertised by the remote peer. This allows sub-systems that use
	// this interface to gate their behavior off the set of negotiated
	// feature bits.
	RemoteGlobalFeatures() *lnwire.FeatureVector
}
