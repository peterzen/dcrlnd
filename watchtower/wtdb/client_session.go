package wtdb

import (
	"errors"

	"github.com/decred/dcrd/dcrec/secp256k1"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/watchtower/wtpolicy"
)

var (
	// ErrClientSessionNotFound signals that the requested client session
	// was not found in the database.
	ErrClientSessionNotFound = errors.New("client session not found")

	// ErrUpdateAlreadyCommitted signals that the chosen sequence number has
	// already been committed to an update with a different breach hint.
	ErrUpdateAlreadyCommitted = errors.New("update already committed")

	// ErrCommitUnorderedUpdate signals the client tried to commit a
	// sequence number other than the next unallocated sequence number.
	ErrCommitUnorderedUpdate = errors.New("update seqnum not monotonic")

	// ErrCommittedUpdateNotFound signals that the tower tried to ACK a
	// sequence number that has not yet been allocated by the client.
	ErrCommittedUpdateNotFound = errors.New("committed update not found")

	// ErrUnallocatedLastApplied signals that the tower tried to provide a
	// LastApplied value greater than any allocated sequence number.
	ErrUnallocatedLastApplied = errors.New("tower echoed last appiled " +
		"greater than allocated seqnum")

	// ErrNoReservedKeyIndex signals that a client session could not be
	// created because no session key index was reserved.
	ErrNoReservedKeyIndex = errors.New("key index not reserved")

	// ErrIncorrectKeyIndex signals that the client session could not be
	// created because session key index differs from the reserved key
	// index.
	ErrIncorrectKeyIndex = errors.New("incorrect key index")
)

// ClientSession encapsulates a SessionInfo returned from a successful
// session negotiation, and also records the tower and ephemeral secret used for
// communicating with the tower.
type ClientSession struct {
	// ID is the client's public key used when authenticating with the
	// tower.
	ID SessionID

	// SeqNum is the next unallocated sequence number that can be sent to
	// the tower.
	SeqNum uint16

	// TowerLastApplied the last last-applied the tower has echoed back.
	TowerLastApplied uint16

	// TowerID is the unique, db-assigned identifier that references the
	// Tower with which the session is negotiated.
	TowerID TowerID

	// Tower holds the pubkey and address of the watchtower.
	//
	// NOTE: This value is not serialized. It is recovered by looking up the
	// tower with TowerID.
	Tower *Tower

	// KeyIndex is the index of key locator used to derive the client's
	// session key so that it can authenticate with the tower to update its
	// session. In order to rederive the private key, the key locator should
	// use the keychain.KeyFamilyTowerSession key family.
	KeyIndex uint32

	// SessionPrivKey is the ephemeral secret key used to connect to the
	// watchtower.
	//
	// NOTE: This value is not serialized. It is derived using the KeyIndex
	// on startup to avoid storing private keys on disk.
	SessionPrivKey *secp256k1.PrivateKey

	// Policy holds the negotiated session parameters.
	Policy wtpolicy.Policy

	// RewardPkScript is the pkscript that the tower's reward will be
	// deposited to if a sweep transaction confirms and the sessions
	// specifies a reward output.
	RewardPkScript []byte

	// CommittedUpdates is a sorted list of unacked updates. These updates
	// can be resent after a restart if the updates failed to send or
	// receive an acknowledgment.
	CommittedUpdates []CommittedUpdate

	// AckedUpdates is a map from sequence number to backup id to record
	// which revoked states were uploaded via this session.
	AckedUpdates map[uint16]BackupID
}

// BackupID identifies a particular revoked, remote commitment by channel id and
// commitment height.
type BackupID struct {
	// ChanID is the channel id of the revoked commitment.
	ChanID lnwire.ChannelID

	// CommitHeight is the commitment height of the revoked commitment.
	CommitHeight uint64
}

// CommittedUpdate holds a state update sent by a client along with its
// allocated sequence number and the exact remote commitment the encrypted
// justice transaction can rectify.
type CommittedUpdate struct {
	// SeqNum is the unique sequence number allocated by the session to this
	// update.
	SeqNum uint16

	CommittedUpdateBody
}

// CommittedUpdateBody represents the primary components of a CommittedUpdate.
// On disk, this is stored under the sequence number, which acts as its key.
type CommittedUpdateBody struct {
	// BackupID identifies the breached commitment that the encrypted blob
	// can spend from.
	BackupID BackupID

	// Hint is the 16-byte prefix of the revoked commitment transaction ID.
	Hint BreachHint

	// EncryptedBlob is a ciphertext containing the sweep information for
	// exacting justice if the commitment transaction matching the breach
	// hint is broadcast.
	EncryptedBlob []byte
}
