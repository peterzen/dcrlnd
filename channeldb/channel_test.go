package channeldb

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"reflect"
	"runtime"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrlnd/keychain"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/decred/dcrd/dcrutil/v2"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/shachain"
)

var (
	key = [chainhash.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	rev = [chainhash.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}
	testTx = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				SignatureScript: []byte{0x04, 0x31, 0xdc, 0x00, 0x1b, 0x01, 0x62},
				Sequence:        0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 5000000000,
				PkScript: []byte{
					0x41, // OP_DATA_65
					0x04, 0xd6, 0x4b, 0xdf, 0xd0, 0x9e, 0xb1, 0xc5,
					0xfe, 0x29, 0x5a, 0xbd, 0xeb, 0x1d, 0xca, 0x42,
					0x81, 0xbe, 0x98, 0x8e, 0x2d, 0xa0, 0xb6, 0xc1,
					0xc6, 0xa5, 0x9d, 0xc2, 0x26, 0xc2, 0x86, 0x24,
					0xe1, 0x81, 0x75, 0xe8, 0x51, 0xc9, 0x6b, 0x97,
					0x3d, 0x81, 0xb0, 0x1c, 0xc3, 0x1f, 0x04, 0x78,
					0x34, 0xbc, 0x06, 0xd6, 0xd6, 0xed, 0xf6, 0x20,
					0xd1, 0x84, 0x24, 0x1a, 0x6a, 0xed, 0x8b, 0x63,
					0xa6, // 65-byte signature
					0xac, // OP_CHECKSIG
				},
			},
		},
		LockTime: 5,
	}

	privKey, pubKey = secp256k1.PrivKeyFromBytes(key[:])

	wireSig, _ = lnwire.NewSigFromSignature(testSig)
)

// makeTestDB creates a new instance of the ChannelDB for testing purposes. A
// callback which cleans up the created temporary directories is also returned
// and intended to be executed after the test completes.
func makeTestDB() (*DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}

func createTestChannelState(cdb *DB) (*OpenChannel, error) {
	// Simulate 1000 channel updates.
	producer, err := shachain.NewRevocationProducerFromBytes(key[:])
	if err != nil {
		return nil, err
	}
	store := shachain.NewRevocationStore()
	for i := 0; i < 1; i++ {
		preImage, err := producer.AtIndex(uint64(i))
		if err != nil {
			return nil, err
		}

		if err := store.AddNextEntry(preImage); err != nil {
			return nil, err
		}
	}

	localCfg := ChannelConfig{
		ChannelConstraints: ChannelConstraints{
			DustLimit:        dcrutil.Amount(rand.Int63()),
			MaxPendingAmount: lnwire.MilliAtom(rand.Int63()),
			ChanReserve:      dcrutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliAtom(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
			CsvDelay:         uint16(rand.Int31()),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
		},
	}
	remoteCfg := ChannelConfig{
		ChannelConstraints: ChannelConstraints{
			DustLimit:        dcrutil.Amount(rand.Int63()),
			MaxPendingAmount: lnwire.MilliAtom(rand.Int63()),
			ChanReserve:      dcrutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliAtom(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
			CsvDelay:         uint16(rand.Int31()),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyMultiSig,
				Index:  9,
			},
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyRevocationBase,
				Index:  8,
			},
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyPaymentBase,
				Index:  7,
			},
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyDelayBase,
				Index:  6,
			},
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: (*secp256k1.PublicKey)(&privKey.PublicKey),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyHtlcBase,
				Index:  5,
			},
		},
	}

	chanID := lnwire.NewShortChanIDFromInt(uint64(rand.Int63()))

	return &OpenChannel{
		ChanType:            SingleFunder,
		ChainHash:           key,
		FundingOutpoint:     wire.OutPoint{Hash: key, Index: rand.Uint32()},
		ShortChannelID:      chanID,
		IsInitiator:         true,
		IsPending:           true,
		IdentityPub:         pubKey,
		Capacity:            dcrutil.Amount(10000),
		LocalChanCfg:        localCfg,
		RemoteChanCfg:       remoteCfg,
		TotalMAtomsSent:     8,
		TotalMAtomsReceived: 2,
		LocalCommitment: ChannelCommitment{
			CommitHeight:  0,
			LocalBalance:  lnwire.MilliAtom(9000),
			RemoteBalance: lnwire.MilliAtom(3000),
			CommitFee:     dcrutil.Amount(rand.Int63()),
			FeePerKB:      dcrutil.Amount(5000),
			CommitTx:      testTx,
			CommitSig:     bytes.Repeat([]byte{1}, 71),
		},
		RemoteCommitment: ChannelCommitment{
			CommitHeight:  0,
			LocalBalance:  lnwire.MilliAtom(3000),
			RemoteBalance: lnwire.MilliAtom(9000),
			CommitFee:     dcrutil.Amount(rand.Int63()),
			FeePerKB:      dcrutil.Amount(5000),
			CommitTx:      testTx,
			CommitSig:     bytes.Repeat([]byte{1}, 71),
		},
		NumConfsRequired:        4,
		RemoteCurrentRevocation: (*secp256k1.PublicKey)(&privKey.PublicKey),
		RemoteNextRevocation:    (*secp256k1.PublicKey)(&privKey.PublicKey),
		RevocationProducer:      producer,
		RevocationStore:         store,
		Db:                      cdb,
		Packager:                NewChannelPackager(chanID),
		FundingTxn:              testTx,
	}, nil
}

func TestOpenChannelPutGetDelete(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	// Create the test channel state, then add an additional fake HTLC
	// before syncing to disk.
	state, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}
	state.LocalCommitment.Htlcs = []HTLC{
		{
			Signature:     testSig.Serialize(),
			Incoming:      true,
			Amt:           10,
			RHash:         key,
			RefundTimeout: 1,
			OnionBlob:     []byte("onionblob"),
		},
	}
	state.RemoteCommitment.Htlcs = []HTLC{
		{
			Signature:     testSig.Serialize(),
			Incoming:      false,
			Amt:           10,
			RHash:         key,
			RefundTimeout: 1,
			OnionBlob:     []byte("onionblob"),
		},
	}

	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}
	if err := state.SyncPending(addr, 101); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}

	openChannels, err := cdb.FetchOpenChannels(state.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch open channel: %v", err)
	}

	newState := openChannels[0]

	// The decoded channel state should be identical to what we stored
	// above.
	if !reflect.DeepEqual(state, newState) {
		t.Fatalf("channel state doesn't match:: %v vs %v",
			spew.Sdump(state), spew.Sdump(newState))
	}

	// We'll also test that the channel is properly able to hot swap the
	// next revocation for the state machine. This tests the initial
	// post-funding revocation exchange.
	nextRevKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("unable to create new private key: %v", err)
	}
	nextRevKeyPub := (*secp256k1.PublicKey)(&nextRevKey.PublicKey)
	if err := state.InsertNextRevocation(nextRevKeyPub); err != nil {
		t.Fatalf("unable to update revocation: %v", err)
	}

	openChannels, err = cdb.FetchOpenChannels(state.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch open channel: %v", err)
	}
	updatedChan := openChannels[0]

	// Ensure that the revocation was set properly.
	if !nextRevKeyPub.IsEqual(updatedChan.RemoteNextRevocation) {
		t.Fatalf("next revocation wasn't updated")
	}

	// Finally to wrap up the test, delete the state of the channel within
	// the database. This involves "closing" the channel which removes all
	// written state, and creates a small "summary" elsewhere within the
	// database.
	closeSummary := &ChannelCloseSummary{
		ChanPoint:         state.FundingOutpoint,
		RemotePub:         state.IdentityPub,
		SettledBalance:    dcrutil.Amount(500),
		TimeLockedBalance: dcrutil.Amount(10000),
		IsPending:         false,
		CloseType:         CooperativeClose,
	}
	if err := state.CloseChannel(closeSummary); err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// As the channel is now closed, attempting to fetch all open channels
	// for our fake node ID should return an empty slice.
	openChans, err := cdb.FetchOpenChannels(state.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch open channels: %v", err)
	}
	if len(openChans) != 0 {
		t.Fatalf("all channels not deleted, found %v", len(openChans))
	}

	// Additionally, attempting to fetch all the open channels globally
	// should yield no results.
	openChans, err = cdb.FetchAllChannels()
	if err != nil {
		t.Fatal("unable to fetch all open chans")
	}
	if len(openChans) != 0 {
		t.Fatalf("all channels not deleted, found %v", len(openChans))
	}
}

func assertCommitmentEqual(t *testing.T, a, b *ChannelCommitment) {
	if !reflect.DeepEqual(a, b) {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: commitments don't match: %v vs %v",
			line, spew.Sdump(a), spew.Sdump(b))
	}
}

func TestChannelStateTransition(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	// First create a minimal channel, then perform a full sync in order to
	// persist the data.
	channel, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}

	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}
	if err := channel.SyncPending(addr, 101); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}

	// Add some HTLCs which were added during this new state transition.
	// Half of the HTLCs are incoming, while the other half are outgoing.
	var (
		htlcs   []HTLC
		htlcAmt lnwire.MilliAtom
	)
	for i := uint32(0); i < 10; i++ {
		var incoming bool
		if i > 5 {
			incoming = true
		}
		htlc := HTLC{
			Signature:     testSig.Serialize(),
			Incoming:      incoming,
			Amt:           10,
			RHash:         key,
			RefundTimeout: i,
			OutputIndex:   int32(i * 3),
			LogIndex:      uint64(i * 2),
			HtlcIndex:     uint64(i),
		}
		htlc.OnionBlob = make([]byte, 10)
		copy(htlc.OnionBlob, bytes.Repeat([]byte{2}, 10))
		htlcs = append(htlcs, htlc)
		htlcAmt += htlc.Amt
	}

	// Create a new channel delta which includes the above HTLCs, some
	// balance updates, and an increment of the current commitment height.
	// Additionally, modify the signature and commitment transaction.
	newSequence := uint32(129498)
	newSig := bytes.Repeat([]byte{3}, 71)
	newTx := channel.LocalCommitment.CommitTx.Copy()
	newTx.TxIn[0].Sequence = newSequence
	commitment := ChannelCommitment{
		CommitHeight:    1,
		LocalLogIndex:   2,
		LocalHtlcIndex:  1,
		RemoteLogIndex:  2,
		RemoteHtlcIndex: 1,
		LocalBalance:    lnwire.MilliAtom(1e8),
		RemoteBalance:   lnwire.MilliAtom(1e8),
		CommitFee:       55,
		FeePerKB:        99,
		CommitTx:        newTx,
		CommitSig:       newSig,
		Htlcs:           htlcs,
	}

	// First update the local node's broadcastable state and also add a
	// CommitDiff remote node's as well in order to simulate a proper state
	// transition.
	if err := channel.UpdateCommitment(&commitment); err != nil {
		t.Fatalf("unable to update commitment: %v", err)
	}

	// The balances, new update, the HTLCs and the changes to the fake
	// commitment transaction along with the modified signature should all
	// have been updated.
	updatedChannel, err := cdb.FetchOpenChannels(channel.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch updated channel: %v", err)
	}
	assertCommitmentEqual(t, &commitment, &updatedChannel[0].LocalCommitment)
	numDiskUpdates, err := updatedChannel[0].CommitmentHeight()
	if err != nil {
		t.Fatalf("unable to read commitment height from disk: %v", err)
	}
	if numDiskUpdates != commitment.CommitHeight {
		t.Fatalf("num disk updates doesn't match: %v vs %v",
			numDiskUpdates, commitment.CommitHeight)
	}

	// Attempting to query for a commitment diff should return
	// ErrNoPendingCommit as we haven't yet created a new state for them.
	_, err = channel.RemoteCommitChainTip()
	if err != ErrNoPendingCommit {
		t.Fatalf("expected ErrNoPendingCommit, instead got %v", err)
	}

	// To simulate us extending a new state to the remote party, we'll also
	// create a new commit diff for them.
	remoteCommit := commitment
	remoteCommit.LocalBalance = lnwire.MilliAtom(2e8)
	remoteCommit.RemoteBalance = lnwire.MilliAtom(3e8)
	remoteCommit.CommitHeight = 1
	commitDiff := &CommitDiff{
		Commitment: remoteCommit,
		CommitSig: &lnwire.CommitSig{
			ChanID:    lnwire.ChannelID(key),
			CommitSig: wireSig,
			HtlcSigs: []lnwire.Sig{
				wireSig,
				wireSig,
			},
		},
		LogUpdates: []LogUpdate{
			{
				LogIndex: 1,
				UpdateMsg: &lnwire.UpdateAddHTLC{
					ID:     1,
					Amount: lnwire.NewMAtomsFromAtoms(100),
					Expiry: 25,
				},
			},
			{
				LogIndex: 2,
				UpdateMsg: &lnwire.UpdateAddHTLC{
					ID:     2,
					Amount: lnwire.NewMAtomsFromAtoms(200),
					Expiry: 50,
				},
			},
		},
		OpenedCircuitKeys: []CircuitKey{},
		ClosedCircuitKeys: []CircuitKey{},
	}
	copy(commitDiff.LogUpdates[0].UpdateMsg.(*lnwire.UpdateAddHTLC).PaymentHash[:],
		bytes.Repeat([]byte{1}, 32))
	copy(commitDiff.LogUpdates[1].UpdateMsg.(*lnwire.UpdateAddHTLC).PaymentHash[:],
		bytes.Repeat([]byte{2}, 32))
	if err := channel.AppendRemoteCommitChain(commitDiff); err != nil {
		t.Fatalf("unable to add to commit chain: %v", err)
	}

	// The commitment tip should now match the commitment that we just
	// inserted.
	diskCommitDiff, err := channel.RemoteCommitChainTip()
	if err != nil {
		t.Fatalf("unable to fetch commit diff: %v", err)
	}
	if !reflect.DeepEqual(commitDiff, diskCommitDiff) {
		t.Fatalf("commit diffs don't match: %v vs %v", spew.Sdump(remoteCommit),
			spew.Sdump(diskCommitDiff))
	}

	// We'll save the old remote commitment as this will be added to the
	// revocation log shortly.
	oldRemoteCommit := channel.RemoteCommitment

	// Next, write to the log which tracks the necessary revocation state
	// needed to rectify any fishy behavior by the remote party. Modify the
	// current uncollapsed revocation state to simulate a state transition
	// by the remote party.
	channel.RemoteCurrentRevocation = channel.RemoteNextRevocation
	newPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	channel.RemoteNextRevocation = (*secp256k1.PublicKey)(&newPriv.PublicKey)

	fwdPkg := NewFwdPkg(channel.ShortChanID(), oldRemoteCommit.CommitHeight,
		diskCommitDiff.LogUpdates, nil)

	err = channel.AdvanceCommitChainTail(fwdPkg)
	if err != nil {
		t.Fatalf("unable to append to revocation log: %v", err)
	}

	// At this point, the remote commit chain should be nil, and the posted
	// remote commitment should match the one we added as a diff above.
	if _, err := channel.RemoteCommitChainTip(); err != ErrNoPendingCommit {
		t.Fatalf("expected ErrNoPendingCommit, instead got %v", err)
	}

	// We should be able to fetch the channel delta created above by its
	// update number with all the state properly reconstructed.
	diskPrevCommit, err := channel.FindPreviousState(
		oldRemoteCommit.CommitHeight,
	)
	if err != nil {
		t.Fatalf("unable to fetch past delta: %v", err)
	}

	// The two deltas (the original vs the on-disk version) should
	// identical, and all HTLC data should properly be retained.
	assertCommitmentEqual(t, &oldRemoteCommit, diskPrevCommit)

	// The state number recovered from the tail of the revocation log
	// should be identical to this current state.
	logTail, err := channel.RevocationLogTail()
	if err != nil {
		t.Fatalf("unable to retrieve log: %v", err)
	}
	if logTail.CommitHeight != oldRemoteCommit.CommitHeight {
		t.Fatal("update number doesn't match")
	}

	oldRemoteCommit = channel.RemoteCommitment

	// Next modify the posted diff commitment slightly, then create a new
	// commitment diff and advance the tail.
	commitDiff.Commitment.CommitHeight = 2
	commitDiff.Commitment.LocalBalance -= htlcAmt
	commitDiff.Commitment.RemoteBalance += htlcAmt
	commitDiff.LogUpdates = []LogUpdate{}
	if err := channel.AppendRemoteCommitChain(commitDiff); err != nil {
		t.Fatalf("unable to add to commit chain: %v", err)
	}

	fwdPkg = NewFwdPkg(channel.ShortChanID(), oldRemoteCommit.CommitHeight, nil, nil)

	err = channel.AdvanceCommitChainTail(fwdPkg)
	if err != nil {
		t.Fatalf("unable to append to revocation log: %v", err)
	}

	// Once again, fetch the state and ensure it has been properly updated.
	prevCommit, err := channel.FindPreviousState(oldRemoteCommit.CommitHeight)
	if err != nil {
		t.Fatalf("unable to fetch past delta: %v", err)
	}
	assertCommitmentEqual(t, &oldRemoteCommit, prevCommit)

	// Once again, state number recovered from the tail of the revocation
	// log should be identical to this current state.
	logTail, err = channel.RevocationLogTail()
	if err != nil {
		t.Fatalf("unable to retrieve log: %v", err)
	}
	if logTail.CommitHeight != oldRemoteCommit.CommitHeight {
		t.Fatal("update number doesn't match")
	}

	// The revocation state stored on-disk should now also be identical.
	updatedChannel, err = cdb.FetchOpenChannels(channel.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch updated channel: %v", err)
	}
	if !channel.RemoteCurrentRevocation.IsEqual(updatedChannel[0].RemoteCurrentRevocation) {
		t.Fatalf("revocation state was not synced")
	}
	if !channel.RemoteNextRevocation.IsEqual(updatedChannel[0].RemoteNextRevocation) {
		t.Fatalf("revocation state was not synced")
	}

	// Now attempt to delete the channel from the database.
	closeSummary := &ChannelCloseSummary{
		ChanPoint:         channel.FundingOutpoint,
		RemotePub:         channel.IdentityPub,
		SettledBalance:    dcrutil.Amount(500),
		TimeLockedBalance: dcrutil.Amount(10000),
		IsPending:         false,
		CloseType:         RemoteForceClose,
	}
	if err := updatedChannel[0].CloseChannel(closeSummary); err != nil {
		t.Fatalf("unable to delete updated channel: %v", err)
	}

	// If we attempt to fetch the target channel again, it shouldn't be
	// found.
	channels, err := cdb.FetchOpenChannels(channel.IdentityPub)
	if err != nil {
		t.Fatalf("unable to fetch updated channels: %v", err)
	}
	if len(channels) != 0 {
		t.Fatalf("%v channels, found, but none should be",
			len(channels))
	}

	// Attempting to find previous states on the channel should fail as the
	// revocation log has been deleted.
	_, err = updatedChannel[0].FindPreviousState(oldRemoteCommit.CommitHeight)
	if err == nil {
		t.Fatal("revocation log search should have failed")
	}
}

func TestFetchPendingChannels(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	// Create first test channel state
	state, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}

	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}

	const broadcastHeight = 99
	if err := state.SyncPending(addr, broadcastHeight); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}

	pendingChannels, err := cdb.FetchPendingChannels()
	if err != nil {
		t.Fatalf("unable to list pending channels: %v", err)
	}

	if len(pendingChannels) != 1 {
		t.Fatalf("incorrect number of pending channels: expecting %v,"+
			"got %v", 1, len(pendingChannels))
	}

	// The broadcast height of the pending channel should have been set
	// properly.
	if pendingChannels[0].FundingBroadcastHeight != broadcastHeight {
		t.Fatalf("broadcast height mismatch: expected %v, got %v",
			pendingChannels[0].FundingBroadcastHeight,
			broadcastHeight)
	}

	chanOpenLoc := lnwire.ShortChannelID{
		BlockHeight: 5,
		TxIndex:     10,
		TxPosition:  15,
	}
	err = pendingChannels[0].MarkAsOpen(chanOpenLoc)
	if err != nil {
		t.Fatalf("unable to mark channel as open: %v", err)
	}

	if pendingChannels[0].IsPending {
		t.Fatalf("channel marked open should no longer be pending")
	}

	if pendingChannels[0].ShortChanID() != chanOpenLoc {
		t.Fatalf("channel opening height not updated: expected %v, "+
			"got %v", spew.Sdump(pendingChannels[0].ShortChanID()),
			chanOpenLoc)
	}

	// Next, we'll re-fetch the channel to ensure that the open height was
	// properly set.
	openChans, err := cdb.FetchAllChannels()
	if err != nil {
		t.Fatalf("unable to fetch channels: %v", err)
	}
	if openChans[0].ShortChanID() != chanOpenLoc {
		t.Fatalf("channel opening heights don't match: expected %v, "+
			"got %v", spew.Sdump(openChans[0].ShortChanID()),
			chanOpenLoc)
	}
	if openChans[0].FundingBroadcastHeight != broadcastHeight {
		t.Fatalf("broadcast height mismatch: expected %v, got %v",
			openChans[0].FundingBroadcastHeight,
			broadcastHeight)
	}

	pendingChannels, err = cdb.FetchPendingChannels()
	if err != nil {
		t.Fatalf("unable to list pending channels: %v", err)
	}

	if len(pendingChannels) != 0 {
		t.Fatalf("incorrect number of pending channels: expecting %v,"+
			"got %v", 0, len(pendingChannels))
	}
}

func TestFetchClosedChannels(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	// First create a test channel, that we'll be closing within this pull
	// request.
	state, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}

	// Next sync the channel to disk, marking it as being in a pending open
	// state.
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}
	const broadcastHeight = 99
	if err := state.SyncPending(addr, broadcastHeight); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}

	// Next, simulate the confirmation of the channel by marking it as
	// pending within the database.
	chanOpenLoc := lnwire.ShortChannelID{
		BlockHeight: 5,
		TxIndex:     10,
		TxPosition:  15,
	}
	err = state.MarkAsOpen(chanOpenLoc)
	if err != nil {
		t.Fatalf("unable to mark channel as open: %v", err)
	}

	// Next, close the channel by including a close channel summary in the
	// database.
	summary := &ChannelCloseSummary{
		ChanPoint:         state.FundingOutpoint,
		ClosingTXID:       rev,
		RemotePub:         state.IdentityPub,
		Capacity:          state.Capacity,
		SettledBalance:    state.LocalCommitment.LocalBalance.ToAtoms(),
		TimeLockedBalance: state.RemoteCommitment.LocalBalance.ToAtoms() + 10000,
		CloseType:         RemoteForceClose,
		IsPending:         true,
		LocalChanConfig:   state.LocalChanCfg,
	}
	if err := state.CloseChannel(summary); err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Query the database to ensure that the channel has now been properly
	// closed. We should get the same result whether querying for pending
	// channels only, or not.
	pendingClosed, err := cdb.FetchClosedChannels(true)
	if err != nil {
		t.Fatalf("failed fetching closed channels: %v", err)
	}
	if len(pendingClosed) != 1 {
		t.Fatalf("incorrect number of pending closed channels: expecting %v,"+
			"got %v", 1, len(pendingClosed))
	}
	if !reflect.DeepEqual(summary, pendingClosed[0]) {
		t.Fatalf("database summaries don't match: expected %v got %v",
			spew.Sdump(summary), spew.Sdump(pendingClosed[0]))
	}
	closed, err := cdb.FetchClosedChannels(false)
	if err != nil {
		t.Fatalf("failed fetching all closed channels: %v", err)
	}
	if len(closed) != 1 {
		t.Fatalf("incorrect number of closed channels: expecting %v, "+
			"got %v", 1, len(closed))
	}
	if !reflect.DeepEqual(summary, closed[0]) {
		t.Fatalf("database summaries don't match: expected %v got %v",
			spew.Sdump(summary), spew.Sdump(closed[0]))
	}

	// Mark the channel as fully closed.
	err = cdb.MarkChanFullyClosed(&state.FundingOutpoint)
	if err != nil {
		t.Fatalf("failed fully closing channel: %v", err)
	}

	// The channel should no longer be considered pending, but should still
	// be retrieved when fetching all the closed channels.
	closed, err = cdb.FetchClosedChannels(false)
	if err != nil {
		t.Fatalf("failed fetching closed channels: %v", err)
	}
	if len(closed) != 1 {
		t.Fatalf("incorrect number of closed channels: expecting %v, "+
			"got %v", 1, len(closed))
	}
	pendingClose, err := cdb.FetchClosedChannels(true)
	if err != nil {
		t.Fatalf("failed fetching channels pending close: %v", err)
	}
	if len(pendingClose) != 0 {
		t.Fatalf("incorrect number of closed channels: expecting %v, "+
			"got %v", 0, len(closed))
	}
}

// TestFetchWaitingCloseChannels ensures that the correct channels that are
// waiting to be closed are returned.
func TestFetchWaitingCloseChannels(t *testing.T) {
	t.Parallel()

	const numChannels = 2
	const broadcastHeight = 99
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18555}

	// We'll start by creating two channels within our test database. One of
	// them will have their funding transaction confirmed on-chain, while
	// the other one will remain unconfirmed.
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	channels := make([]*OpenChannel, numChannels)
	for i := 0; i < numChannels; i++ {
		channel, err := createTestChannelState(db)
		if err != nil {
			t.Fatalf("unable to create channel: %v", err)
		}
		err = channel.SyncPending(addr, broadcastHeight)
		if err != nil {
			t.Fatalf("unable to sync channel: %v", err)
		}
		channels[i] = channel
	}

	// We'll only confirm the first one.
	channelConf := lnwire.ShortChannelID{
		BlockHeight: broadcastHeight + 1,
		TxIndex:     10,
		TxPosition:  15,
	}
	if err := channels[0].MarkAsOpen(channelConf); err != nil {
		t.Fatalf("unable to mark channel as open: %v", err)
	}

	// Then, we'll mark the channels as if their commitments were broadcast.
	// This would happen in the event of a force close and should make the
	// channels enter a state of waiting close.
	for _, channel := range channels {
		closeTx := wire.NewMsgTx()
		closeTx.Version = 2
		closeTx.AddTxIn(
			&wire.TxIn{
				PreviousOutPoint: channel.FundingOutpoint,
			},
		)
		if err := channel.MarkCommitmentBroadcasted(closeTx); err != nil {
			t.Fatalf("unable to mark commitment broadcast: %v", err)
		}
	}

	// Now, we'll fetch all the channels waiting to be closed from the
	// database. We should expect to see both channels above, even if any of
	// them haven't had their funding transaction confirm on-chain.
	waitingCloseChannels, err := db.FetchWaitingCloseChannels()
	if err != nil {
		t.Fatalf("unable to fetch all waiting close channels: %v", err)
	}
	if len(waitingCloseChannels) != 2 {
		t.Fatalf("expected %d channels waiting to be closed, got %d", 2,
			len(waitingCloseChannels))
	}
	expectedChannels := make(map[wire.OutPoint]struct{})
	for _, channel := range channels {
		expectedChannels[channel.FundingOutpoint] = struct{}{}
	}
	for _, channel := range waitingCloseChannels {
		if _, ok := expectedChannels[channel.FundingOutpoint]; !ok {
			t.Fatalf("expected channel %v to be waiting close",
				channel.FundingOutpoint)
		}

		// Finally, make sure we can retrieve the closing tx for the
		// channel.
		closeTx, err := channel.BroadcastedCommitment()
		if err != nil {
			t.Fatalf("Unable to retrieve commitment: %v", err)
		}

		if closeTx.TxIn[0].PreviousOutPoint != channel.FundingOutpoint {
			t.Fatalf("expected outpoint %v, got %v",
				channel.FundingOutpoint,
				closeTx.TxIn[0].PreviousOutPoint)
		}
	}
}

// TestRefreshShortChanID asserts that RefreshShortChanID updates the in-memory
// short channel ID of another OpenChannel to reflect a preceding call to
// MarkOpen on a different OpenChannel.
func TestRefreshShortChanID(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	// First create a test channel.
	state, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}

	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}

	// Mark the channel as pending within the channeldb.
	const broadcastHeight = 99
	if err := state.SyncPending(addr, broadcastHeight); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}

	// Next, locate the pending channel with the database.
	pendingChannels, err := cdb.FetchPendingChannels()
	if err != nil {
		t.Fatalf("unable to load pending channels; %v", err)
	}

	var pendingChannel *OpenChannel
	for _, channel := range pendingChannels {
		if channel.FundingOutpoint == state.FundingOutpoint {
			pendingChannel = channel
			break
		}
	}
	if pendingChannel == nil {
		t.Fatalf("unable to find pending channel with funding "+
			"outpoint=%v: %v", state.FundingOutpoint, err)
	}

	// Next, simulate the confirmation of the channel by marking it as
	// pending within the database.
	chanOpenLoc := lnwire.ShortChannelID{
		BlockHeight: 105,
		TxIndex:     10,
		TxPosition:  15,
	}

	err = state.MarkAsOpen(chanOpenLoc)
	if err != nil {
		t.Fatalf("unable to mark channel open: %v", err)
	}

	// The short_chan_id of the receiver to MarkAsOpen should reflect the
	// open location, but the other pending channel should remain unchanged.
	if state.ShortChanID() == pendingChannel.ShortChanID() {
		t.Fatalf("pending channel short_chan_ID should not have been " +
			"updated before refreshing short_chan_id")
	}

	// Now that the receiver's short channel id has been updated, check to
	// ensure that the channel packager's source has been updated as well.
	// This ensures that the packager will read and write to buckets
	// corresponding to the new short chan id, instead of the prior.
	if state.Packager.(*ChannelPackager).source != chanOpenLoc {
		t.Fatalf("channel packager source was not updated: want %v, "+
			"got %v", chanOpenLoc,
			state.Packager.(*ChannelPackager).source)
	}

	// Now, refresh the short channel ID of the pending channel.
	err = pendingChannel.RefreshShortChanID()
	if err != nil {
		t.Fatalf("unable to refresh short_chan_id: %v", err)
	}

	// This should result in both OpenChannel's now having the same
	// ShortChanID.
	if state.ShortChanID() != pendingChannel.ShortChanID() {
		t.Fatalf("expected pending channel short_chan_id to be "+
			"refreshed: want %v, got %v", state.ShortChanID(),
			pendingChannel.ShortChanID())
	}

	// Check to ensure that the _other_ OpenChannel channel packager's
	// source has also been updated after the refresh. This ensures that the
	// other packagers will read and write to buckets corresponding to the
	// updated short chan id.
	if pendingChannel.Packager.(*ChannelPackager).source != chanOpenLoc {
		t.Fatalf("channel packager source was not updated: want %v, "+
			"got %v", chanOpenLoc,
			pendingChannel.Packager.(*ChannelPackager).source)
	}
}
