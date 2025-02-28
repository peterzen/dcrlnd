package lnwallet

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"fmt"

	"reflect"
	"runtime"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/decred/dcrd/dcrutil/v2"
	"github.com/decred/dcrd/txscript/v2"
	"github.com/decred/dcrd/wire"

	"github.com/decred/dcrlnd/chainntnfs"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/input"
	"github.com/decred/dcrlnd/lnwire"
)

var (
	// TODO(matheusd) Are these all?
	scriptFlagsForTest = txscript.ScriptDiscourageUpgradableNops |
		txscript.ScriptVerifyCleanStack |
		txscript.ScriptVerifyCheckLockTimeVerify |
		txscript.ScriptVerifyCheckSequenceVerify |
		txscript.ScriptVerifySHA256
)

// createHTLC is a utility function for generating an HTLC with a given
// preimage and a given amount.
func createHTLC(id int, amount lnwire.MilliAtom) (*lnwire.UpdateAddHTLC, [32]byte) {
	preimage := bytes.Repeat([]byte{byte(id)}, 32)
	paymentHash := sha256.Sum256(preimage)

	var returnPreimage [32]byte
	copy(returnPreimage[:], preimage)

	return &lnwire.UpdateAddHTLC{
		ID:          uint64(id),
		PaymentHash: paymentHash,
		Amount:      amount,
		Expiry:      uint32(5),
	}, returnPreimage
}

func assertOutputExistsByValue(t *testing.T, commitTx *wire.MsgTx,
	value dcrutil.Amount) {

	for _, txOut := range commitTx.TxOut {
		if txOut.Value == int64(value) {
			return
		}
	}

	t.Fatalf("unable to find output of value %v within tx %v", value,
		spew.Sdump(commitTx))
}

// testAddSettleWorkflow tests a simple channel scenario where Alice and Bob
// add, the settle an HTLC between themselves.
func testAddSettleWorkflow(t *testing.T, tweakless bool) {
	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(tweakless)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	paymentPreimage := bytes.Repeat([]byte{1}, 32)
	paymentHash := sha256.Sum256(paymentPreimage)
	htlcAmt := lnwire.NewMAtomsFromAtoms(dcrutil.AtomsPerCoin)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      htlcAmt,
		Expiry:      uint32(5),
	}

	// First Alice adds the outgoing HTLC to her local channel's state
	// update log. Then Alice sends this wire message over to Bob who adds
	// this htlc to his remote state update log.
	aliceHtlcIndex, err := aliceChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}

	bobHtlcIndex, err := bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Next alice commits this change by sending a signature message. Since
	// we expect the messages to be ordered, Bob will receive the HTLC we
	// just sent before he receives this signature, so the signature will
	// cover the HTLC.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// Bob receives this signature message, and checks that this covers the
	// state he has in his remote log. This includes the HTLC just sent
	// from Alice.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	// Bob revokes his prior commitment given to him by Alice, since he now
	// has a valid signature for a newer commitment.
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}

	// Bob finally send a signature for Alice's commitment transaction.
	// This signature will cover the HTLC, since Bob will first send the
	// revocation just created. The revocation also acks every received
	// HTLC up to the point where Alice sent here signature.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign alice's commitment: %v", err)
	}

	// Alice then processes this revocation, sending her own revocation for
	// her prior commitment transaction. Alice shouldn't have any HTLCs to
	// forward since she's sending an outgoing HTLC.
	fwdPkg, _, _, _, err := aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	}
	if len(fwdPkg.Adds) != 0 {
		t.Fatalf("alice forwards %v add htlcs, should forward none",
			len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("alice forwards %v settle/fail htlcs, "+
			"should forward none", len(fwdPkg.SettleFails))
	}

	// Alice then processes bob's signature, and since she just received
	// the revocation, she expect this signature to cover everything up to
	// the point where she sent her signature, including the HTLC.
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	// Alice then generates a revocation for bob.
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke alice channel: %v", err)
	}

	// Finally Bob processes Alice's revocation, at this point the new HTLC
	// is fully locked in within both commitment transactions. Bob should
	// also be able to forward an HTLC now that the HTLC has been locked
	// into both commitment transactions.
	fwdPkg, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}
	if len(fwdPkg.Adds) != 1 {
		t.Fatalf("bob forwards %v add htlcs, should only forward one",
			len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("bob forwards %v settle/fail htlcs, "+
			"should forward none", len(fwdPkg.SettleFails))
	}

	// At this point, both sides should have the proper number of atoms
	// sent, and commitment height updated within their local channel
	// state.
	aliceSent := lnwire.MilliAtom(0)
	bobSent := lnwire.MilliAtom(0)

	if aliceChannel.channelState.TotalMAtomsSent != aliceSent {
		t.Fatalf("alice has incorrect milli-atoms sent: %v vs %v",
			aliceChannel.channelState.TotalMAtomsSent, aliceSent)
	}
	if aliceChannel.channelState.TotalMAtomsReceived != bobSent {
		t.Fatalf("alice has incorrect milli-atoms received %v vs %v",
			aliceChannel.channelState.TotalMAtomsReceived, bobSent)
	}
	if bobChannel.channelState.TotalMAtomsSent != bobSent {
		t.Fatalf("bob has incorrect milli-atoms sent %v vs %v",
			bobChannel.channelState.TotalMAtomsSent, bobSent)
	}
	if bobChannel.channelState.TotalMAtomsReceived != aliceSent {
		t.Fatalf("bob has incorrect milli-atoms received %v vs %v",
			bobChannel.channelState.TotalMAtomsReceived, aliceSent)
	}
	if bobChannel.currentHeight != 1 {
		t.Fatalf("bob has incorrect commitment height, %v vs %v",
			bobChannel.currentHeight, 1)
	}
	if aliceChannel.currentHeight != 1 {
		t.Fatalf("alice has incorrect commitment height, %v vs %v",
			aliceChannel.currentHeight, 1)
	}

	// Both commitment transactions should have three outputs, and one of
	// them should be exactly the amount of the HTLC.
	if len(aliceChannel.channelState.LocalCommitment.CommitTx.TxOut) != 3 {
		t.Fatalf("alice should have three commitment outputs, instead "+
			"have %v",
			len(aliceChannel.channelState.LocalCommitment.CommitTx.TxOut))
	}
	if len(bobChannel.channelState.LocalCommitment.CommitTx.TxOut) != 3 {
		t.Fatalf("bob should have three commitment outputs, instead "+
			"have %v",
			len(bobChannel.channelState.LocalCommitment.CommitTx.TxOut))
	}
	assertOutputExistsByValue(t,
		aliceChannel.channelState.LocalCommitment.CommitTx,
		htlcAmt.ToAtoms())
	assertOutputExistsByValue(t,
		bobChannel.channelState.LocalCommitment.CommitTx,
		htlcAmt.ToAtoms())

	// Now we'll repeat a similar exchange, this time with Bob settling the
	// HTLC once he learns of the preimage.
	var preimage [32]byte
	copy(preimage[:], paymentPreimage)
	err = bobChannel.SettleHTLC(preimage, bobHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}

	err = aliceChannel.ReceiveHTLCSettle(preimage, aliceHtlcIndex)
	if err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}

	bobSig2, bobHtlcSigs2, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign settle commitment: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSig2, bobHtlcSigs2)
	if err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	aliceRevocation2, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to generate revocation: %v", err)
	}
	aliceSig2, aliceHtlcSigs2, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign new commitment: %v", err)
	}

	fwdPkg, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation2)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}
	if len(fwdPkg.Adds) != 0 {
		t.Fatalf("bob forwards %v add htlcs, should forward none",
			len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("bob forwards %v settle/fail htlcs, "+
			"should forward none", len(fwdPkg.SettleFails))
	}

	err = bobChannel.ReceiveNewCommitment(aliceSig2, aliceHtlcSigs2)
	if err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	bobRevocation2, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("bob unable to revoke commitment: %v", err)
	}
	fwdPkg, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation2)
	if err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	}
	if len(fwdPkg.Adds) != 0 {
		// Alice should now be able to forward the settlement HTLC to
		// any down stream peers.
		t.Fatalf("alice should be forwarding an add HTLC, "+
			"instead forwarding %v: %v", len(fwdPkg.Adds),
			spew.Sdump(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 1 {
		t.Fatalf("alice should be forwarding one settle/fails HTLC, "+
			"instead forwarding: %v", len(fwdPkg.SettleFails))
	}

	// At this point, Bob should have 6 DCR settled, with Alice still
	// having 4 DCR. Alice's channel should show 1 DCR sent and Bob's
	// channel should show 1 DCR received. They should also be at
	// commitment height two, with the revocation window extended by 1 (5).
	mAtTransferred := lnwire.NewMAtomsFromAtoms(dcrutil.AtomsPerCoin)
	if aliceChannel.channelState.TotalMAtomsSent != mAtTransferred {
		t.Fatalf("alice atoms sent incorrect %v vs %v expected",
			aliceChannel.channelState.TotalMAtomsSent,
			mAtTransferred)
	}
	if aliceChannel.channelState.TotalMAtomsReceived != 0 {
		t.Fatalf("alice atoms received incorrect %v vs %v expected",
			aliceChannel.channelState.TotalMAtomsReceived, 0)
	}
	if bobChannel.channelState.TotalMAtomsReceived != mAtTransferred {
		t.Fatalf("bob atoms received incorrect %v vs %v expected",
			bobChannel.channelState.TotalMAtomsReceived,
			mAtTransferred)
	}
	if bobChannel.channelState.TotalMAtomsSent != 0 {
		t.Fatalf("bob atoms sent incorrect %v vs %v expected",
			bobChannel.channelState.TotalMAtomsSent, 0)
	}
	if bobChannel.currentHeight != 2 {
		t.Fatalf("bob has incorrect commitment height, %v vs %v",
			bobChannel.currentHeight, 2)
	}
	if aliceChannel.currentHeight != 2 {
		t.Fatalf("alice has incorrect commitment height, %v vs %v",
			aliceChannel.currentHeight, 2)
	}

	// The logs of both sides should now be cleared since the entry adding
	// the HTLC should have been removed once both sides receive the
	// revocation.
	if aliceChannel.localUpdateLog.Len() != 0 {
		t.Fatalf("alice's local not updated, should be empty, has %v "+
			"entries instead", aliceChannel.localUpdateLog.Len())
	}
	if aliceChannel.remoteUpdateLog.Len() != 0 {
		t.Fatalf("alice's remote not updated, should be empty, has %v "+
			"entries instead", aliceChannel.remoteUpdateLog.Len())
	}
	if len(aliceChannel.localUpdateLog.updateIndex) != 0 {
		t.Fatalf("alice's local log index not cleared, should be empty but "+
			"has %v entries", len(aliceChannel.localUpdateLog.updateIndex))
	}
	if len(aliceChannel.remoteUpdateLog.updateIndex) != 0 {
		t.Fatalf("alice's remote log index not cleared, should be empty but "+
			"has %v entries", len(aliceChannel.remoteUpdateLog.updateIndex))
	}
}

// TestSimpleAddSettleWorkflow tests a simple channel scenario wherein the
// local node (Alice in this case) creates a new outgoing HTLC to bob, commits
// this change, then bob immediately commits a settlement of the HTLC after the
// initial add is fully committed in both commit chains.
//
// TODO(roasbeef): write higher level framework to exercise various states of
// the state machine
//  * DSL language perhaps?
//  * constructed via input/output files
func TestSimpleAddSettleWorkflow(t *testing.T) {
	t.Parallel()

	for _, tweakless := range []bool{true, false} {
		tweakless := tweakless
		t.Run(fmt.Sprintf("tweakless=%v", tweakless), func(t *testing.T) {
			testAddSettleWorkflow(t, tweakless)
		})
	}
}

// TestCheckCommitTxSize checks that estimation size of commitment
// transaction with some degree of error corresponds to the actual size.
func TestCheckCommitTxSize(t *testing.T) {
	t.Parallel()

	checkSize := func(channel *LightningChannel, count int) {
		// Due to the possible number of HTLC outputs being over 0xff, the
		// varint that stores the number of outputs might be one byte off
		// (estimation for worst case commitment tx uses 2 bytes).
		commitTxSizeEstimationError := int64(1)

		// Due to variable size of the signatures (70-73) in witness
		// script actual size of commitment transaction might be lower
		// by 6 bytes (recall that 2 signatures are required when
		// redeeming the funding transaction).
		commitTxSizeEstimationError += int64(6)

		commitTx, err := channel.getSignedCommitTx()
		if err != nil {
			t.Fatalf("unable to initiate alice force close: %v", err)
		}
		actualCost := int64(commitTx.SerializeSize())
		estimatedCost := input.EstimateCommitmentTxSize(count)

		diff := estimatedCost - actualCost
		if 0 > diff || commitTxSizeEstimationError < diff {
			t.Fatalf("estimation is wrong with %d HTLCs, diff: %v",
				count, diff)
		}

	}

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(false)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Check that size estimation of the commitment transaction without
	// HTLCs is right.
	checkSize(aliceChannel, 0)
	checkSize(bobChannel, 0)

	// Adding HTLCs and check that size stays in allowable estimation
	// error window.
	for i := 0; i <= 10; i++ {
		htlc, _ := createHTLC(i, lnwire.MilliAtom(1e9))

		if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
			t.Fatalf("alice unable to add htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("bob unable to receive htlc: %v", err)
		}

		if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
			t.Fatalf("unable to complete state update: %v", err)
		}
		checkSize(aliceChannel, i+1)
		checkSize(bobChannel, i+1)
	}

	// Settle HTLCs and check that estimation is counting cost of settle
	// HTLCs properly.
	for i := 10; i >= 0; i-- {
		_, preimage := createHTLC(i, lnwire.MilliAtom(1e9))

		err := bobChannel.SettleHTLC(preimage, uint64(i), nil, nil, nil)
		if err != nil {
			t.Fatalf("bob unable to settle inbound htlc: %v", err)
		}

		err = aliceChannel.ReceiveHTLCSettle(preimage, uint64(i))
		if err != nil {
			t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
		}

		if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
			t.Fatalf("unable to complete state update: %v", err)
		}
		checkSize(aliceChannel, i)
		checkSize(bobChannel, i)
	}
}

func TestCooperativeChannelClosure(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We can use any dummy script, given that thist test doesn't actually
	// verify whether the signature corresponds to the correct script.
	aliceDeliveryScript := alicePkScript
	bobDeliveryScript := bobPkScript

	aliceFeeRate := AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB)
	bobFeeRate := AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB)

	// We'll store with both Alice and Bob creating a new close proposal
	// with the same fee.
	aliceFee := aliceChannel.CalcFee(aliceFeeRate)
	aliceSig, _, _, err := aliceChannel.CreateCloseProposal(
		aliceFee, aliceDeliveryScript, bobDeliveryScript,
	)
	if err != nil {
		t.Fatalf("unable to create alice coop close proposal: %v", err)
	}
	aliceCloseSig := append(aliceSig, byte(txscript.SigHashAll))

	bobFee := bobChannel.CalcFee(bobFeeRate)
	bobSig, _, _, err := bobChannel.CreateCloseProposal(
		bobFee, bobDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("unable to create bob coop close proposal: %v", err)
	}
	bobCloseSig := append(bobSig, byte(txscript.SigHashAll))

	// With the proposals created, both sides should be able to properly
	// process the other party's signature. This indicates that the
	// transaction is well formed, and the signatures verify.
	aliceCloseTx, _, err := bobChannel.CompleteCooperativeClose(
		bobCloseSig, aliceCloseSig, bobDeliveryScript,
		aliceDeliveryScript, bobFee,
	)
	if err != nil {
		t.Fatalf("unable to complete alice cooperative close: %v", err)
	}
	bobCloseSha := aliceCloseTx.TxHash()

	bobCloseTx, _, err := aliceChannel.CompleteCooperativeClose(
		aliceCloseSig, bobCloseSig, aliceDeliveryScript,
		bobDeliveryScript, aliceFee,
	)
	if err != nil {
		t.Fatalf("unable to complete bob cooperative close: %v", err)
	}
	aliceCloseSha := bobCloseTx.TxHash()

	if bobCloseSha != aliceCloseSha {
		t.Fatalf("alice and bob close transactions don't match: %v", err)
	}
}

// TestForceClose checks that the resulting ForceCloseSummary is correct when a
// peer is ForceClosing the channel. Will check outputs both above and below
// the dust limit. Additionally, we'll ensure that the node which executed the
// force close generates HTLC resolutions that are capable of sweeping both
// incoming and outgoing HTLC's.
func TestForceClose(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	bobAmount := bobChannel.channelState.LocalCommitment.LocalBalance

	// First, we'll add an outgoing HTLC from Alice to Bob, such that it
	// will still be present within the broadcast commitment transaction.
	// We'll ensure that the HTLC amount is above Alice's dust limit.
	htlcAmount := lnwire.NewMAtomsFromAtoms(200000)
	htlcAlice, _ := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlcAlice); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}

	// We'll also a distinct HTLC from Bob -> Alice. This way, Alice will
	// have both an incoming and outgoing HTLC on her commitment
	// transaction.
	htlcBob, preimageBob := createHTLC(0, htlcAmount)
	if _, err := bobChannel.AddHTLC(htlcBob, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := aliceChannel.ReceiveHTLC(htlcBob); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}

	// Next, we'll perform two state transitions to ensure that both HTLC's
	// get fully locked-in.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	// With the cache populated, we'll now attempt the force close
	// initiated by Alice.
	closeSummary, err := aliceChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}

	// Alice should detect that she can sweep the outgoing HTLC after a
	// timeout, but also that she's able to sweep in incoming HTLC Bob sent
	// her.
	if len(closeSummary.HtlcResolutions.OutgoingHTLCs) != 1 {
		t.Fatalf("alice out htlc resolutions not populated: expected %v "+
			"htlcs, got %v htlcs",
			1, len(closeSummary.HtlcResolutions.OutgoingHTLCs))
	}
	if len(closeSummary.HtlcResolutions.IncomingHTLCs) != 1 {
		t.Fatalf("alice in htlc resolutions not populated: expected %v "+
			"htlcs, got %v htlcs",
			1, len(closeSummary.HtlcResolutions.IncomingHTLCs))
	}

	// The SelfOutputSignDesc should be non-nil since the output to-self is
	// non-dust.
	aliceCommitResolution := closeSummary.CommitResolution
	if aliceCommitResolution == nil {
		t.Fatalf("alice fails to include to-self output in " +
			"ForceCloseSummary")
	}

	// The rest of the close summary should have been populated properly.
	aliceDelayPoint := aliceChannel.channelState.LocalChanCfg.DelayBasePoint
	if !aliceCommitResolution.SelfOutputSignDesc.KeyDesc.PubKey.IsEqual(
		aliceDelayPoint.PubKey,
	) {
		t.Fatalf("alice incorrect pubkey in SelfOutputSignDesc")
	}

	// Factoring in the fee rate, Alice's amount should properly reflect
	// that we've added two additional HTLC to the commitment transaction.
	totalCommitSize := input.CommitmentTxSize + (input.HTLCOutputSize * 2)
	feePerKB := AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB)
	commitFee := feePerKB.FeeForSize(totalCommitSize)
	expectedAmount := (aliceChannel.Capacity / 2) - htlcAmount.ToAtoms() - commitFee
	if aliceCommitResolution.SelfOutputSignDesc.Output.Value != int64(expectedAmount) {
		t.Fatalf("alice incorrect output value in SelfOutputSignDesc, "+
			"expected %v, got %v", int64(expectedAmount),
			aliceCommitResolution.SelfOutputSignDesc.Output.Value)
	}

	// Alice's listed CSV delay should also match the delay that was
	// pre-committed to at channel opening.
	if aliceCommitResolution.MaturityDelay !=
		uint32(aliceChannel.localChanCfg.CsvDelay) {

		t.Fatalf("alice: incorrect local CSV delay in ForceCloseSummary, "+
			"expected %v, got %v",
			aliceChannel.channelState.LocalChanCfg.CsvDelay,
			aliceCommitResolution.MaturityDelay)
	}

	// Next, we'll ensure that the second level HTLC transaction it itself
	// spendable, and also that the delivery output (with delay) itself has
	// a valid sign descriptor.
	htlcResolution := closeSummary.HtlcResolutions.OutgoingHTLCs[0]
	outHtlcIndex := htlcResolution.SignedTimeoutTx.TxIn[0].PreviousOutPoint.Index
	senderHtlcPkScript := closeSummary.CloseTx.TxOut[outHtlcIndex].PkScript
	senderHtlcScriptVersion := closeSummary.CloseTx.TxOut[outHtlcIndex].Version

	// First, verify that the second level transaction can properly spend
	// the multi-sig clause within the output on the commitment transaction
	// that produces this HTLC.
	timeoutTx := htlcResolution.SignedTimeoutTx
	vm, err := txscript.NewEngine(senderHtlcPkScript, timeoutTx, 0,
		scriptFlagsForTest, senderHtlcScriptVersion, nil)
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("htlc timeout spend is invalid: %v", err)
	}

	// Next, we'll ensure that we can spend the output of the second level
	// transaction given a properly crafted sweep transaction.
	sweepTx := wire.NewMsgTx()
	sweepTx.Version = input.LNTxVersion
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  htlcResolution.SignedTimeoutTx.TxHash(),
			Index: 0,
		},
		ValueIn: timeoutTx.TxOut[0].Value,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: senderHtlcPkScript,
		Value:    htlcResolution.SweepSignDesc.Output.Value,
	})
	htlcResolution.SweepSignDesc.InputIndex = 0
	witness, err := input.HtlcSpendSuccess(aliceChannel.Signer,
		&htlcResolution.SweepSignDesc, sweepTx,
		uint32(aliceChannel.channelState.LocalChanCfg.CsvDelay))
	if err != nil {
		t.Fatalf("unable to gen witness for timeout output: %v", err)
	}
	sweepTx.TxIn[0].SignatureScript, err = input.WitnessStackToSigScript(witness)
	if err != nil {
		t.Fatalf("unable to convert witness stack to sigScript: %v", err)
	}

	// With the witness fully populated for the success spend from the
	// second-level transaction, we ensure that the scripts properly
	// validate given the information within the htlc resolution struct.
	vm, err = txscript.NewEngine(
		htlcResolution.SweepSignDesc.Output.PkScript,
		sweepTx, 0, scriptFlagsForTest,
		htlcResolution.SweepSignDesc.Output.Version, nil,
	)
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("htlc timeout spend is invalid: %v", err)
	}

	// Finally, the txid of the commitment transaction and the one returned
	// as the closing transaction should also match.
	closeTxHash := closeSummary.CloseTx.TxHash()
	commitTxHash := aliceChannel.channelState.LocalCommitment.CommitTx.TxHash()
	if !bytes.Equal(closeTxHash[:], commitTxHash[:]) {
		t.Fatalf("alice: incorrect close transaction txid")
	}

	// We'll now perform similar set of checks to ensure that Alice is able
	// to sweep the output that Bob sent to her on-chain with knowledge of
	// the preimage.
	inHtlcResolution := closeSummary.HtlcResolutions.IncomingHTLCs[0]
	inHtlcIndex := inHtlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint.Index
	receiverHtlcScript := closeSummary.CloseTx.TxOut[inHtlcIndex].PkScript
	receiverHtlcScriptVersion := closeSummary.CloseTx.TxOut[inHtlcIndex].Version

	// With the original pkscript located, we'll now verify that the second
	// level transaction can spend from the multi-sig out. Supply the
	// preimage manually. This is usually done by the contract resolver
	// before publication.
	successTx := inHtlcResolution.SignedSuccessTx
	successTx.TxIn[0].SignatureScript, err = input.ReplaceReceiverHtlcSpendRedeemPreimage(
		successTx.TxIn[0].SignatureScript, preimageBob[:],
	)
	if err != nil {
		t.Fatalf("unable to replace preimage in sigScript: %v", err)
	}
	vm, err = txscript.NewEngine(receiverHtlcScript,
		successTx, 0, scriptFlagsForTest, receiverHtlcScriptVersion, nil)
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("htlc success spend is invalid: %v", err)
	}

	// Finally, we'll construct a transaction to spend the produced
	// second-level output with the attached input.SignDescriptor.
	sweepTx = wire.NewMsgTx()
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: inHtlcResolution.ClaimOutpoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: receiverHtlcScript,
		Value:    inHtlcResolution.SweepSignDesc.Output.Value,
	})
	inHtlcResolution.SweepSignDesc.InputIndex = 0
	witness, err = input.HtlcSpendSuccess(aliceChannel.Signer,
		&inHtlcResolution.SweepSignDesc, sweepTx,
		uint32(aliceChannel.channelState.LocalChanCfg.CsvDelay))
	if err != nil {
		t.Fatalf("unable to gen witness for timeout output: %v", err)
	}
	sweepTx.TxIn[0].SignatureScript, err = input.WitnessStackToSigScript(witness)
	if err != nil {
		t.Fatalf("unable to convert witness stack to sigScript: %v", err)
	}

	// The spend we create above spending the second level HTLC output
	// should validate without any issues.
	vm, err = txscript.NewEngine(
		inHtlcResolution.SweepSignDesc.Output.PkScript,
		sweepTx, 0, scriptFlagsForTest,
		inHtlcResolution.SweepSignDesc.Output.Version, nil,
	)
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("htlc timeout spend is invalid: %v", err)
	}

	// Check the same for Bob's ForceCloseSummary.
	closeSummary, err = bobChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}
	bobCommitResolution := closeSummary.CommitResolution
	if bobCommitResolution == nil {
		t.Fatalf("bob fails to include to-self output in ForceCloseSummary")
	}
	bobDelayPoint := bobChannel.channelState.LocalChanCfg.DelayBasePoint
	if !bobCommitResolution.SelfOutputSignDesc.KeyDesc.PubKey.IsEqual(bobDelayPoint.PubKey) {
		t.Fatalf("bob incorrect pubkey in SelfOutputSignDesc")
	}
	if bobCommitResolution.SelfOutputSignDesc.Output.Value !=
		int64(bobAmount.ToAtoms()-htlcAmount.ToAtoms()) {

		t.Fatalf("bob incorrect output value in SelfOutputSignDesc, "+
			"expected %v, got %v",
			bobAmount.ToAtoms(),
			bobCommitResolution.SelfOutputSignDesc.Output.Value)
	}
	if bobCommitResolution.MaturityDelay !=
		uint32(bobChannel.channelState.LocalChanCfg.CsvDelay) {

		t.Fatalf("bob: incorrect local CSV delay in ForceCloseSummary, "+
			"expected %v, got %v",
			bobChannel.channelState.LocalChanCfg.CsvDelay,
			bobCommitResolution.MaturityDelay)
	}

	closeTxHash = closeSummary.CloseTx.TxHash()
	commitTxHash = bobChannel.channelState.LocalCommitment.CommitTx.TxHash()
	if !bytes.Equal(closeTxHash[:], commitTxHash[:]) {
		t.Fatalf("bob: incorrect close transaction txid")
	}

	// As we didn't add the preimage of Alice's HTLC to bob's preimage
	// cache, he should only detect that he can sweep only his outgoing
	// HTLC upon force close.
	if len(closeSummary.HtlcResolutions.OutgoingHTLCs) != 1 {
		t.Fatalf("alice out htlc resolutions not populated: expected %v "+
			"htlcs, got %v htlcs",
			1, len(closeSummary.HtlcResolutions.OutgoingHTLCs))
	}

	// Bob should recognize that the incoming HTLC is there, but the
	// preimage should be empty as he doesn't have the knowledge required
	// to sweep it.
	if len(closeSummary.HtlcResolutions.IncomingHTLCs) != 1 {
		t.Fatalf("bob in htlc resolutions not populated: expected %v "+
			"htlcs, got %v htlcs",
			1, len(closeSummary.HtlcResolutions.IncomingHTLCs))
	}
}

// TestForceCloseDustOutput tests that if either side force closes with an
// active dust output (for only a single party due to asymmetric dust values),
// then the force close summary is well crafted.
func TestForceCloseDustOutput(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We set both node's channel reserves to 0, to make sure
	// they can create small dust ouputs without going under
	// their channel reserves.
	aliceChannel.localChanCfg.ChanReserve = 0
	bobChannel.localChanCfg.ChanReserve = 0
	aliceChannel.remoteChanCfg.ChanReserve = 0
	bobChannel.remoteChanCfg.ChanReserve = 0

	htlcAmount := lnwire.NewMAtomsFromAtoms(500)

	bobAmount := bobChannel.channelState.LocalCommitment.LocalBalance

	// Have Bobs' to-self output be below her dust limit and check
	// ForceCloseSummary again on both peers.
	htlc, preimage := createHTLC(0, bobAmount-htlcAmount)
	bobHtlcIndex, err := bobChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	aliceHtlcIndex, err := aliceChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("bob unable to receive htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	// Settle HTLC and sign new commitment.
	err = aliceChannel.SettleHTLC(preimage, aliceHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}
	err = bobChannel.ReceiveHTLCSettle(preimage, bobHtlcIndex)
	if err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	aliceAmount := aliceChannel.channelState.LocalCommitment.LocalBalance

	closeSummary, err := aliceChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}

	// Alice's to-self output should still be in the commitment
	// transaction.
	commitResolution := closeSummary.CommitResolution
	if commitResolution == nil {
		t.Fatalf("alice fails to include to-self output in " +
			"ForceCloseSummary")
	}
	if !commitResolution.SelfOutputSignDesc.KeyDesc.PubKey.IsEqual(
		aliceChannel.channelState.LocalChanCfg.DelayBasePoint.PubKey,
	) {
		t.Fatalf("alice incorrect pubkey in SelfOutputSignDesc")
	}
	if commitResolution.SelfOutputSignDesc.Output.Value !=
		int64(aliceAmount.ToAtoms()) {
		t.Fatalf("alice incorrect output value in SelfOutputSignDesc, "+
			"expected %v, got %v",
			aliceChannel.channelState.LocalCommitment.LocalBalance.ToAtoms(),
			commitResolution.SelfOutputSignDesc.Output.Value)
	}

	if commitResolution.MaturityDelay !=
		uint32(aliceChannel.channelState.LocalChanCfg.CsvDelay) {
		t.Fatalf("alice: incorrect local CSV delay in ForceCloseSummary, "+
			"expected %v, got %v",
			aliceChannel.channelState.LocalChanCfg.CsvDelay,
			commitResolution.MaturityDelay)
	}

	closeTxHash := closeSummary.CloseTx.TxHash()
	commitTxHash := aliceChannel.channelState.LocalCommitment.CommitTx.TxHash()
	if !bytes.Equal(closeTxHash[:], commitTxHash[:]) {
		t.Fatalf("alice: incorrect close transaction txid")
	}

	closeSummary, err = bobChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}

	// Bob's to-self output is below Bob's dust value and should be
	// reflected in the ForceCloseSummary.
	commitResolution = closeSummary.CommitResolution
	if commitResolution != nil {
		t.Fatalf("bob incorrectly includes to-self output in " +
			"ForceCloseSummary")
	}

	closeTxHash = closeSummary.CloseTx.TxHash()
	commitTxHash = bobChannel.channelState.LocalCommitment.CommitTx.TxHash()
	if !bytes.Equal(closeTxHash[:], commitTxHash[:]) {
		t.Fatalf("bob: incorrect close transaction txid")
	}
}

// TestDustHTLCFees checks that fees are calculated correctly when HTLCs fall
// below the nodes' dust limit. In these cases, the amount of the dust HTLCs
// should be applied to the commitment transaction fee.
func TestDustHTLCFees(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	aliceStartingBalance := aliceChannel.channelState.LocalCommitment.LocalBalance

	// This HTLC amount should be lower than the dust limits of both nodes.
	htlcAmount := lnwire.NewMAtomsFromAtoms(100)
	htlc, _ := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("bob unable to receive htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	// After the transition, we'll ensure that we performed fee accounting
	// properly. Namely, the local+remote+commitfee values should add up to
	// the total capacity of the channel. This same should hold for both
	// sides.
	totalAtomsAlice := (aliceChannel.channelState.LocalCommitment.LocalBalance +
		aliceChannel.channelState.LocalCommitment.RemoteBalance +
		lnwire.NewMAtomsFromAtoms(aliceChannel.channelState.LocalCommitment.CommitFee))
	if totalAtomsAlice+htlcAmount != lnwire.NewMAtomsFromAtoms(aliceChannel.Capacity) {
		t.Fatalf("alice's funds leaked: total atoms are %v, but channel "+
			"capacity is %v", int64(totalAtomsAlice),
			int64(aliceChannel.Capacity))
	}
	totalAtomsBob := (bobChannel.channelState.LocalCommitment.LocalBalance +
		bobChannel.channelState.LocalCommitment.RemoteBalance +
		lnwire.NewMAtomsFromAtoms(bobChannel.channelState.LocalCommitment.CommitFee))
	if totalAtomsBob+htlcAmount != lnwire.NewMAtomsFromAtoms(bobChannel.Capacity) {
		t.Fatalf("bob's funds leaked: total atoms are %v, but channel "+
			"capacity is %v", int64(totalAtomsBob),
			int64(bobChannel.Capacity))
	}

	// The commitment fee paid should be the same, as there have been no
	// new material outputs added.
	defaultFee := calcStaticFee(0)
	if aliceChannel.channelState.LocalCommitment.CommitFee != defaultFee {
		t.Fatalf("dust htlc amounts not subtracted from commitment fee "+
			"expected %v, got %v", defaultFee,
			aliceChannel.channelState.LocalCommitment.CommitFee)
	}
	if bobChannel.channelState.LocalCommitment.CommitFee != defaultFee {
		t.Fatalf("dust htlc amounts not subtracted from commitment fee "+
			"expected %v, got %v", defaultFee,
			bobChannel.channelState.LocalCommitment.CommitFee)
	}

	// Alice's final balance should reflect the HTLC deficit even though
	// the HTLC was paid to fees as it was trimmed.
	aliceEndBalance := aliceChannel.channelState.LocalCommitment.LocalBalance
	aliceExpectedBalance := aliceStartingBalance - htlcAmount
	if aliceEndBalance != aliceExpectedBalance {
		t.Fatalf("alice not credited for dust: expected %v, got %v",
			aliceExpectedBalance, aliceEndBalance)
	}
}

// TestHTLCDustLimit checks the situation in which an HTLC is larger than one
// channel participant's dust limit, but smaller than the other participant's
// dust limit. In this case, the participants' commitment chains will diverge.
// In one commitment chain, the HTLC will be added as normal, in the other
// chain, the amount of the HTLC will contribute to the fees to be paid.
func TestHTLCDustLimit(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// The amount of the HTLC should be above Alice's dust limit and below
	// Bob's dust limit.
	htlcAtoms := (dcrutil.Amount(8000) + htlcTimeoutFee(
		AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB)))
	htlcAmount := lnwire.NewMAtomsFromAtoms(htlcAtoms)

	htlc, preimage := createHTLC(0, htlcAmount)
	aliceHtlcIndex, err := aliceChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	bobHtlcIndex, err := bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("bob unable to receive htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	// At this point, Alice's commitment transaction should have an HTLC,
	// while Bob's should not, because the value falls beneath his dust
	// limit. The amount of the HTLC should be applied to fees in Bob's
	// commitment transaction.
	aliceCommitment := aliceChannel.localCommitChain.tip()
	if len(aliceCommitment.txn.TxOut) != 3 {
		t.Fatalf("incorrect # of outputs: expected %v, got %v",
			3, len(aliceCommitment.txn.TxOut))
	}
	bobCommitment := bobChannel.localCommitChain.tip()
	if len(bobCommitment.txn.TxOut) != 2 {
		t.Fatalf("incorrect # of outputs: expected %v, got %v",
			2, len(bobCommitment.txn.TxOut))
	}
	defaultFee := calcStaticFee(0)
	if bobChannel.channelState.LocalCommitment.CommitFee != defaultFee {
		t.Fatalf("dust htlc amount was subtracted from commitment fee "+
			"expected %v, got %v", defaultFee,
			bobChannel.channelState.LocalCommitment.CommitFee)
	}

	// Settle HTLC and create a new commitment state.
	err = bobChannel.SettleHTLC(preimage, bobHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}
	err = aliceChannel.ReceiveHTLCSettle(preimage, aliceHtlcIndex)
	if err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("state transition error: %v", err)
	}

	// At this point, for Alice's commitment chains, the value of the HTLC
	// should have been added to Alice's balance and TotalMatomsSent.
	commitment := aliceChannel.localCommitChain.tip()
	if len(commitment.txn.TxOut) != 2 {
		t.Fatalf("incorrect # of outputs: expected %v, got %v",
			2, len(commitment.txn.TxOut))
	}
	if aliceChannel.channelState.TotalMAtomsSent != htlcAmount {
		t.Fatalf("alice atoms sent incorrect: expected %v, got %v",
			htlcAmount, aliceChannel.channelState.TotalMAtomsSent)
	}
}

// TestHTLCSigNumber tests that a received commitment is only accepted if it
// comes with the exact number of valid HTLC signatures.
func TestHTLCSigNumber(t *testing.T) {
	t.Parallel()

	// createChanWithHTLC is a helper method that sets ut two channels, and
	// adds HTLCs with the passed values to the channels.
	createChanWithHTLC := func(htlcValues ...dcrutil.Amount) (
		*LightningChannel, *LightningChannel, func()) {

		// Create a test channel funded evenly with Alice having 5 DCR,
		// and Bob having 5 DCR. Alice's dustlimit is 200 Atoms, while
		// Bob has 1300 Atoms.
		aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
		if err != nil {
			t.Fatalf("unable to create test channels: %v", err)
		}

		for i, htlcAtoms := range htlcValues {
			htlcMAtoms := lnwire.NewMAtomsFromAtoms(htlcAtoms)
			htlc, _ := createHTLC(i, htlcMAtoms)
			_, err := aliceChannel.AddHTLC(htlc, nil)
			if err != nil {
				t.Fatalf("alice unable to add htlc: %v", err)
			}
			_, err = bobChannel.ReceiveHTLC(htlc)
			if err != nil {
				t.Fatalf("bob unable to receive htlc: %v", err)
			}
		}

		return aliceChannel, bobChannel, cleanUp
	}

	// Calculate two values that will be below and above Bob's dust limit.
	estimator := NewStaticFeeEstimator(1e5, 0)
	FeePerKB, err := estimator.EstimateFeePerKB(1)
	if err != nil {
		t.Fatalf("unable to get fee: %v", err)
	}

	belowDust := dcrutil.Amount(5000) + htlcTimeoutFee(FeePerKB)
	aboveDust := dcrutil.Amount(14000) + htlcSuccessFee(FeePerKB)

	// ===================================================================
	// Test that Bob will reject a commitment if Alice doesn't send enough
	// HTLC signatures.
	// ===================================================================
	aliceChannel, bobChannel, cleanUp := createChanWithHTLC(aboveDust,
		aboveDust)
	defer cleanUp()

	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("Error signing next commitment: %v", err)
	}

	if len(aliceHtlcSigs) != 2 {
		t.Fatalf("expected 2 htlc sig, instead got %v",
			len(aliceHtlcSigs))
	}

	// Now discard one signature from the htlcSig slice. Bob should reject
	// the commitment because of this.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs[1:])
	if err == nil {
		t.Fatalf("Expected Bob to reject signatures")
	}

	// ===================================================================
	// Test that Bob will reject a commitment if Alice doesn't send any
	// HTLC signatures.
	// ===================================================================
	aliceChannel, bobChannel, cleanUp = createChanWithHTLC(aboveDust)
	defer cleanUp()

	aliceSig, aliceHtlcSigs, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("Error signing next commitment: %v", err)
	}

	if len(aliceHtlcSigs) != 1 {
		t.Fatalf("expected 1 htlc sig, instead got %v",
			len(aliceHtlcSigs))
	}

	// Now just give Bob an empty htlcSig slice. He should reject the
	// commitment because of this.
	err = bobChannel.ReceiveNewCommitment(aliceSig, []lnwire.Sig{})
	if err == nil {
		t.Fatalf("Expected Bob to reject signatures")
	}

	// ==============================================================
	// Test that sigs are not returned for HTLCs below dust limit.
	// ==============================================================
	aliceChannel, bobChannel, cleanUp = createChanWithHTLC(belowDust)
	defer cleanUp()

	aliceSig, aliceHtlcSigs, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("Error signing next commitment: %v", err)
	}

	// Since the HTLC is below Bob's dust limit, Alice won't need to send
	// any signatures for this HTLC.
	if len(aliceHtlcSigs) != 0 {
		t.Fatalf("expected no htlc sigs, instead got %v",
			len(aliceHtlcSigs))
	}

	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("Bob failed receiving commitment: %v", err)
	}

	// ================================================================
	// Test that sigs are correctly returned for HTLCs above dust limit.
	// ================================================================
	aliceChannel, bobChannel, cleanUp = createChanWithHTLC(aboveDust)
	defer cleanUp()

	aliceSig, aliceHtlcSigs, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("Error signing next commitment: %v", err)
	}

	// Since the HTLC is above Bob's dust limit, Alice should send a
	// signature for this HTLC.
	if len(aliceHtlcSigs) != 1 {
		t.Fatalf("expected 1 htlc sig, instead got %v",
			len(aliceHtlcSigs))
	}

	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("Bob failed receiving commitment: %v", err)
	}

	// ====================================================================
	// Test that Bob will not validate a received commitment if Alice sends
	// signatures for HTLCs below the dust limit.
	// ====================================================================
	aliceChannel, bobChannel, cleanUp = createChanWithHTLC(belowDust,
		aboveDust)
	defer cleanUp()

	// Alice should produce only one signature, since one HTLC is below
	// dust.
	aliceSig, aliceHtlcSigs, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("Error signing next commitment: %v", err)
	}

	if len(aliceHtlcSigs) != 1 {
		t.Fatalf("expected 1 htlc sig, instead got %v",
			len(aliceHtlcSigs))
	}

	// Add an extra signature.
	aliceHtlcSigs = append(aliceHtlcSigs, aliceHtlcSigs[0])

	// Bob should reject these signatures since they don't match the number
	// of HTLCs above dust.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err == nil {
		t.Fatalf("Expected Bob to reject signatures")
	}
}

// TestChannelBalanceDustLimit tests the condition when the remaining balance
// for one of the channel participants is so small as to be considered dust. In
// this case, the output for that participant is removed and all funds (minus
// fees) in the commitment transaction are allocated to the remaining channel
// participant.
//
// TODO(roasbeef): test needs to be fixed after reserve limits are done
func TestChannelBalanceDustLimit(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	aliceDustLimit := aliceChannel.channelState.LocalChanCfg.DustLimit
	bobDustLimit := bobChannel.channelState.LocalChanCfg.DustLimit
	if aliceDustLimit >= bobDustLimit {
		t.Fatalf("failed test precondition: aliceDustLimit must be < bobDustLimit")
	}

	// To allow Alice's balance to get beneath her dust limit, set the
	// channel reserve to be 0.
	aliceChannel.localChanCfg.ChanReserve = 0
	bobChannel.remoteChanCfg.ChanReserve = 0

	// Figure out how much it will cost to add an HTLC to the channel.
	htlcFee := calcStaticFee(1) - calcStaticFee(0)

	// Select how much to send from alice to bob. This must be enough so that
	// the remaining balance in alice's commitment output is higher than her
	// dust limit but lower than bob's dust limit, causing bob to remove that
	// output from his commitment transaction.
	aliceBalance := aliceChannel.channelState.LocalCommitment.LocalBalance.ToAtoms()
	htlcAtoms := aliceBalance - htlcFee - aliceDustLimit - 1

	htlcAmount := lnwire.NewMAtomsFromAtoms(htlcAtoms)
	htlc, preimage := createHTLC(0, htlcAmount)
	aliceHtlcIndex, err := aliceChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	bobHtlcIndex, err := bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("bob unable to receive htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("state transition error: %v", err)
	}
	err = bobChannel.SettleHTLC(preimage, bobHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}
	err = aliceChannel.ReceiveHTLCSettle(preimage, aliceHtlcIndex)
	if err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("state transition error: %v", err)
	}

	// At the conclusion of this test, in Bob's commitment chains, the
	// output for Alice's balance should have been removed as dust, leaving
	// only a single output that will send the remaining funds in the
	// channel to Bob. Alice should still have 2 outputs, given that we
	// built the htlc to specifically leave some amount to her.
	bobCommitment := bobChannel.localCommitChain.tip()
	if len(bobCommitment.txn.TxOut) != 1 {
		t.Fatalf("incorrect # of outputs for bob: expected %v, got %v",
			1, len(bobCommitment.txn.TxOut))
	}
	if aliceChannel.channelState.TotalMAtomsSent != htlcAmount {
		t.Fatalf("alice atoms sent incorrect: expected %v, got %v",
			htlcAmount, aliceChannel.channelState.TotalMAtomsSent)
	}
	aliceCommitment := aliceChannel.localCommitChain.tip()
	if len(aliceCommitment.txn.TxOut) != 2 {
		t.Fatalf("incorrect # of outputs for alice: expected %v, got %v",
			2, len(aliceCommitment.txn.TxOut))
	}
}

func TestStateUpdatePersistence(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	htlcAmt := lnwire.NewMAtomsFromAtoms(5000)

	var fakeOnionBlob [lnwire.OnionPacketSize]byte
	copy(fakeOnionBlob[:], bytes.Repeat([]byte{0x05}, lnwire.OnionPacketSize))

	// Alice adds 3 HTLCs to the update log, while Bob adds a single HTLC.
	var alicePreimage [32]byte
	copy(alicePreimage[:], bytes.Repeat([]byte{0xaa}, 32))
	var bobPreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xbb}, 32))
	for i := 0; i < 3; i++ {
		rHash := sha256.Sum256(alicePreimage[:])
		h := &lnwire.UpdateAddHTLC{
			ID:          uint64(i),
			PaymentHash: rHash,
			Amount:      htlcAmt,
			Expiry:      uint32(10),
			OnionBlob:   fakeOnionBlob,
		}

		if _, err := aliceChannel.AddHTLC(h, nil); err != nil {
			t.Fatalf("unable to add alice's htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(h); err != nil {
			t.Fatalf("unable to recv alice's htlc: %v", err)
		}
	}
	rHash := sha256.Sum256(bobPreimage[:])
	bobh := &lnwire.UpdateAddHTLC{
		PaymentHash: rHash,
		Amount:      htlcAmt,
		Expiry:      uint32(10),
		OnionBlob:   fakeOnionBlob,
	}
	if _, err := bobChannel.AddHTLC(bobh, nil); err != nil {
		t.Fatalf("unable to add bob's htlc: %v", err)
	}
	if _, err := aliceChannel.ReceiveHTLC(bobh); err != nil {
		t.Fatalf("unable to recv bob's htlc: %v", err)
	}

	// Also add a fee update to the update logs.
	fee := AtomPerKByte(2e4)
	if err := aliceChannel.UpdateFee(fee); err != nil {
		t.Fatalf("unable to send fee update")
	}
	if err := bobChannel.ReceiveUpdateFee(fee); err != nil {
		t.Fatalf("unable to receive fee update")
	}

	// Helper method that asserts the expected number of updates are found
	// in the update logs.
	assertNumLogUpdates := func(numAliceUpdates, numBobUpdates int) {
		if aliceChannel.localUpdateLog.Len() != numAliceUpdates {
			t.Fatalf("expected %d local updates, found %d",
				numAliceUpdates,
				aliceChannel.localUpdateLog.Len())
		}
		if aliceChannel.remoteUpdateLog.Len() != numBobUpdates {
			t.Fatalf("expected %d remote updates, found %d",
				numBobUpdates,
				aliceChannel.remoteUpdateLog.Len())
		}

		if bobChannel.localUpdateLog.Len() != numBobUpdates {
			t.Fatalf("expected %d local updates, found %d",
				numBobUpdates,
				bobChannel.localUpdateLog.Len())
		}
		if bobChannel.remoteUpdateLog.Len() != numAliceUpdates {
			t.Fatalf("expected %d remote updates, found %d",
				numAliceUpdates,
				bobChannel.remoteUpdateLog.Len())
		}
	}

	// Both nodes should now have Alice's 3 Adds and 1 FeeUpdate in the
	// log, and Bob's 1 Add.
	assertNumLogUpdates(4, 1)

	// Next, Alice initiates a state transition to include the HTLC's she
	// added above in a new commitment state.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete alice's state transition: %v", err)
	}

	// Since the HTLC Bob sent wasn't included in Bob's version of the
	// commitment transaction (but it was in Alice's, as he ACK'd her
	// changes before creating a new state), Bob needs to trigger another
	// state update in order to re-sync their states.
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// After the state transition the fee update is fully locked in, and
	// should've been removed from both channels' update logs.
	if aliceChannel.localCommitChain.tail().feePerKB != fee {
		t.Fatalf("fee not locked in")
	}
	if bobChannel.localCommitChain.tail().feePerKB != fee {
		t.Fatalf("fee not locked in")
	}
	assertNumLogUpdates(3, 1)

	// The latest commitment from both sides should have all the HTLCs.
	numAliceOutgoing := aliceChannel.localCommitChain.tail().outgoingHTLCs
	numAliceIncoming := aliceChannel.localCommitChain.tail().incomingHTLCs
	if len(numAliceOutgoing) != 3 {
		t.Fatalf("expected %v htlcs, instead got %v", 3, numAliceOutgoing)
	}
	if len(numAliceIncoming) != 1 {
		t.Fatalf("expected %v htlcs, instead got %v", 1, numAliceIncoming)
	}
	numBobOutgoing := bobChannel.localCommitChain.tail().outgoingHTLCs
	numBobIncoming := bobChannel.localCommitChain.tail().incomingHTLCs
	if len(numBobOutgoing) != 1 {
		t.Fatalf("expected %v htlcs, instead got %v", 1, numBobOutgoing)
	}
	if len(numBobIncoming) != 3 {
		t.Fatalf("expected %v htlcs, instead got %v", 3, numBobIncoming)
	}

	// TODO(roasbeef): also ensure signatures were stored
	//  * ensure expiry matches

	// Now fetch both of the channels created above from disk to simulate a
	// node restart with persistence.
	alicePub := aliceChannel.channelState.IdentityPub
	aliceChannels, err := aliceChannel.channelState.Db.FetchOpenChannels(
		alicePub,
	)
	if err != nil {
		t.Fatalf("unable to fetch channel: %v", err)
	}
	bobPub := bobChannel.channelState.IdentityPub
	bobChannels, err := bobChannel.channelState.Db.FetchOpenChannels(bobPub)
	if err != nil {
		t.Fatalf("unable to fetch channel: %v", err)
	}

	aliceChannelNew, err := NewLightningChannel(
		aliceChannel.Signer, aliceChannels[0], aliceChannel.sigPool,
	)
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}

	bobChannelNew, err := NewLightningChannel(
		bobChannel.Signer, bobChannels[0], bobChannel.sigPool,
	)
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}

	// The state update logs of the new channels and the old channels
	// should now be identical other than the height the HTLCs were added.
	if aliceChannel.localUpdateLog.logIndex !=
		aliceChannelNew.localUpdateLog.logIndex {
		t.Fatalf("alice log counter: expected %v, got %v",
			aliceChannel.localUpdateLog.logIndex,
			aliceChannelNew.localUpdateLog.logIndex)
	}
	if aliceChannel.remoteUpdateLog.logIndex !=
		aliceChannelNew.remoteUpdateLog.logIndex {
		t.Fatalf("alice log counter: expected %v, got %v",
			aliceChannel.remoteUpdateLog.logIndex,
			aliceChannelNew.remoteUpdateLog.logIndex)
	}
	if aliceChannel.localUpdateLog.Len() !=
		aliceChannelNew.localUpdateLog.Len() {
		t.Fatalf("alice log len: expected %v, got %v",
			aliceChannel.localUpdateLog.Len(),
			aliceChannelNew.localUpdateLog.Len())
	}
	if aliceChannel.remoteUpdateLog.Len() !=
		aliceChannelNew.remoteUpdateLog.Len() {
		t.Fatalf("alice log len: expected %v, got %v",
			aliceChannel.remoteUpdateLog.Len(),
			aliceChannelNew.remoteUpdateLog.Len())
	}
	if bobChannel.localUpdateLog.logIndex !=
		bobChannelNew.localUpdateLog.logIndex {
		t.Fatalf("bob log counter: expected %v, got %v",
			bobChannel.localUpdateLog.logIndex,
			bobChannelNew.localUpdateLog.logIndex)
	}
	if bobChannel.remoteUpdateLog.logIndex !=
		bobChannelNew.remoteUpdateLog.logIndex {
		t.Fatalf("bob log counter: expected %v, got %v",
			bobChannel.remoteUpdateLog.logIndex,
			bobChannelNew.remoteUpdateLog.logIndex)
	}
	if bobChannel.localUpdateLog.Len() !=
		bobChannelNew.localUpdateLog.Len() {
		t.Fatalf("bob log len: expected %v, got %v",
			bobChannel.localUpdateLog.Len(),
			bobChannelNew.localUpdateLog.Len())
	}
	if bobChannel.remoteUpdateLog.Len() !=
		bobChannelNew.remoteUpdateLog.Len() {
		t.Fatalf("bob log len: expected %v, got %v",
			bobChannel.remoteUpdateLog.Len(),
			bobChannelNew.remoteUpdateLog.Len())
	}

	// TODO(roasbeef): expand test to also ensure state revocation log has
	// proper pk scripts

	// Newly generated pkScripts for HTLCs should be the same as in the old channel.
	for _, entry := range aliceChannel.localUpdateLog.htlcIndex {
		htlc := entry.Value.(*PaymentDescriptor)
		restoredHtlc := aliceChannelNew.localUpdateLog.lookupHtlc(htlc.HtlcIndex)
		if !bytes.Equal(htlc.ourPkScript, restoredHtlc.ourPkScript) {
			t.Fatalf("alice ourPkScript in ourLog: expected %X, got %X",
				htlc.ourPkScript[:5], restoredHtlc.ourPkScript[:5])
		}
		if !bytes.Equal(htlc.theirPkScript, restoredHtlc.theirPkScript) {
			t.Fatalf("alice theirPkScript in ourLog: expected %X, got %X",
				htlc.theirPkScript[:5], restoredHtlc.theirPkScript[:5])
		}
	}
	for _, entry := range aliceChannel.remoteUpdateLog.htlcIndex {
		htlc := entry.Value.(*PaymentDescriptor)
		restoredHtlc := aliceChannelNew.remoteUpdateLog.lookupHtlc(htlc.HtlcIndex)
		if !bytes.Equal(htlc.ourPkScript, restoredHtlc.ourPkScript) {
			t.Fatalf("alice ourPkScript in theirLog: expected %X, got %X",
				htlc.ourPkScript[:5], restoredHtlc.ourPkScript[:5])
		}
		if !bytes.Equal(htlc.theirPkScript, restoredHtlc.theirPkScript) {
			t.Fatalf("alice theirPkScript in theirLog: expected %X, got %X",
				htlc.theirPkScript[:5], restoredHtlc.theirPkScript[:5])
		}
	}
	for _, entry := range bobChannel.localUpdateLog.htlcIndex {
		htlc := entry.Value.(*PaymentDescriptor)
		restoredHtlc := bobChannelNew.localUpdateLog.lookupHtlc(htlc.HtlcIndex)
		if !bytes.Equal(htlc.ourPkScript, restoredHtlc.ourPkScript) {
			t.Fatalf("bob ourPkScript in ourLog: expected %X, got %X",
				htlc.ourPkScript[:5], restoredHtlc.ourPkScript[:5])
		}
		if !bytes.Equal(htlc.theirPkScript, restoredHtlc.theirPkScript) {
			t.Fatalf("bob theirPkScript in ourLog: expected %X, got %X",
				htlc.theirPkScript[:5], restoredHtlc.theirPkScript[:5])
		}
	}
	for _, entry := range bobChannel.remoteUpdateLog.htlcIndex {
		htlc := entry.Value.(*PaymentDescriptor)
		restoredHtlc := bobChannelNew.remoteUpdateLog.lookupHtlc(htlc.HtlcIndex)
		if !bytes.Equal(htlc.ourPkScript, restoredHtlc.ourPkScript) {
			t.Fatalf("bob ourPkScript in theirLog: expected %X, got %X",
				htlc.ourPkScript[:5], restoredHtlc.ourPkScript[:5])
		}
		if !bytes.Equal(htlc.theirPkScript, restoredHtlc.theirPkScript) {
			t.Fatalf("bob theirPkScript in theirLog: expected %X, got %X",
				htlc.theirPkScript[:5], restoredHtlc.theirPkScript[:5])
		}
	}

	// Now settle all the HTLCs, then force a state update. The state
	// update should succeed as both sides have identical.
	for i := 0; i < 3; i++ {
		err := bobChannelNew.SettleHTLC(alicePreimage, uint64(i), nil, nil, nil)
		if err != nil {
			t.Fatalf("unable to settle htlc #%v: %v", i, err)
		}
		err = aliceChannelNew.ReceiveHTLCSettle(alicePreimage, uint64(i))
		if err != nil {
			t.Fatalf("unable to settle htlc#%v: %v", i, err)
		}
	}
	err = aliceChannelNew.SettleHTLC(bobPreimage, 0, nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	err = bobChannelNew.ReceiveHTLCSettle(bobPreimage, 0)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}

	// Similar to the two transitions above, as both Bob and Alice added
	// entries to the update log before a state transition was initiated by
	// either side, both sides are required to trigger an update in order
	// to lock in their changes.
	if err := ForceStateTransition(aliceChannelNew, bobChannelNew); err != nil {
		t.Fatalf("unable to update commitments: %v", err)
	}
	if err := ForceStateTransition(bobChannelNew, aliceChannelNew); err != nil {
		t.Fatalf("unable to update commitments: %v", err)
	}

	// The amounts transferred should been updated as per the amounts in
	// the HTLCs
	if aliceChannelNew.channelState.TotalMAtomsSent != htlcAmt*3 {
		t.Fatalf("expected %v alice atoms sent, got %v",
			htlcAmt*3, aliceChannelNew.channelState.TotalMAtomsSent)
	}
	if aliceChannelNew.channelState.TotalMAtomsReceived != htlcAmt {
		t.Fatalf("expected %v alice atoms received, got %v",
			htlcAmt, aliceChannelNew.channelState.TotalMAtomsReceived)
	}
	if bobChannelNew.channelState.TotalMAtomsSent != htlcAmt {
		t.Fatalf("expected %v bob atoms sent, got %v",
			htlcAmt, bobChannel.channelState.TotalMAtomsSent)
	}
	if bobChannelNew.channelState.TotalMAtomsReceived != htlcAmt*3 {
		t.Fatalf("expected %v bob atoms sent, got %v",
			htlcAmt*3, bobChannel.channelState.TotalMAtomsReceived)
	}

	// As a final test, we'll ensure that the HTLC counters for both sides
	// has been persisted properly. If we instruct Alice to add a new HTLC,
	// it should have an index of 3. If we instruct Bob to do the
	// same, it should have an index of 1.
	aliceHtlcIndex, err := aliceChannel.AddHTLC(bobh, nil)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if aliceHtlcIndex != 3 {
		t.Fatalf("wrong htlc index: expected %v, got %v", 3, aliceHtlcIndex)
	}
	bobHtlcIndex, err := bobChannel.AddHTLC(bobh, nil)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if bobHtlcIndex != 1 {
		t.Fatalf("wrong htlc index: expected %v, got %v", 1, aliceHtlcIndex)
	}
}

func TestCancelHTLC(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Add a new HTLC from Alice to Bob, then trigger a new state
	// transition in order to include it in the latest state.
	htlcAmt := lnwire.NewMAtomsFromAtoms(dcrutil.AtomsPerCoin)

	var preImage [32]byte
	copy(preImage[:], bytes.Repeat([]byte{0xaa}, 32))
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: sha256.Sum256(preImage[:]),
		Amount:      htlcAmt,
		Expiry:      10,
	}

	aliceHtlcIndex, err := aliceChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("unable to add alice htlc: %v", err)
	}
	bobHtlcIndex, err := bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("unable to add bob htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to create new commitment state: %v", err)
	}

	// With the HTLC committed, Alice's balance should reflect the clearing
	// of the new HTLC.
	aliceExpectedBalance := dcrutil.Amount(dcrutil.AtomsPerCoin*4) -
		calcStaticFee(1)
	if aliceChannel.channelState.LocalCommitment.LocalBalance.ToAtoms() !=
		aliceExpectedBalance {
		t.Fatalf("Alice's balance is wrong: expected %v, got %v",
			aliceExpectedBalance,
			aliceChannel.channelState.LocalCommitment.LocalBalance.ToAtoms())
	}

	// Now, with the HTLC committed on both sides, trigger a cancellation
	// from Bob to Alice, removing the HTLC.
	err = bobChannel.FailHTLC(bobHtlcIndex, []byte("failreason"), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	err = aliceChannel.ReceiveFailHTLC(aliceHtlcIndex, []byte("bad"))
	if err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// Now trigger another state transition, the HTLC should now be removed
	// from both sides, with balances reflected.
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to create new commitment: %v", err)
	}

	// Now HTLCs should be present on the commitment transaction for either
	// side.
	if len(aliceChannel.localCommitChain.tip().outgoingHTLCs) != 0 ||
		len(aliceChannel.remoteCommitChain.tip().outgoingHTLCs) != 0 {
		t.Fatalf("htlc's still active from alice's POV")
	}
	if len(aliceChannel.localCommitChain.tip().incomingHTLCs) != 0 ||
		len(aliceChannel.remoteCommitChain.tip().incomingHTLCs) != 0 {
		t.Fatalf("htlc's still active from alice's POV")
	}
	if len(bobChannel.localCommitChain.tip().outgoingHTLCs) != 0 ||
		len(bobChannel.remoteCommitChain.tip().outgoingHTLCs) != 0 {
		t.Fatalf("htlc's still active from bob's POV")
	}
	if len(bobChannel.localCommitChain.tip().incomingHTLCs) != 0 ||
		len(bobChannel.remoteCommitChain.tip().incomingHTLCs) != 0 {
		t.Fatalf("htlc's still active from bob's POV")
	}

	expectedBalance := dcrutil.Amount(dcrutil.AtomsPerCoin * 5)
	if aliceChannel.channelState.LocalCommitment.LocalBalance.ToAtoms() !=
		expectedBalance-calcStaticFee(0) {

		t.Fatalf("balance is wrong: expected %v, got %v",
			aliceChannel.channelState.LocalCommitment.LocalBalance.ToAtoms(),
			expectedBalance-calcStaticFee(0))
	}
	if aliceChannel.channelState.LocalCommitment.RemoteBalance.ToAtoms() !=
		expectedBalance {

		t.Fatalf("balance is wrong: expected %v, got %v",
			aliceChannel.channelState.LocalCommitment.RemoteBalance.ToAtoms(),
			expectedBalance)
	}
	if bobChannel.channelState.LocalCommitment.LocalBalance.ToAtoms() !=
		expectedBalance {

		t.Fatalf("balance is wrong: expected %v, got %v",
			bobChannel.channelState.LocalCommitment.LocalBalance.ToAtoms(),
			expectedBalance)
	}
	if bobChannel.channelState.LocalCommitment.RemoteBalance.ToAtoms() !=
		expectedBalance-calcStaticFee(0) {

		t.Fatalf("balance is wrong: expected %v, got %v",
			bobChannel.channelState.LocalCommitment.RemoteBalance.ToAtoms(),
			expectedBalance-calcStaticFee(0))
	}
}

func TestCooperativeCloseDustAdherence(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	aliceFeeRate := AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB)
	bobFeeRate := AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB)

	setDustLimit := func(dustVal dcrutil.Amount) {
		aliceChannel.channelState.LocalChanCfg.DustLimit = dustVal
		aliceChannel.channelState.RemoteChanCfg.DustLimit = dustVal
		bobChannel.channelState.LocalChanCfg.DustLimit = dustVal
		bobChannel.channelState.RemoteChanCfg.DustLimit = dustVal
	}

	resetChannelState := func() {
		aliceChannel.status = channelOpen
		bobChannel.status = channelOpen
	}

	setBalances := func(aliceBalance, bobBalance lnwire.MilliAtom) {
		aliceChannel.channelState.LocalCommitment.LocalBalance = aliceBalance
		aliceChannel.channelState.LocalCommitment.RemoteBalance = bobBalance
		bobChannel.channelState.LocalCommitment.LocalBalance = bobBalance
		bobChannel.channelState.LocalCommitment.RemoteBalance = aliceBalance
	}

	aliceDeliveryScript := alicePkScript
	bobDeliveryScript := bobPkScript

	// We'll start be initializing the limit of both Alice and Bob to 10k
	// atoms.
	dustLimit := dcrutil.Amount(10000)
	setDustLimit(dustLimit)

	// Both sides currently have over 1 DCR settled as part of their
	// balances. As a result, performing a cooperative closure now result
	// in both sides having an output within the closure transaction.
	aliceFee := aliceChannel.CalcFee(aliceFeeRate) + 1000
	aliceSig, _, _, err := aliceChannel.CreateCloseProposal(aliceFee,
		aliceDeliveryScript, bobDeliveryScript)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	aliceCloseSig := append(aliceSig, byte(txscript.SigHashAll))

	bobFee := bobChannel.CalcFee(bobFeeRate) + 1000
	bobSig, _, _, err := bobChannel.CreateCloseProposal(bobFee,
		bobDeliveryScript, aliceDeliveryScript)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	bobCloseSig := append(bobSig, byte(txscript.SigHashAll))

	closeTx, _, err := bobChannel.CompleteCooperativeClose(
		bobCloseSig, aliceCloseSig,
		bobDeliveryScript, aliceDeliveryScript, bobFee)
	if err != nil {
		t.Fatalf("unable to accept channel close: %v", err)
	}

	// The closure transaction should have exactly two outputs.
	if len(closeTx.TxOut) != 2 {
		t.Fatalf("close tx has wrong number of outputs: expected %v "+
			"got %v", 2, len(closeTx.TxOut))
	}

	// We'll reset the channel states before proceeding to our nest test.
	resetChannelState()

	// Next we'll modify the current balances and dust limits such that
	// Bob's current balance is above _below_ his dust limit.
	aliceBal := lnwire.NewMAtomsFromAtoms(dcrutil.AtomsPerCoin)
	bobBal := lnwire.NewMAtomsFromAtoms(250)
	setBalances(aliceBal, bobBal)

	// Attempt another cooperative channel closure. It should succeed
	// without any issues.
	aliceSig, _, _, err = aliceChannel.CreateCloseProposal(aliceFee,
		aliceDeliveryScript, bobDeliveryScript)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	aliceCloseSig = append(aliceSig, byte(txscript.SigHashAll))

	bobSig, _, _, err = bobChannel.CreateCloseProposal(bobFee,
		bobDeliveryScript, aliceDeliveryScript)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	bobCloseSig = append(bobSig, byte(txscript.SigHashAll))

	closeTx, _, err = bobChannel.CompleteCooperativeClose(
		bobCloseSig, aliceCloseSig,
		bobDeliveryScript, aliceDeliveryScript, bobFee)
	if err != nil {
		t.Fatalf("unable to accept channel close: %v", err)
	}

	// The closure transaction should only have a single output, and that
	// output should be Alice's balance.
	if len(closeTx.TxOut) != 1 {
		t.Fatalf("close tx has wrong number of outputs: expected %v "+
			"got %v", 1, len(closeTx.TxOut))
	}
	commitFee := aliceChannel.channelState.LocalCommitment.CommitFee
	aliceExpectedBalance := aliceBal.ToAtoms() - aliceFee + commitFee
	if closeTx.TxOut[0].Value != int64(aliceExpectedBalance) {
		t.Fatalf("alice's balance is incorrect: expected %v, got %v",
			aliceExpectedBalance,
			closeTx.TxOut[0].Value)
	}

	// Finally, we'll modify the current balances and dust limits such that
	// Alice's current balance is _below_ his her limit.
	setBalances(bobBal, aliceBal)
	resetChannelState()

	// Our final attempt at another cooperative channel closure. It should
	// succeed without any issues.
	aliceSig, _, _, err = aliceChannel.CreateCloseProposal(
		aliceFee, aliceDeliveryScript, bobDeliveryScript,
	)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	aliceCloseSig = append(aliceSig, byte(txscript.SigHashAll))

	bobSig, _, _, err = bobChannel.CreateCloseProposal(
		bobFee, bobDeliveryScript, aliceDeliveryScript,
	)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	bobCloseSig = append(bobSig, byte(txscript.SigHashAll))

	closeTx, _, err = bobChannel.CompleteCooperativeClose(
		bobCloseSig, aliceCloseSig,
		bobDeliveryScript, aliceDeliveryScript, bobFee)
	if err != nil {
		t.Fatalf("unable to accept channel close: %v", err)
	}

	// The closure transaction should only have a single output, and that
	// output should be Bob's balance.
	if len(closeTx.TxOut) != 1 {
		t.Fatalf("close tx has wrong number of outputs: expected %v "+
			"got %v", 1, len(closeTx.TxOut))
	}
	if closeTx.TxOut[0].Value != int64(aliceBal.ToAtoms()) {
		t.Fatalf("bob's balance is incorrect: expected %v, got %v",
			aliceBal.ToAtoms(), closeTx.TxOut[0].Value)
	}
}

// TestUpdateFeeAdjustments tests that the state machine is able to properly
// accept valid fee changes, as well as reject any invalid fee updates.
func TestUpdateFeeAdjustments(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First, we'll grab the current base fee rate as we'll be using this
	// to make relative adjustments int he fee rate.
	baseFeeRate := aliceChannel.channelState.LocalCommitment.FeePerKB

	// We'll first try to increase the fee rate 5x, this should be able to
	// be committed without any issue.
	newFee := AtomPerKByte(baseFeeRate * 5)

	if err := aliceChannel.UpdateFee(newFee); err != nil {
		t.Fatalf("unable to alice update fee: %v", err)
	}
	if err := bobChannel.ReceiveUpdateFee(newFee); err != nil {
		t.Fatalf("unable to bob update fee: %v", err)
	}

	// With the fee updates applied, we'll now initiate a state transition
	// to ensure the fee update is locked in.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to create new commitment: %v", err)
	}

	// We'll now attempt to increase the fee rate 1,000,000x of the base
	// fee.  This should result in an error as Alice won't be able to pay
	// this new fee rate.
	newFee = AtomPerKByte(baseFeeRate * 1000000)
	if err := aliceChannel.UpdateFee(newFee); err == nil {
		t.Fatalf("alice should reject the fee rate")
	}

	// Finally, we'll attempt to adjust the fee down and use a fee which is
	// smaller than the initial base fee rate. The fee application and
	// state transition should proceed without issue.
	newFee = AtomPerKByte(baseFeeRate / 10)
	if err := aliceChannel.UpdateFee(newFee); err != nil {
		t.Fatalf("unable to alice update fee: %v", err)
	}
	if err := bobChannel.ReceiveUpdateFee(newFee); err != nil {
		t.Fatalf("unable to bob update fee: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to create new commitment: %v", err)
	}
}

// TestUpdateFeeFail tests that the signature verification will fail if they
// fee updates are out of sync.
func TestUpdateFeeFail(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Bob receives the update, that will apply to his commitment
	// transaction.
	if err := bobChannel.ReceiveUpdateFee(2e4); err != nil {
		t.Fatalf("unable to apply fee update: %v", err)
	}

	// Alice sends signature for commitment that does not cover any fee
	// update.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// Bob verifies this commit, meaning that he checks that it is
	// consistent everything he has received. This should fail, since he got
	// the fee update, but Alice never sent it.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err == nil {
		t.Fatalf("expected bob to fail receiving alice's signature")
	}

}

// TestUpdateFeeConcurrentSig tests that the channel can properly handle a fee
// update that it receives concurrently with signing its next commitment.
func TestUpdateFeeConcurrentSig(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	paymentPreimage := bytes.Repeat([]byte{1}, 32)
	paymentHash := sha256.Sum256(paymentPreimage)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      dcrutil.AtomsPerCoin,
		Expiry:      uint32(5),
	}

	// First Alice adds the outgoing HTLC to her local channel's state
	// update log. Then Alice sends this wire message over to Bob who
	// adds this htlc to his remote state update log.
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Simulate Alice sending update fee message to bob.
	fee := AtomPerKByte(2e4)
	if err := aliceChannel.UpdateFee(fee); err != nil {
		t.Fatalf("unable to send fee update")
	}

	// Alice signs a commitment, and sends this to bob.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// At the same time, Bob signs a commitment.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign alice's commitment: %v", err)
	}

	// ...that Alice receives.
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	// Now let Bob receive the fee update + commitment that Alice sent.
	if err := bobChannel.ReceiveUpdateFee(fee); err != nil {
		t.Fatalf("unable to receive fee update")
	}

	// Bob receives this signature message, and verifies that it is
	// consistent with the state he had for Alice, including the received
	// HTLC and fee update.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) == fee {
		t.Fatalf("bob's feePerKw was unexpectedly locked in")
	}

	// Bob can revoke the prior commitment he had. This should lock in the
	// fee update for him.
	_, _, err = bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}

	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) != fee {
		t.Fatalf("bob's feePerKw was not locked in")
	}
}

// TestUpdateFeeSenderCommits verifies that the state machine progresses as
// expected if we send a fee update, and then the sender of the fee update
// sends a commitment signature.
func TestUpdateFeeSenderCommits(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	paymentPreimage := bytes.Repeat([]byte{1}, 32)
	paymentHash := sha256.Sum256(paymentPreimage)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      dcrutil.AtomsPerCoin,
		Expiry:      uint32(5),
	}

	// First Alice adds the outgoing HTLC to her local channel's state
	// update log. Then Alice sends this wire message over to Bob who
	// adds this htlc to his remote state update log.
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Simulate Alice sending update fee message to bob.
	fee := AtomPerKByte(2e4)
	if err = aliceChannel.UpdateFee(fee); err != nil {
		t.Fatalf("alice unable to update fee: %v", err)
	}
	if err = bobChannel.ReceiveUpdateFee(fee); err != nil {
		t.Fatalf("bob unable to receive update fee: %v", err)
	}

	// Alice signs a commitment, which will cover everything sent to Bob
	// (the HTLC and the fee update), and everything acked by Bob (nothing
	// so far).
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// Bob receives this signature message, and verifies that it is
	// consistent with the state he had for Alice, including the received
	// HTLC and fee update.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) == fee {
		t.Fatalf("bob's FeePerKB was unexpectedly locked in")
	}

	// Bob can revoke the prior commitment he had. This should lock in the
	// fee update for him.
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}

	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) != fee {
		t.Fatalf("bob's FeePerKB was not locked in")
	}

	// Bob commits to all updates he has received from Alice. This includes
	// the HTLC he received, and the fee update.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign alice's commitment: %v", err)
	}

	// Alice receives the revocation of the old one, and can now assume
	// that Bob's received everything up to the signature she sent,
	// including the HTLC and fee update.
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	}

	// Alice receives new signature from Bob, and assumes this covers the
	// changes.
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) == fee {
		t.Fatalf("alice's FeePerKB was unexpectedly locked in")
	}

	// Alice can revoke the old commitment, which will lock in the fee
	// update.
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke alice channel: %v", err)
	}

	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) != fee {
		t.Fatalf("alice's FeePerKB was not locked in")
	}

	// Bob receives revocation from Alice.
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}

}

// TestUpdateFeeReceiverCommits tests that the state machine progresses as
// expected if we send a fee update, and then the receiver of the fee update
// sends a commitment signature.
func TestUpdateFeeReceiverCommits(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	paymentPreimage := bytes.Repeat([]byte{1}, 32)
	paymentHash := sha256.Sum256(paymentPreimage)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      dcrutil.AtomsPerCoin,
		Expiry:      uint32(5),
	}

	// First Alice adds the outgoing HTLC to her local channel's state
	// update log. Then Alice sends this wire message over to Bob who
	// adds this htlc to his remote state update log.
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Simulate Alice sending update fee message to bob
	fee := AtomPerKByte(2e4)
	if err = aliceChannel.UpdateFee(fee); err != nil {
		t.Fatalf("unable to update fee: %v", err)
	}
	if err = bobChannel.ReceiveUpdateFee(fee); err != nil {
		t.Fatalf("unable to receive update fee: %v", err)
	}

	// Bob commits to every change he has sent since last time (none). He
	// does not commit to the received HTLC and fee update, since Alice
	// cannot know if he has received them.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// Alice receives this signature message, and verifies that it is
	// consistent with the remote state, not including any of the updates.
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	// Alice can revoke the prior commitment she had, this will ack
	// everything received before last commitment signature, but in this
	// case that is nothing.
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}

	// Bob receives the revocation of the old commitment
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	}

	// Alice will sign next commitment. Since she sent the revocation, she
	// also ack'ed everything received, but in this case this is nothing.
	// Since she sent the two updates, this signature will cover those two.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign alice's commitment: %v", err)
	}

	// Bob gets the signature for the new commitment from Alice. He assumes
	// this covers everything received from alice, including the two updates.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) == fee {
		t.Fatalf("bob's FeePerKB was unexpectedly locked in")
	}

	// Bob can revoke the old commitment. This will ack what he has
	// received, including the HTLC and fee update. This will lock in the
	// fee update for bob.
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke alice channel: %v", err)
	}

	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) != fee {
		t.Fatalf("bob's FeePerKB was not locked in")
	}

	// Bob will send a new signature, which will cover what he just acked:
	// the HTLC and fee update.
	bobSig, bobHtlcSigs, _, err = bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	// Alice receives revocation from Bob, and can now be sure that Bob
	// received the two updates, and they are considered locked in.
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}

	// Alice will receive the signature from Bob, which will cover what was
	// just acked by his revocation.
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) == fee {
		t.Fatalf("alice's FeePerKB was unexpectedly locked in")
	}

	// After Alice now revokes her old commitment, the fee update should
	// lock in.
	aliceRevocation, _, err = aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}

	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) != fee {
		t.Fatalf("Alice's FeePerKB was not locked in")
	}

	// Bob receives revocation from Alice.
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}
}

// TestUpdateFeeReceiverSendsUpdate tests that receiving a fee update as channel
// initiator fails, and that trying to initiate fee update as non-initiation
// fails.
func TestUpdateFeeReceiverSendsUpdate(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Since Alice is the channel initiator, she should fail when receiving
	// fee update
	fee := AtomPerKByte(2e4)
	err = aliceChannel.ReceiveUpdateFee(fee)
	if err == nil {
		t.Fatalf("expected alice to fail receiving fee update")
	}

	// Similarly, initiating fee update should fail for Bob.
	err = bobChannel.UpdateFee(fee)
	if err == nil {
		t.Fatalf("expected bob to fail initiating fee update")
	}
}

// Test that if multiple update fee messages are sent consecutively, then the
// last one is the one that is being committed to.
func TestUpdateFeeMultipleUpdates(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Simulate Alice sending update fee message to bob.
	fee1 := AtomPerKByte(2e4)
	fee2 := AtomPerKByte(2e4)
	fee := AtomPerKByte(2e4)
	if err = aliceChannel.UpdateFee(fee1); err != nil {
		t.Fatalf("alice unable to update fee1: %v", err)
	}
	if err = aliceChannel.UpdateFee(fee2); err != nil {
		t.Fatalf("alice unable to update fee2: %v", err)
	}
	if err = aliceChannel.UpdateFee(fee); err != nil {
		t.Fatalf("alice unable to update fee: %v", err)
	}

	// Alice signs a commitment, which will cover everything sent to Bob
	// (the HTLC and the fee update), and everything acked by Bob (nothing
	// so far).
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("alice unable to sign commitment: %v", err)
	}

	if err = bobChannel.ReceiveUpdateFee(fee1); err != nil {
		t.Fatalf("bob unable to receive update fee1: %v", err)
	}
	if err = bobChannel.ReceiveUpdateFee(fee2); err != nil {
		t.Fatalf("bob unable to receive update fee2: %v", err)
	}
	if err = bobChannel.ReceiveUpdateFee(fee); err != nil {
		t.Fatalf("bob unable to receive update fee: %v", err)
	}

	// Bob receives this signature message, and verifies that it is
	// consistent with the state he had for Alice, including the received
	// HTLC and fee update.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's new commitment: %v", err)
	}

	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) == fee {
		t.Fatalf("bob's FeePerKB was unexpectedly locked in")
	}

	// Alice sending more fee updates now should not mess up the old fee
	// they both committed to.
	fee3 := AtomPerKByte(444)
	fee4 := AtomPerKByte(555)
	fee5 := AtomPerKByte(666)
	if err = aliceChannel.UpdateFee(fee3); err != nil {
		t.Fatalf("alice unable to update fee3: %v", err)
	}
	if err = aliceChannel.UpdateFee(fee4); err != nil {
		t.Fatalf("alice unable to update fee4: %v", err)
	}
	if err = aliceChannel.UpdateFee(fee5); err != nil {
		t.Fatalf("alice unable to update fee5: %v", err)
	}
	if err = bobChannel.ReceiveUpdateFee(fee3); err != nil {
		t.Fatalf("bob unable to receive update fee3: %v", err)
	}
	if err = bobChannel.ReceiveUpdateFee(fee4); err != nil {
		t.Fatalf("bob unable to receive update fee4: %v", err)
	}
	if err = bobChannel.ReceiveUpdateFee(fee5); err != nil {
		t.Fatalf("bob unable to receive update fee5; %v", err)
	}

	// Bob can revoke the prior commitment he had. This should lock in the
	// fee update for him.
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to generate bob revocation: %v", err)
	}

	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) != fee {
		t.Fatalf("bob's FeePerKB was not locked in")
	}

	// Bob commits to all updates he has received from Alice. This includes
	// the HTLC he received, and the fee update.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign alice's commitment: %v", err)
	}

	// Alice receives the revocation of the old one, and can now assume that
	// Bob's received everything up to the signature she sent, including the
	// HTLC and fee update.
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to process bob's revocation: %v", err)
	}

	// Alice receives new signature from Bob, and assumes this covers the
	// changes.
	if err := aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs); err != nil {
		t.Fatalf("alice unable to process bob's new commitment: %v", err)
	}

	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) == fee {
		t.Fatalf("alice's FeePerKB was unexpectedly locked in")
	}

	// Alice can revoke the old commitment, which will lock in the fee
	// update.
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke alice channel: %v", err)
	}

	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) != fee {
		t.Fatalf("alice's FeePerKB was not locked in")
	}

	// Bob receives revocation from Alice.
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}
}

// TestAddHTLCNegativeBalance tests that if enough HTLC's are added to the
// state machine to drive the balance to zero, then the next HTLC attempted to
// be added will result in an error being returned.
func TestAddHTLCNegativeBalance(t *testing.T) {
	t.Parallel()

	// We'll kick off the test by creating our channels which both are
	// loaded with 5 DCR each.
	aliceChannel, _, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We set the channel reserve to 0, such that we can add HTLCs all the
	// way to a negative balance.
	aliceChannel.localChanCfg.ChanReserve = 0

	// First, we'll add 3 HTLCs of 1 DCR each to Alice's commitment.
	const numHTLCs = 3
	htlcAmt := lnwire.NewMAtomsFromAtoms(dcrutil.AtomsPerCoin)
	for i := 0; i < numHTLCs; i++ {
		htlc, _ := createHTLC(i, htlcAmt)
		if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
			t.Fatalf("unable to add htlc: %v", err)
		}
	}

	// Alice now has an available balance of 2 DCR. We'll add a new HTLC of
	// value 2 DCR, which should make Alice's balance negative (since she
	// has to pay a commitment fee).
	htlcAmt = lnwire.NewMAtomsFromAtoms(2 * dcrutil.AtomsPerCoin)
	htlc, _ := createHTLC(numHTLCs+1, htlcAmt)
	_, err = aliceChannel.AddHTLC(htlc, nil)
	if err != ErrBelowChanReserve {
		t.Fatalf("expected balance below channel reserve, instead "+
			"got: %v", err)
	}
}

// assertNoChanSyncNeeded is a helper function that asserts that upon restart,
// two channels conclude that they're fully synchronized and don't need to
// retransmit any new messages.
func assertNoChanSyncNeeded(t *testing.T, aliceChannel *LightningChannel,
	bobChannel *LightningChannel) {

	_, _, line, _ := runtime.Caller(1)

	aliceChanSyncMsg, err := aliceChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("line #%v: unable to produce chan sync msg: %v",
			line, err)
	}
	bobMsgsToSend, _, _, err := bobChannel.ProcessChanSyncMsg(aliceChanSyncMsg)
	if err != nil {
		t.Fatalf("line #%v: unable to process ChannelReestablish "+
			"msg: %v", line, err)
	}
	if len(bobMsgsToSend) != 0 {
		t.Fatalf("line #%v: bob shouldn't have to send any messages, "+
			"instead wants to send: %v", line, spew.Sdump(bobMsgsToSend))
	}

	bobChanSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("line #%v: unable to produce chan sync msg: %v",
			line, err)
	}
	aliceMsgsToSend, _, _, err := aliceChannel.ProcessChanSyncMsg(bobChanSyncMsg)
	if err != nil {
		t.Fatalf("line #%v: unable to process ChannelReestablish "+
			"msg: %v", line, err)
	}
	if len(bobMsgsToSend) != 0 {
		t.Fatalf("line #%v: alice shouldn't have to send any "+
			"messages, instead wants to send: %v", line,
			spew.Sdump(aliceMsgsToSend))
	}
}

// TestChanSyncFullySynced tests that after a successful commitment exchange,
// and a forced restart, both nodes conclude that they're fully synchronized
// and don't need to retransmit any messages.
func TestChanSyncFullySynced(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// If we exchange channel sync messages from the get-go , then both
	// sides should conclude that no further synchronization is needed.
	assertNoChanSyncNeeded(t, aliceChannel, bobChannel)

	// Next, we'll create an HTLC for Alice to extend to Bob.
	var paymentPreimage [32]byte
	copy(paymentPreimage[:], bytes.Repeat([]byte{1}, 32))
	paymentHash := sha256.Sum256(paymentPreimage[:])
	htlcAmt := lnwire.NewMAtomsFromAtoms(dcrutil.AtomsPerCoin)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      htlcAmt,
		Expiry:      uint32(5),
	}
	aliceHtlcIndex, err := aliceChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	bobHtlcIndex, err := bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Then we'll initiate a state transition to lock in this new HTLC.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete alice's state transition: %v", err)
	}

	// At this point, if both sides generate a ChannelReestablish message,
	// they should both conclude that they're fully in sync.
	assertNoChanSyncNeeded(t, aliceChannel, bobChannel)

	// If bob settles the HTLC, and then initiates a state transition, they
	// should both still think that they're in sync.
	err = bobChannel.SettleHTLC(paymentPreimage, bobHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	err = aliceChannel.ReceiveHTLCSettle(paymentPreimage, aliceHtlcIndex)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}

	// Next, we'll complete Bob's state transition, and assert again that
	// they think they're fully synced.
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}
	assertNoChanSyncNeeded(t, aliceChannel, bobChannel)

	// Finally, if we simulate a restart on both sides, then both should
	// still conclude that they don't need to synchronize their state.
	alicePub := aliceChannel.channelState.IdentityPub
	aliceChannels, err := aliceChannel.channelState.Db.FetchOpenChannels(
		alicePub,
	)
	if err != nil {
		t.Fatalf("unable to fetch channel: %v", err)
	}
	bobPub := bobChannel.channelState.IdentityPub
	bobChannels, err := bobChannel.channelState.Db.FetchOpenChannels(bobPub)
	if err != nil {
		t.Fatalf("unable to fetch channel: %v", err)
	}

	aliceChannelNew, err := NewLightningChannel(
		aliceChannel.Signer, aliceChannels[0], aliceChannel.sigPool,
	)
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}
	bobChannelNew, err := NewLightningChannel(
		bobChannel.Signer, bobChannels[0], bobChannel.sigPool,
	)
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}

	assertNoChanSyncNeeded(t, aliceChannelNew, bobChannelNew)
}

// restartChannel reads the passed channel from disk, and returns a newly
// initialized instance. This simulates one party restarting and losing their
// in memory state.
func restartChannel(channelOld *LightningChannel) (*LightningChannel, error) {
	nodePub := channelOld.channelState.IdentityPub
	nodeChannels, err := channelOld.channelState.Db.FetchOpenChannels(
		nodePub,
	)
	if err != nil {
		return nil, err
	}

	channelNew, err := NewLightningChannel(
		channelOld.Signer, nodeChannels[0],
		channelOld.sigPool,
	)
	if err != nil {
		return nil, err
	}

	return channelNew, nil
}

// TestChanSyncOweCommitment tests that if Bob restarts (and then Alice) before
// he receives Alice's CommitSig message, then Alice concludes that she needs
// to re-send the CommitDiff. After the diff has been sent, both nodes should
// resynchronize and be able to complete the dangling commit.
func TestChanSyncOweCommitment(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	var fakeOnionBlob [lnwire.OnionPacketSize]byte
	copy(fakeOnionBlob[:], bytes.Repeat([]byte{0x05}, lnwire.OnionPacketSize))

	// We'll start off the scenario with Bob sending 3 HTLC's to Alice in a
	// single state update.
	htlcAmt := lnwire.NewMAtomsFromAtoms(20000)
	const numBobHtlcs = 3
	var bobPreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xbb}, 32))
	for i := 0; i < 3; i++ {
		rHash := sha256.Sum256(bobPreimage[:])
		h := &lnwire.UpdateAddHTLC{
			PaymentHash: rHash,
			Amount:      htlcAmt,
			Expiry:      uint32(10),
			OnionBlob:   fakeOnionBlob,
		}

		htlcIndex, err := bobChannel.AddHTLC(h, nil)
		if err != nil {
			t.Fatalf("unable to add bob's htlc: %v", err)
		}

		h.ID = htlcIndex
		if _, err := aliceChannel.ReceiveHTLC(h); err != nil {
			t.Fatalf("unable to recv bob's htlc: %v", err)
		}
	}

	chanID := lnwire.NewChanIDFromOutPoint(
		&aliceChannel.channelState.FundingOutpoint,
	)

	// With the HTLC's applied to both update logs, we'll initiate a state
	// transition from Bob.
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// Next, Alice's settles all 3 HTLC's from Bob, and also adds a new
	// HTLC of her own.
	for i := 0; i < 3; i++ {
		err := aliceChannel.SettleHTLC(bobPreimage, uint64(i), nil, nil, nil)
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}
		err = bobChannel.ReceiveHTLCSettle(bobPreimage, uint64(i))
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}
	}
	var alicePreimage [32]byte
	copy(alicePreimage[:], bytes.Repeat([]byte{0xaa}, 32))
	rHash := sha256.Sum256(alicePreimage[:])
	aliceHtlc := &lnwire.UpdateAddHTLC{
		ChanID:      chanID,
		PaymentHash: rHash,
		Amount:      htlcAmt,
		Expiry:      uint32(10),
		OnionBlob:   fakeOnionBlob,
	}
	aliceHtlcIndex, err := aliceChannel.AddHTLC(aliceHtlc, nil)
	if err != nil {
		t.Fatalf("unable to add alice's htlc: %v", err)
	}
	bobHtlcIndex, err := bobChannel.ReceiveHTLC(aliceHtlc)
	if err != nil {
		t.Fatalf("unable to recv alice's htlc: %v", err)
	}

	// Now we'll begin the core of the test itself. Alice will extend a new
	// commitment to Bob, but the connection drops before Bob can process
	// it.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// Bob doesn't get this message so upon reconnection, they need to
	// synchronize. Alice should conclude that she owes Bob a commitment,
	// while Bob should think he's properly synchronized.
	aliceSyncMsg, err := aliceChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}

	// This is a helper function that asserts Alice concludes that she
	// needs to retransmit the exact commitment that we failed to send
	// above.
	assertAliceCommitRetransmit := func() {
		aliceMsgsToSend, _, _, err := aliceChannel.ProcessChanSyncMsg(
			bobSyncMsg,
		)
		if err != nil {
			t.Fatalf("unable to process chan sync msg: %v", err)
		}
		if len(aliceMsgsToSend) != 5 {
			t.Fatalf("expected alice to send %v messages instead "+
				"will send %v: %v", 5, len(aliceMsgsToSend),
				spew.Sdump(aliceMsgsToSend))
		}

		// Each of the settle messages that Alice sent should match her
		// original intent.
		for i := 0; i < 3; i++ {
			settleMsg, ok := aliceMsgsToSend[i].(*lnwire.UpdateFulfillHTLC)
			if !ok {
				t.Fatalf("expected an HTLC settle message, "+
					"instead have %v", spew.Sdump(settleMsg))
			}
			if settleMsg.ID != uint64(i) {
				t.Fatalf("wrong ID in settle msg: expected %v, "+
					"got %v", i, settleMsg.ID)
			}
			if settleMsg.ChanID != chanID {
				t.Fatalf("incorrect chan id: expected %v, got %v",
					chanID, settleMsg.ChanID)
			}
			if settleMsg.PaymentPreimage != bobPreimage {
				t.Fatalf("wrong pre-image: expected %v, got %v",
					alicePreimage, settleMsg.PaymentPreimage)
			}
		}

		// The HTLC add message should be identical.
		if _, ok := aliceMsgsToSend[3].(*lnwire.UpdateAddHTLC); !ok {
			t.Fatalf("expected an HTLC add message, instead have %v",
				spew.Sdump(aliceMsgsToSend[3]))
		}
		if !reflect.DeepEqual(aliceHtlc, aliceMsgsToSend[3]) {
			t.Fatalf("htlc msg doesn't match exactly: "+
				"expected %v got %v", spew.Sdump(aliceHtlc),
				spew.Sdump(aliceMsgsToSend[3]))
		}

		// Next, we'll ensure that the CommitSig message exactly
		// matches what Alice originally intended to send.
		commitSigMsg, ok := aliceMsgsToSend[4].(*lnwire.CommitSig)
		if !ok {
			t.Fatalf("expected a CommitSig message, instead have %v",
				spew.Sdump(aliceMsgsToSend[4]))
		}
		if commitSigMsg.CommitSig != aliceSig {
			t.Fatalf("commit sig msgs don't match: expected %x got %x",
				aliceSig, commitSigMsg.CommitSig)
		}
		if len(commitSigMsg.HtlcSigs) != len(aliceHtlcSigs) {
			t.Fatalf("wrong number of htlc sigs: expected %v, got %v",
				len(aliceHtlcSigs), len(commitSigMsg.HtlcSigs))
		}
		for i, htlcSig := range commitSigMsg.HtlcSigs {
			if htlcSig != aliceHtlcSigs[i] {
				t.Fatalf("htlc sig msgs don't match: "+
					"expected %x got %x",
					aliceHtlcSigs[i],
					htlcSig)
			}
		}
	}

	// Alice should detect that she needs to re-send 5 messages: the 3
	// settles, her HTLC add, and finally her commit sig message.
	assertAliceCommitRetransmit()

	// From Bob's Pov he has nothing else to send, so he should conclude he
	// has no further action remaining.
	bobMsgsToSend, _, _, err := bobChannel.ProcessChanSyncMsg(aliceSyncMsg)
	if err != nil {
		t.Fatalf("unable to process chan sync msg: %v", err)
	}
	if len(bobMsgsToSend) != 0 {
		t.Fatalf("expected bob to send %v messages instead will "+
			"send %v: %v", 5, len(bobMsgsToSend),
			spew.Sdump(bobMsgsToSend))
	}

	// If we restart Alice, she should still conclude that she needs to
	// send the exact same set of messages.
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}
	assertAliceCommitRetransmit()

	// TODO(roasbeef): restart bob as well???

	// At this point, we should be able to resume the prior state update
	// without any issues, resulting in Alice settling the 3 htlc's, and
	// adding one of her own.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's commitment: %v", err)
	}
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke bob commitment: %v", err)
	}
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign commitment: %v", err)
	}
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to recv revocation: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to rev bob's commitment: %v", err)
	}
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to revoke commitment: %v", err)
	}
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to recv revocation: %v", err)
	}

	// At this point, we'll now assert that their log states are what we
	// expect.
	//
	// Alice's local log counter should be 4 and her HTLC index 3. She
	// should detect Bob's remote log counter as being 3 and his HTLC index
	// 3 as well.
	if aliceChannel.localUpdateLog.logIndex != 4 {
		t.Fatalf("incorrect log index: expected %v, got %v", 4,
			aliceChannel.localUpdateLog.logIndex)
	}
	if aliceChannel.localUpdateLog.htlcCounter != 1 {
		t.Fatalf("incorrect htlc index: expected %v, got %v", 1,
			aliceChannel.localUpdateLog.htlcCounter)
	}
	if aliceChannel.remoteUpdateLog.logIndex != 3 {
		t.Fatalf("incorrect log index: expected %v, got %v", 3,
			aliceChannel.localUpdateLog.logIndex)
	}
	if aliceChannel.remoteUpdateLog.htlcCounter != 3 {
		t.Fatalf("incorrect htlc index: expected %v, got %v", 3,
			aliceChannel.localUpdateLog.htlcCounter)
	}

	// Bob should also have the same state, but mirrored.
	if bobChannel.localUpdateLog.logIndex != 3 {
		t.Fatalf("incorrect log index: expected %v, got %v", 3,
			bobChannel.localUpdateLog.logIndex)
	}
	if bobChannel.localUpdateLog.htlcCounter != 3 {
		t.Fatalf("incorrect htlc index: expected %v, got %v", 3,
			bobChannel.localUpdateLog.htlcCounter)
	}
	if bobChannel.remoteUpdateLog.logIndex != 4 {
		t.Fatalf("incorrect log index: expected %v, got %v", 4,
			bobChannel.localUpdateLog.logIndex)
	}
	if bobChannel.remoteUpdateLog.htlcCounter != 1 {
		t.Fatalf("incorrect htlc index: expected %v, got %v", 1,
			bobChannel.localUpdateLog.htlcCounter)
	}

	// We'll conclude the test by having Bob settle Alice's HTLC, then
	// initiate a state transition.
	err = bobChannel.SettleHTLC(alicePreimage, bobHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	err = aliceChannel.ReceiveHTLCSettle(alicePreimage, aliceHtlcIndex)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// At this point, the final balances of both parties should properly
	// reflect the amount of HTLC's sent.
	bobMAtomsSent := numBobHtlcs * htlcAmt
	if aliceChannel.channelState.TotalMAtomsSent != htlcAmt {
		t.Fatalf("wrong value for mAt sent: expected %v, got %v",
			htlcAmt, aliceChannel.channelState.TotalMAtomsSent)
	}
	if aliceChannel.channelState.TotalMAtomsReceived != bobMAtomsSent {
		t.Fatalf("wrong value for mAt recv: expected %v, got %v",
			bobMAtomsSent, aliceChannel.channelState.TotalMAtomsReceived)
	}
	if bobChannel.channelState.TotalMAtomsSent != bobMAtomsSent {
		t.Fatalf("wrong value for mAt sent: expected %v, got %v",
			bobMAtomsSent, bobChannel.channelState.TotalMAtomsSent)
	}
	if bobChannel.channelState.TotalMAtomsReceived != htlcAmt {
		t.Fatalf("wrong value for mAt recv: expected %v, got %v",
			htlcAmt, bobChannel.channelState.TotalMAtomsReceived)
	}
}

// TestChanSyncOweRevocation tests that if Bob restarts (and then Alice) before
// he receiver's Alice's RevokeAndAck message, then Alice concludes that she
// needs to re-send the RevokeAndAck. After the revocation has been sent, both
// nodes should be able to successfully complete another state transition.
func TestChanSyncOweRevocation(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	chanID := lnwire.NewChanIDFromOutPoint(
		&aliceChannel.channelState.FundingOutpoint,
	)

	// We'll start the test with Bob extending a single HTLC to Alice, and
	// then initiating a state transition.
	htlcAmt := lnwire.NewMAtomsFromAtoms(20000)
	var bobPreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xaa}, 32))
	rHash := sha256.Sum256(bobPreimage[:])
	bobHtlc := &lnwire.UpdateAddHTLC{
		ChanID:      chanID,
		PaymentHash: rHash,
		Amount:      htlcAmt,
		Expiry:      uint32(10),
	}
	bobHtlcIndex, err := bobChannel.AddHTLC(bobHtlc, nil)
	if err != nil {
		t.Fatalf("unable to add bob's htlc: %v", err)
	}
	aliceHtlcIndex, err := aliceChannel.ReceiveHTLC(bobHtlc)
	if err != nil {
		t.Fatalf("unable to recv bob's htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// Next, Alice will settle that single HTLC, the _begin_ the start of a
	// state transition.
	err = aliceChannel.SettleHTLC(bobPreimage, aliceHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	err = bobChannel.ReceiveHTLCSettle(bobPreimage, bobHtlcIndex)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}

	// We'll model the state transition right up until Alice needs to send
	// her revocation message to complete the state transition.
	//
	// Alice signs the next state, then Bob receives and sends his
	// revocation message.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's commitment: %v", err)
	}

	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke bob commitment: %v", err)
	}
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign commitment: %v", err)
	}

	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to recv revocation: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to rev bob's commitment: %v", err)
	}

	// At this point, we'll simulate the connection breaking down by Bob's
	// lack of knowledge of the revocation message that Alice just sent.
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to revoke commitment: %v", err)
	}

	// If we fetch the channel sync messages at this state, then Alice
	// should report that she owes Bob a revocation message, while Bob
	// thinks they're fully in sync.
	aliceSyncMsg, err := aliceChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}

	assertAliceOwesRevoke := func() {
		aliceMsgsToSend, _, _, err := aliceChannel.ProcessChanSyncMsg(bobSyncMsg)
		if err != nil {
			t.Fatalf("unable to process chan sync msg: %v", err)
		}
		if len(aliceMsgsToSend) != 1 {
			t.Fatalf("expected single message retransmission from Alice, "+
				"instead got %v", spew.Sdump(aliceMsgsToSend))
		}
		aliceReRevoke, ok := aliceMsgsToSend[0].(*lnwire.RevokeAndAck)
		if !ok {
			t.Fatalf("expected to retransmit revocation msg, instead "+
				"have: %v", spew.Sdump(aliceMsgsToSend[0]))
		}

		// Alice should re-send the revocation message for her prior
		// state.
		expectedRevocation, err := aliceChannel.generateRevocation(
			aliceChannel.currentHeight - 1,
		)
		if err != nil {
			t.Fatalf("unable to regenerate revocation: %v", err)
		}
		if !reflect.DeepEqual(expectedRevocation, aliceReRevoke) {
			t.Fatalf("wrong re-revocation: expected %v, got %v",
				expectedRevocation, aliceReRevoke)
		}
	}

	// From Bob's PoV he shouldn't think that he owes Alice any messages.
	bobMsgsToSend, _, _, err := bobChannel.ProcessChanSyncMsg(aliceSyncMsg)
	if err != nil {
		t.Fatalf("unable to process chan sync msg: %v", err)
	}
	if len(bobMsgsToSend) != 0 {
		t.Fatalf("expected bob to not retransmit, instead has: %v",
			spew.Sdump(bobMsgsToSend))
	}

	// Alice should detect that she owes Bob a revocation message, and only
	// that single message.
	assertAliceOwesRevoke()

	// If we restart Alice, then she should still decide that she owes a
	// revocation message to Bob.
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}
	assertAliceOwesRevoke()

	// TODO(roasbeef): restart bob too???

	// We'll continue by then allowing bob to process Alice's revocation message.
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to recv revocation: %v", err)
	}

	// Finally, Alice will add an HTLC over her own such that we assert the
	// channel can continue to receive updates.
	var alicePreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xaa}, 32))
	rHash = sha256.Sum256(alicePreimage[:])
	aliceHtlc := &lnwire.UpdateAddHTLC{
		ChanID:      chanID,
		PaymentHash: rHash,
		Amount:      htlcAmt,
		Expiry:      uint32(10),
	}
	if _, err := aliceChannel.AddHTLC(aliceHtlc, nil); err != nil {
		t.Fatalf("unable to add alice's htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(aliceHtlc); err != nil {
		t.Fatalf("unable to recv alice's htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete alice's state transition: %v", err)
	}

	// At this point, both sides should detect that they're fully synced.
	assertNoChanSyncNeeded(t, aliceChannel, bobChannel)
}

// TestChanSyncOweRevocationAndCommit tests that if Alice initiates a state
// transition with Bob and Bob sends both a RevokeAndAck and CommitSig message
// but Alice doesn't receive them before the connection dies, then he'll
// retransmit them both.
func TestChanSyncOweRevocationAndCommit(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	htlcAmt := lnwire.NewMAtomsFromAtoms(20000)

	// We'll kick off the test by having Bob send Alice an HTLC, then lock
	// it in with a state transition.
	var bobPreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xaa}, 32))
	rHash := sha256.Sum256(bobPreimage[:])
	bobHtlc := &lnwire.UpdateAddHTLC{
		PaymentHash: rHash,
		Amount:      htlcAmt,
		Expiry:      uint32(10),
	}
	bobHtlcIndex, err := bobChannel.AddHTLC(bobHtlc, nil)
	if err != nil {
		t.Fatalf("unable to add bob's htlc: %v", err)
	}
	aliceHtlcIndex, err := aliceChannel.ReceiveHTLC(bobHtlc)
	if err != nil {
		t.Fatalf("unable to recv bob's htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// Next, Alice will settle that incoming HTLC, then we'll start the
	// core of the test itself.
	err = aliceChannel.SettleHTLC(bobPreimage, aliceHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	err = bobChannel.ReceiveHTLCSettle(bobPreimage, bobHtlcIndex)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}

	// Progressing the exchange: Alice will send her signature, Bob will
	// receive, send a revocation and also a signature for Alice's state.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's commitment: %v", err)
	}

	// Bob generates the revoke and sig message, but the messages don't
	// reach Alice before the connection dies.
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke bob commitment: %v", err)
	}
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign commitment: %v", err)
	}

	// If we now attempt to resync, then Alice should conclude that she
	// doesn't need any further updates, while Bob concludes that he needs
	// to re-send both his revocation and commit sig message.
	aliceSyncMsg, err := aliceChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}

	aliceMsgsToSend, _, _, err := aliceChannel.ProcessChanSyncMsg(bobSyncMsg)
	if err != nil {
		t.Fatalf("unable to process chan sync msg: %v", err)
	}
	if len(aliceMsgsToSend) != 0 {
		t.Fatalf("expected alice to not retransmit, instead she's "+
			"sending: %v", spew.Sdump(aliceMsgsToSend))
	}

	assertBobSendsRevokeAndCommit := func() {
		bobMsgsToSend, _, _, err := bobChannel.ProcessChanSyncMsg(aliceSyncMsg)
		if err != nil {
			t.Fatalf("unable to process chan sync msg: %v", err)
		}
		if len(bobMsgsToSend) != 2 {
			t.Fatalf("expected bob to send %v messages, instead "+
				"sends: %v", 2, spew.Sdump(bobMsgsToSend))
		}
		bobReRevoke, ok := bobMsgsToSend[0].(*lnwire.RevokeAndAck)
		if !ok {
			t.Fatalf("expected bob to re-send revoke, instead sending: %v",
				spew.Sdump(bobMsgsToSend[0]))
		}
		if !reflect.DeepEqual(bobReRevoke, bobRevocation) {
			t.Fatalf("revocation msgs don't match: expected %v, got %v",
				bobRevocation, bobReRevoke)
		}

		bobReCommitSigMsg, ok := bobMsgsToSend[1].(*lnwire.CommitSig)
		if !ok {
			t.Fatalf("expected bob to re-send commit sig, instead sending: %v",
				spew.Sdump(bobMsgsToSend[1]))
		}
		if bobReCommitSigMsg.CommitSig != bobSig {
			t.Fatalf("commit sig msgs don't match: expected %x got %x",
				bobSig, bobReCommitSigMsg.CommitSig)
		}
		if len(bobReCommitSigMsg.HtlcSigs) != len(bobHtlcSigs) {
			t.Fatalf("wrong number of htlc sigs: expected %v, got %v",
				len(bobHtlcSigs), len(bobReCommitSigMsg.HtlcSigs))
		}
		for i, htlcSig := range bobReCommitSigMsg.HtlcSigs {
			if htlcSig != aliceHtlcSigs[i] {
				t.Fatalf("htlc sig msgs don't match: "+
					"expected %x got %x",
					bobHtlcSigs[i], htlcSig)
			}
		}
	}

	// We expect Bob to send exactly two messages: first his revocation
	// message to Alice, and second his original commit sig message.
	assertBobSendsRevokeAndCommit()

	// At this point we simulate the connection failing with a restart from
	// Bob. He should still re-send the exact same set of messages.
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}
	assertBobSendsRevokeAndCommit()

	// We'll now finish the state transition by having Alice process both
	// messages, and send her final revocation.
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to recv revocation: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to rev bob's commitment: %v", err)
	}
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to revoke commitment: %v", err)
	}
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to recv revocation: %v", err)
	}
}

// TestChanSyncOweRevocationAndCommitForceTransition tests that if Alice
// initiates a state transition with Bob, but Alice fails to receive his
// RevokeAndAck and the connection dies before Bob sends his CommitSig message,
// then Bob will re-send her RevokeAndAck message. Bob will also send and
// _identical_ CommitSig as he detects his commitment chain is ahead of
// Alice's.
func TestChanSyncOweRevocationAndCommitForceTransition(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	htlcAmt := lnwire.NewMAtomsFromAtoms(20000)

	// We'll kick off the test by having Bob send Alice an HTLC, then lock
	// it in with a state transition.
	var bobPreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xaa}, 32))
	rHash := sha256.Sum256(bobPreimage[:])
	var bobHtlc [2]*lnwire.UpdateAddHTLC
	bobHtlc[0] = &lnwire.UpdateAddHTLC{
		PaymentHash: rHash,
		Amount:      htlcAmt,
		Expiry:      uint32(10),
	}
	bobHtlcIndex, err := bobChannel.AddHTLC(bobHtlc[0], nil)
	if err != nil {
		t.Fatalf("unable to add bob's htlc: %v", err)
	}
	aliceHtlcIndex, err := aliceChannel.ReceiveHTLC(bobHtlc[0])
	if err != nil {
		t.Fatalf("unable to recv bob's htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// To ensure the channel sync logic handles the case where the two
	// commit chains are at different heights, we'll add another HTLC from
	// Bob to Alice, but let Alice skip the commitment for this state
	// update.
	rHash = sha256.Sum256(bytes.Repeat([]byte{0xbb}, 32))
	bobHtlc[1] = &lnwire.UpdateAddHTLC{
		PaymentHash: rHash,
		Amount:      htlcAmt,
		Expiry:      uint32(10),
		ID:          1,
	}
	_, err = bobChannel.AddHTLC(bobHtlc[1], nil)
	if err != nil {
		t.Fatalf("unable to add bob's htlc: %v", err)
	}
	_, err = aliceChannel.ReceiveHTLC(bobHtlc[1])
	if err != nil {
		t.Fatalf("unable to recv bob's htlc: %v", err)
	}

	// Bob signs the new state update, and sends the signature to Alice.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign commitment: %v", err)
	}

	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to rev bob's commitment: %v", err)
	}

	// Alice revokes her current state, but doesn't immediately send a
	// signature for Bob's updated state. Instead she will issue a new
	// update before sending a new CommitSig. This will lead to Alice's
	// local commit chain getting height > remote commit chain.
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to revoke commitment: %v", err)
	}
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to recv revocation: %v", err)
	}

	// Next, Alice will settle that incoming HTLC, then we'll start the
	// core of the test itself.
	err = aliceChannel.SettleHTLC(bobPreimage, aliceHtlcIndex, nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	err = bobChannel.ReceiveHTLCSettle(bobPreimage, bobHtlcIndex)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}

	// Progressing the exchange: Alice will send her signature, with Bob
	// processing the new state locally.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's commitment: %v", err)
	}

	// Bob then sends his revocation message, but before Alice can process
	// it (and before he scan send his CommitSig message), then connection
	// dies.
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke bob commitment: %v", err)
	}

	// Now if we attempt to synchronize states at this point, Alice should
	// detect that she owes nothing, while Bob should re-send both his
	// RevokeAndAck as well as his commitment message.
	aliceSyncMsg, err := aliceChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}

	aliceMsgsToSend, _, _, err := aliceChannel.ProcessChanSyncMsg(bobSyncMsg)
	if err != nil {
		t.Fatalf("unable to process chan sync msg: %v", err)
	}
	if len(aliceMsgsToSend) != 0 {
		t.Fatalf("expected alice to not retransmit, instead she's "+
			"sending: %v", spew.Sdump(aliceMsgsToSend))
	}

	// If we process Alice's sync message from Bob's PoV, then he should
	// send his RevokeAndAck message again. Additionally, the CommitSig
	// message that he sends should be sufficient to finalize the state
	// transition.
	bobMsgsToSend, _, _, err := bobChannel.ProcessChanSyncMsg(aliceSyncMsg)
	if err != nil {
		t.Fatalf("unable to process chan sync msg: %v", err)
	}
	if len(bobMsgsToSend) != 2 {
		t.Fatalf("expected bob to send %v messages, instead "+
			"sends: %v", 2, spew.Sdump(bobMsgsToSend))
	}
	bobReRevoke, ok := bobMsgsToSend[0].(*lnwire.RevokeAndAck)
	if !ok {
		t.Fatalf("expected bob to re-send revoke, instead sending: %v",
			spew.Sdump(bobMsgsToSend[0]))
	}
	if !reflect.DeepEqual(bobReRevoke, bobRevocation) {
		t.Fatalf("revocation msgs don't match: expected %v, got %v",
			bobRevocation, bobReRevoke)
	}

	// The second message should be his CommitSig message that he never
	// sent, but will send in order to force both states to synchronize.
	bobReCommitSigMsg, ok := bobMsgsToSend[1].(*lnwire.CommitSig)
	if !ok {
		t.Fatalf("expected bob to re-send commit sig, instead sending: %v",
			spew.Sdump(bobMsgsToSend[1]))
	}

	// At this point we simulate the connection failing with a restart from
	// Bob. He should still re-send the exact same set of messages.
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}
	if len(bobMsgsToSend) != 2 {
		t.Fatalf("expected bob to send %v messages, instead "+
			"sends: %v", 2, spew.Sdump(bobMsgsToSend))
	}
	bobReRevoke, ok = bobMsgsToSend[0].(*lnwire.RevokeAndAck)
	if !ok {
		t.Fatalf("expected bob to re-send revoke, instead sending: %v",
			spew.Sdump(bobMsgsToSend[0]))
	}
	bobSigMsg, ok := bobMsgsToSend[1].(*lnwire.CommitSig)
	if !ok {
		t.Fatalf("expected bob to re-send commit sig, instead sending: %v",
			spew.Sdump(bobMsgsToSend[1]))
	}
	if !reflect.DeepEqual(bobReRevoke, bobRevocation) {
		t.Fatalf("revocation msgs don't match: expected %v, got %v",
			bobRevocation, bobReRevoke)
	}
	if bobReCommitSigMsg.CommitSig != bobSigMsg.CommitSig {
		t.Fatalf("commit sig msgs don't match: expected %x got %x",
			bobSigMsg.CommitSig,
			bobReCommitSigMsg.CommitSig)
	}
	if len(bobReCommitSigMsg.HtlcSigs) != len(bobSigMsg.HtlcSigs) {
		t.Fatalf("wrong number of htlc sigs: expected %v, got %v",
			len(bobSigMsg.HtlcSigs), len(bobReCommitSigMsg.HtlcSigs))
	}
	for i, htlcSig := range bobReCommitSigMsg.HtlcSigs {
		if htlcSig != bobSigMsg.HtlcSigs[i] {
			t.Fatalf("htlc sig msgs don't match: "+
				"expected %x got %x",
				bobSigMsg.HtlcSigs[i], htlcSig)
		}
	}

	// Now, we'll continue the exchange, sending Bob's revocation and
	// signature message to Alice, ending with Alice sending her revocation
	// message to Bob.
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to recv revocation: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(
		bobSigMsg.CommitSig, bobSigMsg.HtlcSigs,
	)
	if err != nil {
		t.Fatalf("alice unable to rev bob's commitment: %v", err)
	}
	aliceRevocation, _, err = aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to revoke commitment: %v", err)
	}
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to recv revocation: %v", err)
	}
}

// TestChanSyncFailure tests the various scenarios during channel sync where we
// should be able to detect that the channels cannot be synced because of
// invalid state.
func TestChanSyncFailure(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(false)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	htlcAmt := lnwire.NewMAtomsFromAtoms(20000)
	index := byte(0)

	// advanceState is a helper method to fully advance the channel state
	// by one.
	advanceState := func() {
		t.Helper()

		// We'll kick off the test by having Bob send Alice an HTLC,
		// then lock it in with a state transition.
		var bobPreimage [32]byte
		copy(bobPreimage[:], bytes.Repeat([]byte{0xaa - index}, 32))
		rHash := sha256.Sum256(bobPreimage[:])
		bobHtlc := &lnwire.UpdateAddHTLC{
			PaymentHash: rHash,
			Amount:      htlcAmt,
			Expiry:      uint32(10),
			ID:          uint64(index),
		}
		index++

		_, err := bobChannel.AddHTLC(bobHtlc, nil)
		if err != nil {
			t.Fatalf("unable to add bob's htlc: %v", err)
		}
		_, err = aliceChannel.ReceiveHTLC(bobHtlc)
		if err != nil {
			t.Fatalf("unable to recv bob's htlc: %v", err)
		}
		err = ForceStateTransition(bobChannel, aliceChannel)
		if err != nil {
			t.Fatalf("unable to complete bob's state "+
				"transition: %v", err)
		}
	}

	// halfAdvance is a helper method that sends a new commitment signature
	// from Alice to Bob, but doesn't make Bob revoke his current state.
	halfAdvance := func() {
		t.Helper()

		// We'll kick off the test by having Bob send Alice an HTLC,
		// then lock it in with a state transition.
		var bobPreimage [32]byte
		copy(bobPreimage[:], bytes.Repeat([]byte{0xaa - index}, 32))
		rHash := sha256.Sum256(bobPreimage[:])
		bobHtlc := &lnwire.UpdateAddHTLC{
			PaymentHash: rHash,
			Amount:      htlcAmt,
			Expiry:      uint32(10),
			ID:          uint64(index),
		}
		index++

		_, err := bobChannel.AddHTLC(bobHtlc, nil)
		if err != nil {
			t.Fatalf("unable to add bob's htlc: %v", err)
		}
		_, err = aliceChannel.ReceiveHTLC(bobHtlc)
		if err != nil {
			t.Fatalf("unable to recv bob's htlc: %v", err)
		}

		aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
		if err != nil {
			t.Fatalf("unable to sign next commit: %v", err)
		}
		err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
		if err != nil {
			t.Fatalf("unable to receive commit sig: %v", err)
		}
	}

	// assertLocalDataLoss checks that aliceOld and bobChannel detects that
	// Alice has lost state during sync.
	assertLocalDataLoss := func(aliceOld *LightningChannel) {
		t.Helper()

		aliceSyncMsg, err := aliceOld.channelState.ChanSyncMsg()
		if err != nil {
			t.Fatalf("unable to produce chan sync msg: %v", err)
		}
		bobSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
		if err != nil {
			t.Fatalf("unable to produce chan sync msg: %v", err)
		}

		// Alice should detect from Bob's message that she lost state.
		_, _, _, err = aliceOld.ProcessChanSyncMsg(bobSyncMsg)
		if _, ok := err.(*ErrCommitSyncLocalDataLoss); !ok {
			t.Fatalf("wrong error, expected "+
				"ErrCommitSyncLocalDataLoss instead got: %v",
				err)
		}

		// Bob should detect that Alice probably lost state.
		_, _, _, err = bobChannel.ProcessChanSyncMsg(aliceSyncMsg)
		if err != ErrCommitSyncRemoteDataLoss {
			t.Fatalf("wrong error, expected "+
				"ErrCommitSyncRemoteDataLoss instead got: %v",
				err)
		}
	}

	// clearBorkedState is a method that allows us to clear the borked
	// state that will arise after the first chan message sync. We need to
	// do this in order to be able to continue to update the commitment
	// state for our test scenarios.
	clearBorkedState := func() {
		err = aliceChannel.channelState.ClearChanStatus(
			channeldb.ChanStatusLocalDataLoss | channeldb.ChanStatusBorked,
		)
		if err != nil {
			t.Fatalf("unable to update channel state: %v", err)
		}
		err = bobChannel.channelState.ClearChanStatus(
			channeldb.ChanStatusLocalDataLoss | channeldb.ChanStatusBorked,
		)
		if err != nil {
			t.Fatalf("unable to update channel state: %v", err)
		}
	}

	// Start by advancing the state.
	advanceState()

	// They should be in sync.
	assertNoChanSyncNeeded(t, aliceChannel, bobChannel)

	// Make a copy of Alice's state from the database at this point.
	aliceOld, err := restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}

	// Advance the states.
	advanceState()

	// Trying to sync up the old version of Alice's channel should detect
	// that we are out of sync.
	assertLocalDataLoss(aliceOld)

	// Make sure the up-to-date channels still are in sync.
	assertNoChanSyncNeeded(t, aliceChannel, bobChannel)

	// Clear the borked state before we attempt to advance.
	clearBorkedState()

	// Advance the state again, and do the same check.
	advanceState()
	assertNoChanSyncNeeded(t, aliceChannel, bobChannel)
	assertLocalDataLoss(aliceOld)

	// If we remove the recovery options from Bob's message, Alice cannot
	// tell if she lost state, since Bob might be lying. She still should
	// be able to detect that chains cannot be synced.
	bobSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg.LocalUnrevokedCommitPoint = nil
	_, _, _, err = aliceOld.ProcessChanSyncMsg(bobSyncMsg)
	if err != ErrCannotSyncCommitChains {
		t.Fatalf("wrong error, expected ErrCannotSyncCommitChains "+
			"instead got: %v", err)
	}

	// If Bob lies about the NextLocalCommitHeight, making it greater than
	// what Alice expect, she cannot tell for sure whether she lost state,
	// but should detect the desync.
	bobSyncMsg, err = bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg.NextLocalCommitHeight++
	_, _, _, err = aliceChannel.ProcessChanSyncMsg(bobSyncMsg)
	if err != ErrCannotSyncCommitChains {
		t.Fatalf("wrong error, expected ErrCannotSyncCommitChains "+
			"instead got: %v", err)
	}

	// If Bob's NextLocalCommitHeight is lower than what Alice expects, Bob
	// probably lost state.
	bobSyncMsg, err = bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg.NextLocalCommitHeight--
	_, _, _, err = aliceChannel.ProcessChanSyncMsg(bobSyncMsg)
	if err != ErrCommitSyncRemoteDataLoss {
		t.Fatalf("wrong error, expected ErrCommitSyncRemoteDataLoss "+
			"instead got: %v", err)
	}

	// If Alice and Bob's states are in sync, but Bob is sending the wrong
	// LocalUnrevokedCommitPoint, Alice should detect this.
	bobSyncMsg, err = bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	p := bobSyncMsg.LocalUnrevokedCommitPoint.SerializeCompressed()
	p[4] ^= 0x01
	modCommitPoint, err := secp256k1.ParsePubKey(p)
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}

	bobSyncMsg.LocalUnrevokedCommitPoint = modCommitPoint
	_, _, _, err = aliceChannel.ProcessChanSyncMsg(bobSyncMsg)
	if err != ErrInvalidLocalUnrevokedCommitPoint {
		t.Fatalf("wrong error, expected "+
			"ErrInvalidLocalUnrevokedCommitPoint instead got: %v",
			err)
	}

	// Make sure the up-to-date channels still are good.
	assertNoChanSyncNeeded(t, aliceChannel, bobChannel)

	// Clear the borked state before we attempt to advance.
	clearBorkedState()

	// Finally check that Alice is also able to detect a wrong commit point
	// when there's a pending remote commit.
	halfAdvance()

	bobSyncMsg, err = bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg.LocalUnrevokedCommitPoint = modCommitPoint
	_, _, _, err = aliceChannel.ProcessChanSyncMsg(bobSyncMsg)
	if err != ErrInvalidLocalUnrevokedCommitPoint {
		t.Fatalf("wrong error, expected "+
			"ErrInvalidLocalUnrevokedCommitPoint instead got: %v",
			err)
	}
}

// TestFeeUpdateRejectInsaneFee tests that if the initiator tries to attach a
// fee that would put them below their current reserve, then it's rejected by
// the state machine.
func TestFeeUpdateRejectInsaneFee(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, _, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Next, we'll try to add a fee rate to Alice which is 1,000,000x her
	// starting fee rate.
	startingFeeRate := AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB)
	newFeeRate := startingFeeRate * 1000000

	// Both Alice and Bob should reject this new fee rate as it is far too
	// large.
	if err := aliceChannel.UpdateFee(newFeeRate); err == nil {
		t.Fatalf("alice should have rejected fee update")
	}
}

// TestChannelRetransmissionFeeUpdate tests that the initiator will include any
// pending fee updates if it needs to retransmit signatures.
func TestChannelRetransmissionFeeUpdate(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First, we'll fetch the current fee rate present within the
	// commitment transactions.
	startingFeeRate := AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB)

	// Next, we'll start a commitment update, with Alice sending a new
	// update to double the fee rate of the commitment.
	newFeeRate := startingFeeRate * 2
	if err := aliceChannel.UpdateFee(newFeeRate); err != nil {
		t.Fatalf("unable to update fee for Alice's channel: %v", err)
	}
	if err := bobChannel.ReceiveUpdateFee(newFeeRate); err != nil {
		t.Fatalf("unable to update fee for Bob's channel: %v", err)
	}

	// Now, Alice will send a new commitment to Bob, but we'll simulate a
	// connection failure, so Bob doesn't get her signature.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// Restart both channels to simulate a connection restart.
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}

	// Bob doesn't get this message so upon reconnection, they need to
	// synchronize. Alice should conclude that she owes Bob a commitment,
	// while Bob should think he's properly synchronized.
	aliceSyncMsg, err := aliceChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}
	bobSyncMsg, err := bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to produce chan sync msg: %v", err)
	}

	// Bob should detect that he doesn't need to send anything to Alice.
	bobMsgsToSend, _, _, err := bobChannel.ProcessChanSyncMsg(aliceSyncMsg)
	if err != nil {
		t.Fatalf("unable to process chan sync msg: %v", err)
	}
	if len(bobMsgsToSend) != 0 {
		t.Fatalf("expected bob to send %v messages instead "+
			"will send %v: %v", 0, len(bobMsgsToSend),
			spew.Sdump(bobMsgsToSend))
	}

	// When Alice processes Bob's chan sync message, she should realize
	// that she needs to first send a new UpdateFee message, and also a
	// CommitSig.
	aliceMsgsToSend, _, _, err := aliceChannel.ProcessChanSyncMsg(
		bobSyncMsg,
	)
	if err != nil {
		t.Fatalf("unable to process chan sync msg: %v", err)
	}
	if len(aliceMsgsToSend) != 2 {
		t.Fatalf("expected alice to send %v messages instead "+
			"will send %v: %v", 2, len(aliceMsgsToSend),
			spew.Sdump(aliceMsgsToSend))
	}

	// The first message should be an UpdateFee message.
	retransFeeMsg, ok := aliceMsgsToSend[0].(*lnwire.UpdateFee)
	if !ok {
		t.Fatalf("expected UpdateFee message, instead have: %v",
			spew.Sdump(aliceMsgsToSend[0]))
	}

	// The fee should match exactly the new fee update we applied above.
	if retransFeeMsg.FeePerKB != uint32(newFeeRate) {
		t.Fatalf("fee update doesn't match: expected %v, got %v",
			uint32(newFeeRate), retransFeeMsg)
	}

	// The second, should be a CommitSig message, and be identical to the
	// sig message she sent prior.
	commitSigMsg, ok := aliceMsgsToSend[1].(*lnwire.CommitSig)
	if !ok {
		t.Fatalf("expected a CommitSig message, instead have %v",
			spew.Sdump(aliceMsgsToSend[1]))
	}
	if commitSigMsg.CommitSig != aliceSig {
		t.Fatalf("commit sig msgs don't match: expected %x got %x",
			aliceSig, commitSigMsg.CommitSig)
	}
	if len(commitSigMsg.HtlcSigs) != len(aliceHtlcSigs) {
		t.Fatalf("wrong number of htlc sigs: expected %v, got %v",
			len(aliceHtlcSigs), len(commitSigMsg.HtlcSigs))
	}
	for i, htlcSig := range commitSigMsg.HtlcSigs {
		if htlcSig != aliceHtlcSigs[i] {
			t.Fatalf("htlc sig msgs don't match: "+
				"expected %x got %x",
				aliceHtlcSigs[i], htlcSig)
		}
	}

	// Now, we if re-apply the updates to Bob, we should be able to resume
	// the commitment update as normal.
	if err := bobChannel.ReceiveUpdateFee(newFeeRate); err != nil {
		t.Fatalf("unable to update fee for Bob's channel: %v", err)
	}

	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's commitment: %v", err)
	}
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke bob commitment: %v", err)
	}
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign commitment: %v", err)
	}
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to recv revocation: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to rev bob's commitment: %v", err)
	}
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to revoke commitment: %v", err)
	}
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to recv revocation: %v", err)
	}

	// Both parties should now have the latest fee rate locked-in.
	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) != newFeeRate {
		t.Fatalf("alice's FeePerKB was not locked in")
	}
	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) != newFeeRate {
		t.Fatalf("bob's FeePerKB was not locked in")
	}

	// Finally, we'll add with adding a new HTLC, then forcing a state
	// transition. This should also proceed as normal.
	var bobPreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xaa}, 32))
	rHash := sha256.Sum256(bobPreimage[:])
	bobHtlc := &lnwire.UpdateAddHTLC{
		PaymentHash: rHash,
		Amount:      lnwire.NewMAtomsFromAtoms(20000),
		Expiry:      uint32(10),
	}
	if _, err := bobChannel.AddHTLC(bobHtlc, nil); err != nil {
		t.Fatalf("unable to add bob's htlc: %v", err)
	}
	if _, err := aliceChannel.ReceiveHTLC(bobHtlc); err != nil {
		t.Fatalf("unable to recv bob's htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}
}

// TestFeeUpdateOldDiskFormat tests that we properly recover FeeUpdates written
// to disk using the old format, where the logIndex was not written.
func TestFeeUpdateOldDiskFormat(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 BTC,
	// and Bob having 5 BTC.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// helper that counts the number of updates, and number of fee updates
	// in the given log.
	countLog := func(log *updateLog) (int, int) {
		var numUpdates, numFee int
		for e := log.Front(); e != nil; e = e.Next() {
			htlc := e.Value.(*PaymentDescriptor)
			if htlc.EntryType == FeeUpdate {
				numFee++
			}
			numUpdates++
		}
		return numUpdates, numFee
	}

	// helper that asserts that Alice's local log and Bob's remote log
	// contains the expected number of fee updates and adds.
	assertLogItems := func(expFee, expAdd int) {
		t.Helper()

		expUpd := expFee + expAdd
		upd, fees := countLog(aliceChannel.localUpdateLog)
		if upd != expUpd {
			t.Fatalf("expected %d updates, found %d in Alice's "+
				"log", expUpd, upd)
		}
		if fees != expFee {
			t.Fatalf("expected %d fee updates, found %d in "+
				"Alice's log", expFee, fees)
		}
		upd, fees = countLog(bobChannel.remoteUpdateLog)
		if upd != expUpd {
			t.Fatalf("expected %d updates, found %d in Bob's log",
				expUpd, upd)
		}
		if fees != expFee {
			t.Fatalf("expected %d fee updates, found %d in Bob's "+
				"log", expFee, fees)
		}
	}

	// First, we'll fetch the current fee rate present within the
	// commitment transactions.
	startingFeeRate := AtomPerKByte(
		aliceChannel.channelState.LocalCommitment.FeePerKB,
	)
	newFeeRate := startingFeeRate

	// We will send a few HTLCs and a fee update.
	htlcAmt := lnwire.NewMAtomsFromAtoms(0.1 * dcrutil.AtomsPerCoin)
	const numHTLCs = 30
	var htlcs []*lnwire.UpdateAddHTLC
	for i := 0; i < numHTLCs; i++ {
		htlc, _ := createHTLC(i, htlcAmt)
		if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
			t.Fatalf("unable to add htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("unable to recv htlc: %v", err)
		}
		htlcs = append(htlcs, htlc)

		if i%5 != 0 {
			continue
		}

		// After every 5th HTLC, we'll also include a fee update.
		newFeeRate += startingFeeRate
		if err := aliceChannel.UpdateFee(newFeeRate); err != nil {
			t.Fatalf("unable to update fee for Alice's channel: %v",
				err)
		}
		if err := bobChannel.ReceiveUpdateFee(newFeeRate); err != nil {
			t.Fatalf("unable to update fee for Bob's channel: %v",
				err)
		}
	}
	// Check that the expected number of items is found in the logs.
	expFee := numHTLCs / 5
	assertLogItems(expFee, numHTLCs)

	// Now, Alice will send a new commitment to Bob, but we'll simulate a
	// connection failure, so Bob doesn't get the signature.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// Before restarting Alice, to mimic the old format, we fetch the
	// pending remote commit from disk, set the UpdateFee message's
	// logIndex to 0, and re-write it.
	pendingRemoteCommitDiff, err := aliceChannel.channelState.RemoteCommitChainTip()
	if err != nil {
		t.Fatal(err)
	}
	for i, u := range pendingRemoteCommitDiff.LogUpdates {
		switch u.UpdateMsg.(type) {
		case *lnwire.UpdateFee:
			pendingRemoteCommitDiff.LogUpdates[i].LogIndex = 0
		}
	}
	err = aliceChannel.channelState.AppendRemoteCommitChain(
		pendingRemoteCommitDiff,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Restart both channels to simulate a connection restart. This will
	// trigger a update logs restoration.
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}

	// After a reconnection, Alice will resend the pending updates, that
	// was not ACKed by Bob, so we re-send the HTLCs and fee updates.
	newFeeRate = startingFeeRate
	for i := 0; i < numHTLCs; i++ {
		htlc := htlcs[i]
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("unable to recv htlc: %v", err)
		}

		if i%5 != 0 {
			continue
		}

		newFeeRate += startingFeeRate
		if err := bobChannel.ReceiveUpdateFee(newFeeRate); err != nil {
			t.Fatalf("unable to update fee for Bob's channel: %v",
				err)
		}
	}
	assertLogItems(expFee, numHTLCs)

	// We send Alice's commitment signatures, and finish the state
	// transition.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("bob unable to process alice's commitment: %v", err)
	}
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke bob commitment: %v", err)
	}
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("bob unable to sign commitment: %v", err)
	}
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("alice unable to recv revocation: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("alice unable to rev bob's commitment: %v", err)
	}
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("alice unable to revoke commitment: %v", err)
	}
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to recv revocation: %v", err)
	}

	// Both parties should now have the latest fee rate locked-in.
	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) != newFeeRate {
		t.Fatalf("alice's feePerKB was not locked in")
	}
	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) != newFeeRate {
		t.Fatalf("bob's feePerKB was not locked in")
	}

	// Finally, to trigger a compactLogs execution, we'll add a new HTLC,
	// then force a state transition.
	htlc, _ := createHTLC(numHTLCs, htlcAmt)
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// Finally, check the logs to make sure all fee updates have been
	// removed...
	assertLogItems(0, numHTLCs+1)

	// ...and the final fee rate locked in.
	if AtomPerKByte(aliceChannel.channelState.LocalCommitment.FeePerKB) != newFeeRate {
		t.Fatalf("alice's feePerKB was not locked in")
	}
	if AtomPerKByte(bobChannel.channelState.LocalCommitment.FeePerKB) != newFeeRate {
		t.Fatalf("bob's feePerKB was not locked in")
	}
}

// TestChanSyncUnableToSync tests that if Alice or Bob receive an invalid
// ChannelReestablish messages,then they reject the message and declare the
// channel un-continuable by returning ErrCannotSyncCommitChains.
func TestChanSyncUnableToSync(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// If we immediately send both sides a "bogus" ChanSync message, then
	// they both should conclude that they're unable to synchronize the
	// state.
	badChanSync := &lnwire.ChannelReestablish{
		ChanID: lnwire.NewChanIDFromOutPoint(
			&aliceChannel.channelState.FundingOutpoint,
		),
		NextLocalCommitHeight:  1000,
		RemoteCommitTailHeight: 9000,
	}
	_, _, _, err = bobChannel.ProcessChanSyncMsg(badChanSync)
	if err != ErrCannotSyncCommitChains {
		t.Fatalf("expected error instead have: %v", err)
	}
	_, _, _, err = aliceChannel.ProcessChanSyncMsg(badChanSync)
	if err != ErrCannotSyncCommitChains {
		t.Fatalf("expected error instead have: %v", err)
	}
}

// TestChanSyncInvalidLastSecret ensures that if Alice and Bob have completed
// state transitions in an existing channel, and then send a ChannelReestablish
// message after a restart, the following holds: if Alice has lost data, so she
// sends an invalid commit secret then both parties recognize this as possible
// data loss.
func TestChanSyncInvalidLastSecret(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We'll create a new instances of Alice before doing any state updates
	// such that we have the initial in memory state at the start of the
	// channel.
	aliceOld, err := restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart alice")
	}

	// First, we'll add an HTLC, and then initiate a state transition
	// between the two parties such that we actually have a prior
	// revocation to send.
	var paymentPreimage [32]byte
	copy(paymentPreimage[:], bytes.Repeat([]byte{1}, 32))
	paymentHash := sha256.Sum256(paymentPreimage[:])
	htlcAmt := lnwire.NewMAtomsFromAtoms(dcrutil.AtomsPerCoin)
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: paymentHash,
		Amount:      htlcAmt,
		Expiry:      uint32(5),
	}
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Then we'll initiate a state transition to lock in this new HTLC.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete alice's state transition: %v", err)
	}

	// Next, we'll restart both parties in order to simulate a connection
	// re-establishment.
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart bob: %v", err)
	}

	// Next, we'll produce the ChanSync messages for both parties.
	aliceChanSync, err := aliceChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to generate chan sync msg: %v", err)
	}
	bobChanSync, err := bobChannel.channelState.ChanSyncMsg()
	if err != nil {
		t.Fatalf("unable to generate chan sync msg: %v", err)
	}

	// We'll modify Alice's sync message to have an invalid commitment
	// secret.
	aliceChanSync.LastRemoteCommitSecret[4] ^= 0x01

	// Alice's former self should conclude that she possibly lost data as
	// Bob is sending a valid commit secret for the latest state.
	_, _, _, err = aliceOld.ProcessChanSyncMsg(bobChanSync)
	if _, ok := err.(*ErrCommitSyncLocalDataLoss); !ok {
		t.Fatalf("wrong error, expected ErrCommitSyncLocalDataLoss "+
			"instead got: %v", err)
	}

	// Bob should conclude that he should force close the channel, as Alice
	// cannot continue operation.
	_, _, _, err = bobChannel.ProcessChanSyncMsg(aliceChanSync)
	if err != ErrInvalidLastCommitSecret {
		t.Fatalf("wrong error, expected ErrInvalidLastCommitSecret, "+
			"instead got: %v", err)
	}
}

// TestChanAvailableBandwidth tests the accuracy of the AvailableBalance()
// method. The value returned from this message should reflect the value
// returned within the commitment state of a channel after the transition is
// initiated.
func TestChanAvailableBandwidth(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	assertBandwidthEstimateCorrect := func(aliceInitiate bool) {
		// With the HTLC's added, we'll now query the AvailableBalance
		// method for the current available channel bandwidth from
		// Alice's PoV.
		aliceAvailableBalance := aliceChannel.AvailableBalance()

		// With this balance obtained, we'll now trigger a state update
		// to actually determine what the current up to date balance
		// is.
		if aliceInitiate {
			err := ForceStateTransition(aliceChannel, bobChannel)
			if err != nil {
				t.Fatalf("unable to complete alice's state "+
					"transition: %v", err)
			}
		} else {
			err := ForceStateTransition(bobChannel, aliceChannel)
			if err != nil {
				t.Fatalf("unable to complete alice's state "+
					"transition: %v", err)
			}
		}

		// Now, we'll obtain the current available bandwidth in Alice's
		// latest commitment and compare that to the prior estimate.
		aliceBalance := aliceChannel.channelState.LocalCommitment.LocalBalance
		if aliceBalance != aliceAvailableBalance {
			_, _, line, _ := runtime.Caller(1)
			t.Fatalf("line: %v, incorrect balance: expected %v, "+
				"got %v", line, aliceBalance,
				aliceAvailableBalance)
		}
	}

	// First, we'll add 3 outgoing HTLC's from Alice to Bob.
	const numHtlcs = 3
	var htlcAmt lnwire.MilliAtom = 100000
	alicePreimages := make([][32]byte, numHtlcs)
	for i := 0; i < numHtlcs; i++ {
		htlc, preImage := createHTLC(i, htlcAmt)
		if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
			t.Fatalf("unable to add htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("unable to recv htlc: %v", err)
		}

		alicePreimages[i] = preImage
	}

	assertBandwidthEstimateCorrect(true)

	// We'll repeat the same exercise, but with non-dust HTLCs. So we'll
	// crank up the value of the HTLC's we're adding to the commitment
	// transaction.
	htlcAmt = lnwire.NewMAtomsFromAtoms(30000)
	for i := 0; i < numHtlcs; i++ {
		htlc, preImage := createHTLC(numHtlcs+i, htlcAmt)
		if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
			t.Fatalf("unable to add htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("unable to recv htlc: %v", err)
		}

		alicePreimages = append(alicePreimages, preImage)
	}

	assertBandwidthEstimateCorrect(true)

	// Next, we'll have Bob 5 of Alice's HTLC's, and cancel one of them (in
	// the update log).
	for i := 0; i < (numHtlcs*2)-1; i++ {
		preImage := alicePreimages[i]
		err := bobChannel.SettleHTLC(preImage, uint64(i), nil, nil, nil)
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}
		err = aliceChannel.ReceiveHTLCSettle(preImage, uint64(i))
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}
	}

	htlcIndex := uint64((numHtlcs * 2) - 1)
	err = bobChannel.FailHTLC(htlcIndex, []byte("f"), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	err = aliceChannel.ReceiveFailHTLC(htlcIndex, []byte("bad"))
	if err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// We must do a state transition before the balance is available
	// for Alice.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete alice's state "+
			"transition: %v", err)
	}

	// With the HTLC's settled in the log, we'll now assert that if we
	// initiate a state transition, then our guess was correct.
	assertBandwidthEstimateCorrect(false)

	// TODO(roasbeef): additional tests from diff starting conditions
}

// TestSignCommitmentFailNotLockedIn tests that a channel will not attempt to
// create a new state if it doesn't yet know of the next revocation point for
// the remote party.
func TestSignCommitmentFailNotLockedIn(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, _, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Next, we'll modify Alice's internal state to omit knowledge of Bob's
	// next revocation point.
	aliceChannel.channelState.RemoteNextRevocation = nil

	// If we now try to initiate a state update, then it should fail as
	// Alice is unable to actually create a new state.
	_, _, _, err = aliceChannel.SignNextCommitment()
	if err != ErrNoWindow {
		t.Fatalf("expected ErrNoWindow, instead have: %v", err)
	}
}

// TestLockedInHtlcForwardingSkipAfterRestart ensures that after a restart, a
// state machine doesn't attempt to re-forward any HTLC's that were already
// locked in, but in a prior state.
func TestLockedInHtlcForwardingSkipAfterRestart(t *testing.T) {
	t.Parallel()

	// First, we'll make a channel between Alice and Bob.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We'll now add two HTLC's from Alice to Bob, then Alice will initiate
	// a state transition.
	var htlcAmt lnwire.MilliAtom = 100000
	htlc, _ := createHTLC(0, htlcAmt)
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}
	htlc2, _ := createHTLC(1, htlcAmt)
	if _, err := aliceChannel.AddHTLC(htlc2, nil); err != nil {
		t.Fatalf("unable to add htlc2: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc2); err != nil {
		t.Fatalf("unable to recv htlc2: %v", err)
	}

	// We'll now manually initiate a state transition between Alice and
	// bob.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatal(err)
	}
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// Alice should detect that she doesn't need to forward any HTLC's.
	fwdPkg, _, _, _, err := aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatal(err)
	}
	if len(fwdPkg.Adds) != 0 {
		t.Fatalf("alice shouldn't forward any HTLC's, instead wants to "+
			"forward %v htlcs", len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("alice shouldn't forward any HTLC's, instead wants to "+
			"forward %v htlcs", len(fwdPkg.SettleFails))
	}

	// Now, have Bob initiate a transition to lock in the Adds sent by
	// Alice.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}

	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatal(err)
	}
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// Bob should now detect that he now has 2 incoming HTLC's that he can
	// forward along.
	fwdPkg, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatal(err)
	}
	if len(fwdPkg.Adds) != 2 {
		t.Fatalf("bob should forward 2 hltcs, instead has %v",
			len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("bob should forward 0 hltcs, instead has %v",
			len(fwdPkg.SettleFails))
	}

	// We'll now restart both Alice and Bob. This emulates a reconnection
	// between the two peers.
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart bob: %v", err)
	}

	// With both nodes restarted, Bob will now attempt to cancel one of
	// Alice's HTLC's.
	err = bobChannel.FailHTLC(htlc.ID, []byte("failreason"), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	err = aliceChannel.ReceiveFailHTLC(htlc.ID, []byte("bad"))
	if err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// We'll now initiate another state transition, but this time Bob will
	// lead.
	bobSig, bobHtlcSigs, _, err = bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatal(err)
	}
	aliceRevocation, _, err = aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// At this point, Bob receives the revocation from Alice, which is now
	// his signal to examine all the HTLC's that have been locked in to
	// process.
	fwdPkg, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatal(err)
	}

	// Bob should detect that he doesn't need to forward *any* HTLC's, as
	// he was the one that initiated extending the commitment chain of
	// Alice.
	if len(fwdPkg.Adds) != 0 {
		t.Fatalf("alice shouldn't forward any HTLC's, instead wants to "+
			"forward %v htlcs", len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("alice shouldn't forward any HTLC's, instead wants to "+
			"forward %v htlcs", len(fwdPkg.SettleFails))
	}

	// Now, begin another state transition led by Alice, and fail the second
	// HTLC part-way through the dance.
	aliceSig, aliceHtlcSigs, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatal(err)
	}

	// Failing the HTLC here will cause the update to be included in Alice's
	// remote log, but it should not be committed by this transition.
	err = bobChannel.FailHTLC(htlc2.ID, []byte("failreason"), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	err = aliceChannel.ReceiveFailHTLC(htlc2.ID, []byte("bad"))
	if err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	bobRevocation, _, err = bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// Alice should detect that she doesn't need to forward any Adds's, but
	// that the Fail has been locked in an can be forwarded.
	_, adds, settleFails, _, err := aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatal(err)
	}
	if len(adds) != 0 {
		t.Fatalf("alice shouldn't forward any HTLC's, instead wants to "+
			"forward %v htlcs", len(adds))
	}
	if len(settleFails) != 1 {
		t.Fatalf("alice should only forward %d HTLC's, instead wants to "+
			"forward %v htlcs", 1, len(settleFails))
	}
	if settleFails[0].ParentIndex != htlc.ID {
		t.Fatalf("alice should forward fail for htlcid=%d, instead "+
			"forwarding id=%d", htlc.ID,
			settleFails[0].ParentIndex)
	}

	// We'll now restart both Alice and Bob. This emulates a reconnection
	// between the two peers.
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart bob: %v", err)
	}

	// Readd the Fail to both Alice and Bob's channels, as the non-committed
	// update will not have survived the restart.
	err = bobChannel.FailHTLC(htlc2.ID, []byte("failreason"), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	err = aliceChannel.ReceiveFailHTLC(htlc2.ID, []byte("bad"))
	if err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// Have Alice initiate a state transition, which does not include the
	// HTLCs just readded to the channel state.
	aliceSig, aliceHtlcSigs, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatal(err)
	}
	bobRevocation, _, err = bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// Alice should detect that she doesn't need to forward any HTLC's, as
	// the updates haven't been committed by Bob yet.
	fwdPkg, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatal(err)
	}
	if len(fwdPkg.Adds) != 0 {
		t.Fatalf("alice shouldn't forward any HTLC's, instead wants to "+
			"forward %v htlcs", len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("alice shouldn't forward any HTLC's, instead wants to "+
			"forward %v htlcs", len(fwdPkg.SettleFails))
	}

	// Now initiate a final update from Bob to lock in the final Fail.
	bobSig, bobHtlcSigs, _, err = bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}

	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatal(err)
	}

	aliceRevocation, _, err = aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// Bob should detect that he has nothing to forward, as he hasn't
	// received any HTLCs.
	fwdPkg, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatal(err)
	}
	if len(fwdPkg.Adds) != 0 {
		t.Fatalf("bob should forward 4 hltcs, instead has %v",
			len(fwdPkg.Adds))
	}
	if len(fwdPkg.SettleFails) != 0 {
		t.Fatalf("bob should forward 0 hltcs, instead has %v",
			len(fwdPkg.SettleFails))
	}

	// Finally, have Bob initiate a state transition that locks in the Fail
	// added after the restart.
	aliceSig, aliceHtlcSigs, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatal(err)
	}
	bobRevocation, _, err = bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// When Alice receives the revocation, she should detect that she
	// can now forward the freshly locked-in Fail.
	_, adds, settleFails, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatal(err)
	}
	if len(adds) != 0 {
		t.Fatalf("alice shouldn't forward any HTLC's, instead wants to "+
			"forward %v htlcs", len(adds))
	}
	if len(settleFails) != 1 {
		t.Fatalf("alice should only forward one HTLC, instead wants to "+
			"forward %v htlcs", len(settleFails))
	}
	if settleFails[0].ParentIndex != htlc2.ID {
		t.Fatalf("alice should forward fail for htlcid=%d, instead "+
			"forwarding id=%d", htlc2.ID,
			settleFails[0].ParentIndex)
	}
}

// TestInvalidCommitSigError tests that if the remote party sends us an invalid
// commitment signature, then we'll reject it and return a special error that
// contains information to allow the remote party to debug their issues.
func TestInvalidCommitSigError(t *testing.T) {
	t.Parallel()

	// First, we'll make a channel between Alice and Bob.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// With the channel established, we'll now send a single HTLC from
	// Alice to Bob.
	var htlcAmt lnwire.MilliAtom = 100000
	htlc, _ := createHTLC(0, htlcAmt)
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Alice will now attempt to initiate a state transition.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign new commit: %v", err)
	}

	// Before the signature gets to Bob, we'll mutate it, such that the
	// signature is now actually invalid.
	aliceSig[0] ^= 88

	// Bob should reject this new state, and return the proper error.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err == nil {
		t.Fatalf("bob accepted invalid state but shouldn't have")
	}
	if _, ok := err.(*InvalidCommitSigError); !ok {
		t.Fatalf("bob sent incorrect error, expected %T, got %T",
			&InvalidCommitSigError{}, err)
	}
}

// TestChannelUnilateralCloseHtlcResolution tests that in the case of a
// unilateral channel closure, then the party that didn't broadcast the
// commitment is able to properly sweep all relevant outputs.
func TestChannelUnilateralCloseHtlcResolution(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We'll start off the test by adding an HTLC in both directions, then
	// initiating enough state transitions to lock both of them in.
	htlcAmount := lnwire.NewMAtomsFromAtoms(200000)
	htlcAlice, _ := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlcAlice); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}
	htlcBob, preimageBob := createHTLC(0, htlcAmount)
	if _, err := bobChannel.AddHTLC(htlcBob, nil); err != nil {
		t.Fatalf("bob unable to add htlc: %v", err)
	}
	if _, err := aliceChannel.ReceiveHTLC(htlcBob); err != nil {
		t.Fatalf("alice unable to recv add htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("Can't update the channel state: %v", err)
	}

	// With both HTLC's locked in, we'll now simulate Bob force closing the
	// transaction on Alice.
	bobForceClose, err := bobChannel.ForceClose()
	if err != nil {
		t.Fatalf("unable to close: %v", err)
	}

	// We'll then use Bob's transaction to trigger a spend notification for
	// Alice.
	closeTx := bobForceClose.CloseTx
	commitTxHash := closeTx.TxHash()
	spendDetail := &chainntnfs.SpendDetail{
		SpendingTx:    closeTx,
		SpenderTxHash: &commitTxHash,
	}
	aliceCloseSummary, err := NewUnilateralCloseSummary(
		aliceChannel.channelState, aliceChannel.Signer,
		spendDetail,
		aliceChannel.channelState.RemoteCommitment,
		aliceChannel.channelState.RemoteCurrentRevocation,
	)
	if err != nil {
		t.Fatalf("unable to create alice close summary: %v", err)
	}

	// She should detect that she can sweep both the outgoing HTLC as well
	// as the incoming one from Bob.
	if len(aliceCloseSummary.HtlcResolutions.OutgoingHTLCs) != 1 {
		t.Fatalf("alice out htlc resolutions not populated: expected %v "+
			"htlcs, got %v htlcs",
			1, len(aliceCloseSummary.HtlcResolutions.OutgoingHTLCs))
	}
	if len(aliceCloseSummary.HtlcResolutions.IncomingHTLCs) != 1 {
		t.Fatalf("alice in htlc resolutions not populated: expected %v "+
			"htlcs, got %v htlcs",
			1, len(aliceCloseSummary.HtlcResolutions.IncomingHTLCs))
	}

	outHtlcResolution := aliceCloseSummary.HtlcResolutions.OutgoingHTLCs[0]
	inHtlcResolution := aliceCloseSummary.HtlcResolutions.IncomingHTLCs[0]

	// First, we'll ensure that Alice can directly spend the outgoing HTLC
	// given a transaction with the proper lock time set.
	receiverHtlcScript := closeTx.TxOut[outHtlcResolution.ClaimOutpoint.Index].PkScript
	sweepTx := wire.NewMsgTx()
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outHtlcResolution.ClaimOutpoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: receiverHtlcScript,
		Value:    outHtlcResolution.SweepSignDesc.Output.Value,
	})
	outHtlcResolution.SweepSignDesc.InputIndex = 0
	sweepTx.LockTime = outHtlcResolution.Expiry

	// With the transaction constructed, we'll generate a witness that
	// should be valid for it, and verify using an instance of Script.
	witness, err := input.ReceiverHtlcSpendTimeout(
		aliceChannel.Signer, &outHtlcResolution.SweepSignDesc,
		sweepTx, int32(outHtlcResolution.Expiry),
	)
	if err != nil {
		t.Fatalf("unable to witness: %v", err)
	}
	sweepTx.TxIn[0].SignatureScript, err = input.WitnessStackToSigScript(witness)
	if err != nil {
		t.Fatalf("unable to convert witness stack to sigScript: %v", err)
	}

	vm, err := txscript.NewEngine(
		outHtlcResolution.SweepSignDesc.Output.PkScript,
		sweepTx, 0, scriptFlagsForTest,
		outHtlcResolution.SweepSignDesc.Output.Version, nil,
	)
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("htlc timeout spend is invalid: %v", err)
	}

	// Next, we'll ensure that we're able to sweep the incoming HTLC with a
	// similar sweep transaction, this time using the payment pre-image.
	senderHtlcScript := closeTx.TxOut[inHtlcResolution.ClaimOutpoint.Index].PkScript
	sweepTx = wire.NewMsgTx()
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: inHtlcResolution.ClaimOutpoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: senderHtlcScript,
		Value:    inHtlcResolution.SweepSignDesc.Output.Value,
	})
	inHtlcResolution.SweepSignDesc.InputIndex = 0
	witness, err = input.SenderHtlcSpendRedeem(
		aliceChannel.Signer, &inHtlcResolution.SweepSignDesc,
		sweepTx, preimageBob[:],
	)
	if err != nil {
		t.Fatalf("unable to construct witnesss: %v", err)
	}
	sweepTx.TxIn[0].SignatureScript, err = input.WitnessStackToSigScript(witness)
	if err != nil {
		t.Fatalf("unable to generate witness for success "+
			"output: %v", err)
	}

	// Finally, we'll verify the constructed witness to ensure that Alice
	// can properly sweep the output.
	vm, err = txscript.NewEngine(
		inHtlcResolution.SweepSignDesc.Output.PkScript,
		sweepTx, 0, scriptFlagsForTest,
		inHtlcResolution.SweepSignDesc.Output.Version, nil,
	)
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("htlc timeout spend is invalid: %v", err)
	}
}

// TestChannelUnilateralClosePendingCommit tests that if the remote party
// broadcasts their pending commit (hasn't yet revoked the lower one), then
// we'll create a proper unilateral channel clsoure that can sweep the created
// outputs.
func TestChannelUnilateralClosePendingCommit(t *testing.T) {
	t.Parallel()

	// Create a test channel which will be used for the duration of this
	// unittest. The channel will be funded evenly with Alice having 5 DCR,
	// and Bob having 5 DCR.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(false)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First, we'll add an HTLC from Alice to Bob, just to be be able to
	// create a new state transition.
	htlcAmount := lnwire.NewMAtomsFromAtoms(20000)
	htlcAlice, _ := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlcAlice); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}

	// With the HTLC added, we'll now manually initiate a state transition
	// from Alice to Bob.
	_, _, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// At this point, Alice's commitment chain should have a new pending
	// commit for Bob. We'll extract it so we can simulate Bob broadcasting
	// the commitment due to an issue.
	bobCommit := aliceChannel.remoteCommitChain.tip().txn
	bobTxHash := bobCommit.TxHash()
	spendDetail := &chainntnfs.SpendDetail{
		SpenderTxHash: &bobTxHash,
		SpendingTx:    bobCommit,
	}

	// At this point, if we attempt to create a unilateral close summary
	// using this commitment, but with the wrong state, we should find that
	// our output wasn't picked up.
	aliceWrongCloseSummary, err := NewUnilateralCloseSummary(
		aliceChannel.channelState, aliceChannel.Signer,
		spendDetail,
		aliceChannel.channelState.RemoteCommitment,
		aliceChannel.channelState.RemoteCurrentRevocation,
	)
	if err != nil {
		t.Fatalf("unable to create alice close summary: %v", err)
	}

	if aliceWrongCloseSummary.CommitResolution != nil {
		t.Fatalf("alice shouldn't have found self output")
	}

	// If we create the close summary again, but this time use Alice's
	// pending commit to Bob, then the unilateral close summary should be
	// properly populated.
	aliceRemoteChainTip, err := aliceChannel.channelState.RemoteCommitChainTip()
	if err != nil {
		t.Fatalf("unable to fetch remote chain tip: %v", err)
	}
	aliceCloseSummary, err := NewUnilateralCloseSummary(
		aliceChannel.channelState, aliceChannel.Signer,
		spendDetail,
		aliceRemoteChainTip.Commitment,
		aliceChannel.channelState.RemoteNextRevocation,
	)
	if err != nil {
		t.Fatalf("unable to create alice close summary: %v", err)
	}

	// With this proper version, Alice's commit resolution should have been
	// properly located.
	if aliceCloseSummary.CommitResolution == nil {
		t.Fatalf("unable to find alice's commit resolution")
	}

	// The proper short channel ID should also be set in Alice's close
	// channel summary.
	if aliceCloseSummary.ChannelCloseSummary.ShortChanID !=
		aliceChannel.ShortChanID() {

		t.Fatalf("wrong short chan ID, expected %v got %v",
			aliceChannel.ShortChanID(),
			aliceCloseSummary.ChannelCloseSummary.ShortChanID)
	}

	aliceSignDesc := aliceCloseSummary.CommitResolution.SelfOutputSignDesc

	// Finally, we'll ensure that we're able to properly sweep our output
	// from using the materials within the unilateral close summary.
	sweepTx := wire.NewMsgTx()
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: aliceCloseSummary.CommitResolution.SelfOutPoint,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: testHdSeed[:],
		Value:    aliceSignDesc.Output.Value,
	})
	witness, err := input.CommitSpendNoDelay(
		aliceChannel.Signer, &aliceSignDesc, sweepTx, false,
	)
	if err != nil {
		t.Fatalf("unable to generate sweep witness: %v", err)
	}
	sweepTx.TxIn[0].SignatureScript, err = input.WitnessStackToSigScript(witness)
	if err != nil {
		t.Fatalf("unable to convert witness stack to sigScript: %v", err)
	}

	// If we validate the signature on the new sweep transaction, it should
	// be fully valid.
	vm, err := txscript.NewEngine(
		aliceSignDesc.Output.PkScript,
		sweepTx, 0, scriptFlagsForTest, aliceSignDesc.Output.Version, nil,
	)
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("htlc timeout spend is invalid: %v", err)
	}
}

// TestDesyncHTLCs checks that we cannot add HTLCs that would make the
// balance negative, when the remote and local update logs are desynced.
func TestDesyncHTLCs(t *testing.T) {
	t.Parallel()

	// We'll kick off the test by creating our channels which both are
	// loaded with 5 DCR each.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First add one HTLC of value 4.1 DCR.
	htlcAmt := lnwire.NewMAtomsFromAtoms(4.1 * dcrutil.AtomsPerCoin)
	htlc, _ := createHTLC(0, htlcAmt)
	aliceIndex, err := aliceChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	bobIndex, err := bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Lock this HTLC in.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}

	// Now let Bob fail this HTLC.
	err = bobChannel.FailHTLC(bobIndex, []byte("failreason"), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	if err := aliceChannel.ReceiveFailHTLC(aliceIndex, []byte("bad")); err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// Alice now has gotten all her original balance (5 DCR) back, however,
	// adding a new HTLC at this point SHOULD fail, since if she adds the
	// HTLC and signs the next state, Bob cannot assume she received the
	// FailHTLC, and must assume she doesn't have the necessary balance
	// available.
	//
	// We try adding an HTLC of value 1 DCR, which should fail because the
	// balance is unavailable.
	htlcAmt = lnwire.NewMAtomsFromAtoms(1 * dcrutil.AtomsPerCoin)
	htlc, _ = createHTLC(1, htlcAmt)
	if _, err = aliceChannel.AddHTLC(htlc, nil); err != ErrBelowChanReserve {
		t.Fatalf("expected ErrInsufficientBalance, instead received: %v",
			err)
	}

	// Now do a state transition, which will ACK the FailHTLC, making Alice
	// able to add the new HTLC.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}
	if _, err = aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
}

// TODO(roasbeef): testing.Quick test case for retrans!!!

// TestMaxAcceptedHTLCs tests that the correct error message (ErrMaxHTLCNumber)
// is thrown when a node tries to accept more than MaxAcceptedHTLCs in a
// channel.
func TestMaxAcceptedHTLCs(t *testing.T) {
	t.Parallel()

	// We'll kick off the test by creating our channels which both are
	// loaded with 5 DCR each.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// One over the maximum number of HTLCs that either can accept.
	const numHTLCs = 20
	const numHTLCsReceived = 12

	// Set the remote's required MaxAcceptedHtlcs. This means that alice
	// can only offer the remote up to numHTLCs HTLCs.
	aliceChannel.localChanCfg.MaxAcceptedHtlcs = numHTLCs
	bobChannel.remoteChanCfg.MaxAcceptedHtlcs = numHTLCs

	// Similarly, set the remote config's MaxAcceptedHtlcs. This means
	// that the remote will be aware that Alice will only accept up to
	// numHTLCsRecevied at a time.
	aliceChannel.remoteChanCfg.MaxAcceptedHtlcs = numHTLCsReceived
	bobChannel.localChanCfg.MaxAcceptedHtlcs = numHTLCsReceived

	// Each HTLC amount is 0.1 DCR.
	htlcAmt := lnwire.NewMAtomsFromAtoms(0.1 * dcrutil.AtomsPerCoin)

	// Send the maximum allowed number of HTLCs.
	for i := 0; i < numHTLCs; i++ {
		htlc, _ := createHTLC(i, htlcAmt)
		if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
			t.Fatalf("unable to add htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("unable to recv htlc: %v", err)
		}
	}

	// The next HTLC should fail with ErrMaxHTLCNumber.
	htlc, _ := createHTLC(numHTLCs, htlcAmt)
	_, err = aliceChannel.AddHTLC(htlc, nil)
	if err != ErrMaxHTLCNumber {
		t.Fatalf("expected ErrMaxHTLCNumber, instead received: %v", err)
	}

	// After receiving the next HTLC, next state transition should fail
	// with ErrMaxHTLCNumber.
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}
	err = ForceStateTransition(aliceChannel, bobChannel)
	if err != ErrMaxHTLCNumber {
		t.Fatalf("expected ErrMaxHTLCNumber, instead received: %v", err)
	}
}

// TestMaxPendingAmount tests that the maximum overall pending HTLC value is met
// given several HTLCs that, combined, exceed this value. An ErrMaxPendingAmount
// error should be returned.
func TestMaxPendingAmount(t *testing.T) {
	t.Parallel()

	// We'll kick off the test by creating our channels which both are
	// loaded with 5 DCR each.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We set the remote required MaxPendingAmount to 3 DCR. We will
	// attempt to overflow this value and see if it gives us the
	// ErrMaxPendingAmount error.
	maxPending := lnwire.NewMAtomsFromAtoms(dcrutil.AtomsPerCoin * 3)

	// We set the max pending amount of Alice's config. This mean that she
	// cannot offer Bob HTLCs with a total value above this limit at a given
	// time.
	aliceChannel.localChanCfg.MaxPendingAmount = maxPending
	bobChannel.remoteChanCfg.MaxPendingAmount = maxPending

	// First, we'll add 2 HTLCs of 1.5 DCR each to Alice's commitment.
	// This won't trigger Alice's ErrMaxPendingAmount error.
	const numHTLCs = 2
	htlcAmt := lnwire.NewMAtomsFromAtoms(1.5 * dcrutil.AtomsPerCoin)
	for i := 0; i < numHTLCs; i++ {
		htlc, _ := createHTLC(i, htlcAmt)
		if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
			t.Fatalf("unable to add htlc: %v", err)
		}
		if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
			t.Fatalf("unable to recv htlc: %v", err)
		}
	}

	// We finally add one more HTLC of 0.1 DCR to Alice's commitment. This
	// SHOULD trigger Alice's ErrMaxPendingAmount error.
	htlcAmt = lnwire.NewMAtomsFromAtoms(0.1 * dcrutil.AtomsPerCoin)
	htlc, _ := createHTLC(numHTLCs, htlcAmt)
	_, err = aliceChannel.AddHTLC(htlc, nil)
	if err != ErrMaxPendingAmount {
		t.Fatalf("expected ErrMaxPendingAmount, instead received: %v", err)
	}

	// And also Bob shouldn't be accepting this HTLC in the next state
	// transition.
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}
	err = ForceStateTransition(aliceChannel, bobChannel)
	if err != ErrMaxPendingAmount {
		t.Fatalf("expected ErrMaxPendingAmount, instead received: %v", err)
	}
}

func assertChannelBalances(t *testing.T, alice, bob *LightningChannel,
	aliceBalance, bobBalance dcrutil.Amount) {

	_, _, line, _ := runtime.Caller(1)

	aliceSelfBalance := alice.channelState.LocalCommitment.LocalBalance.ToAtoms()
	aliceBobBalance := alice.channelState.LocalCommitment.RemoteBalance.ToAtoms()
	if aliceSelfBalance != aliceBalance {
		t.Fatalf("line #%v: wrong alice self balance: expected %v, got %v",
			line, aliceBalance, aliceSelfBalance)
	}
	if aliceBobBalance != bobBalance {
		t.Fatalf("line #%v: wrong alice bob's balance: expected %v, got %v",
			line, bobBalance, aliceBobBalance)
	}

	bobSelfBalance := bob.channelState.LocalCommitment.LocalBalance.ToAtoms()
	bobAliceBalance := bob.channelState.LocalCommitment.RemoteBalance.ToAtoms()
	if bobSelfBalance != bobBalance {
		t.Fatalf("line #%v: wrong bob self balance: expected %v, got %v",
			line, bobBalance, bobSelfBalance)
	}
	if bobAliceBalance != aliceBalance {
		t.Fatalf("line #%v: wrong alice bob's balance: expected %v, got %v",
			line, aliceBalance, bobAliceBalance)
	}
}

// TestChanReserve tests that the ErrBelowChanReserve error is thrown when an
// HTLC is added that causes a node's balance to dip below its channel reserve
// limit.
func TestChanReserve(t *testing.T) {
	t.Parallel()

	setupChannels := func() (*LightningChannel, *LightningChannel, func()) {
		// We'll kick off the test by creating our channels which both
		// are loaded with 5 DCR each.
		aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
		if err != nil {
			t.Fatalf("unable to create test channels: %v", err)
		}

		// We set the remote required ChanReserve to 0.5 DCR. We will
		// attempt to cause Alice's balance to dip below this amount
		// and test whether it triggers the ErrBelowChanReserve error.
		aliceMinReserve := dcrutil.Amount(0.5 *
			dcrutil.AtomsPerCoin)

		// Alice will need to keep her reserve above aliceMinReserve,
		// so set this limit to here local config.
		aliceChannel.localChanCfg.ChanReserve = aliceMinReserve

		// During channel opening Bob will also get to know Alice's
		// minimum reserve, and this will be found in his remote
		// config.
		bobChannel.remoteChanCfg.ChanReserve = aliceMinReserve

		// We set Bob's channel reserve to a value that is larger than
		// his current balance in the channel. This will ensure that
		// after a channel is first opened, Bob can still receive HTLCs
		// even though his balance is less than his channel reserve.
		bobMinReserve := dcrutil.Amount(6 * dcrutil.AtomsPerCoin)
		bobChannel.localChanCfg.ChanReserve = bobMinReserve
		aliceChannel.remoteChanCfg.ChanReserve = bobMinReserve

		return aliceChannel, bobChannel, cleanUp
	}
	aliceChannel, bobChannel, cleanUp := setupChannels()
	defer cleanUp()

	aliceIndex := 0
	bobIndex := 0

	// Add an HTLC that will increase Bob's balance. This should succeed,
	// since Alice stays above her channel reserve, and Bob increases his
	// balance (while still being below his channel reserve).
	//
	// Resulting balances:
	//	Alice:	4.5
	//	Bob:	5.0
	htlcAmt := lnwire.NewMAtomsFromAtoms(0.5 * dcrutil.AtomsPerCoin)
	htlc, _ := createHTLC(aliceIndex, htlcAmt)
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Force a state transition, making sure this HTLC is considered valid
	// even though the channel reserves are not met.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}

	commitFee := aliceChannel.channelState.LocalCommitment.CommitFee
	assertChannelBalances(
		t, aliceChannel, bobChannel,
		dcrutil.AtomsPerCoin*4.5-commitFee, dcrutil.AtomsPerCoin*5,
	)

	// Now let Bob try to add an HTLC. This should fail, since it will
	// decrease his balance, which is already below the channel reserve.
	//
	// Resulting balances:
	//	Alice:	4.5
	//	Bob:	5.0
	htlc, _ = createHTLC(bobIndex, htlcAmt)
	_, err := bobChannel.AddHTLC(htlc, nil)
	if err != ErrBelowChanReserve {
		t.Fatalf("expected ErrBelowChanReserve, instead received: %v", err)
	}

	// Alice will reject this htlc when a state transition is attempted.
	if _, err := aliceChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}
	err = ForceStateTransition(aliceChannel, bobChannel)
	if err != ErrBelowChanReserve {
		t.Fatalf("expected ErrBelowChanReserve, instead received: %v", err)
	}

	// We must setup the channels again, since a violation of the channel
	// constraints leads to channel shutdown.
	aliceChannel, bobChannel, cleanUp = setupChannels()
	defer cleanUp()

	aliceIndex = 0

	// Now we'll add HTLC of 3.5 DCR to Alice's commitment, this should put
	// Alice's balance at 1.5 DCR.
	//
	// Resulting balances:
	//	Alice:	1.5
	//	Bob:	9.5
	htlcAmt = lnwire.NewMAtomsFromAtoms(3.5 * dcrutil.AtomsPerCoin)

	// The first HTLC should successfully be sent.
	htlc, _ = createHTLC(aliceIndex, htlcAmt)
	aliceIndex++
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Add a second HTLC of 1 DCR. This should fail because it will take
	// Alice's balance all the way down to her channel reserve, but since
	// she is the initiator the additional transaction fee makes her
	// balance dip below.
	htlcAmt = lnwire.NewMAtomsFromAtoms(1 * dcrutil.AtomsPerCoin)
	htlc, _ = createHTLC(aliceIndex, htlcAmt)
	_, err = aliceChannel.AddHTLC(htlc, nil)
	if err != ErrBelowChanReserve {
		t.Fatalf("expected ErrBelowChanReserve, instead received: %v", err)
	}

	// Likewise, Bob will reject a state transition after this htlc is
	// received, of the same reason.
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}
	err = ForceStateTransition(aliceChannel, bobChannel)
	if err != ErrBelowChanReserve {
		t.Fatalf("expected ErrBelowChanReserve, instead received: %v", err)
	}

	// We must setup the channels again, since a violation of the channel
	// constraints leads to channel shutdown.
	aliceChannel, bobChannel, cleanUp = setupChannels()
	defer cleanUp()

	aliceIndex = 0
	bobIndex = 0

	// Add a HTLC of 2 DCR to Alice, and the settle it.
	// Resulting balances:
	//	Alice:	3.0
	//	Bob:	7.0
	htlcAmt = lnwire.NewMAtomsFromAtoms(2 * dcrutil.AtomsPerCoin)
	htlc, preimage := createHTLC(aliceIndex, htlcAmt)
	aliceHtlcIndex, err := aliceChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	bobHtlcIndex, err := bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}

	commitFee = aliceChannel.channelState.LocalCommitment.CommitFee
	assertChannelBalances(
		t, aliceChannel, bobChannel,
		dcrutil.AtomsPerCoin*3-commitFee, dcrutil.AtomsPerCoin*5,
	)

	if err := bobChannel.SettleHTLC(preimage, bobHtlcIndex, nil, nil, nil); err != nil {
		t.Fatalf("bob unable to settle inbound htlc: %v", err)
	}
	if err := aliceChannel.ReceiveHTLCSettle(preimage, aliceHtlcIndex); err != nil {
		t.Fatalf("alice unable to accept settle of outbound htlc: %v", err)
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}

	commitFee = aliceChannel.channelState.LocalCommitment.CommitFee
	assertChannelBalances(
		t, aliceChannel, bobChannel,
		dcrutil.AtomsPerCoin*3-commitFee, dcrutil.AtomsPerCoin*7,
	)

	// And now let Bob add an HTLC of 1 DCR. This will take Bob's balance
	// all the way down to his channel reserve, but since he is not paying
	// the fee this is okay.
	htlcAmt = lnwire.NewMAtomsFromAtoms(1 * dcrutil.AtomsPerCoin)
	htlc, _ = createHTLC(bobIndex, htlcAmt)
	if _, err := bobChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := aliceChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// Do a last state transition, which should succeed.
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}

	commitFee = aliceChannel.channelState.LocalCommitment.CommitFee
	assertChannelBalances(
		t, aliceChannel, bobChannel,
		dcrutil.AtomsPerCoin*3-commitFee, dcrutil.AtomsPerCoin*6,
	)
}

// TestMinHTLC tests that the ErrBelowMinHTLC error is thrown if an HTLC is added
// that is below the minimm allowed value for HTLCs.
func TestMinHTLC(t *testing.T) {
	t.Parallel()

	// We'll kick off the test by creating our channels which both are
	// loaded with 5 DCR each.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We set Alice's MinHTLC to 0.1 DCR. We will attempt to send an
	// HTLC BELOW this value to trigger the ErrBelowMinHTLC error.
	minValue := lnwire.NewMAtomsFromAtoms(0.1 * dcrutil.AtomsPerCoin)

	// Setting the min value in Alice's local config means that the
	// remote will not accept any HTLCs of value less than specified.
	aliceChannel.localChanCfg.MinHTLC = minValue
	bobChannel.remoteChanCfg.MinHTLC = minValue

	// First, we will add an HTLC of 0.5 DCR. This will not trigger
	// ErrBelowMinHTLC.
	htlcAmt := lnwire.NewMAtomsFromAtoms(0.5 * dcrutil.AtomsPerCoin)
	htlc, _ := createHTLC(0, htlcAmt)
	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	// We add an HTLC below the min value, this should result in
	// an ErrBelowMinHTLC error.
	amt := minValue - 100
	htlc, _ = createHTLC(1, amt)
	_, err = aliceChannel.AddHTLC(htlc, nil)
	if err != ErrBelowMinHTLC {
		t.Fatalf("expected ErrBelowMinHTLC, instead received: %v", err)
	}

	// Bob will receive this HTLC, but reject the next state update, since
	// the htlc is too small.
	_, err = bobChannel.ReceiveHTLC(htlc)
	if err != nil {
		t.Fatalf("error receiving htlc: %v", err)
	}
	err = ForceStateTransition(aliceChannel, bobChannel)
	if err != ErrBelowMinHTLC {
		t.Fatalf("expected ErrBelowMinHTLC, instead received: %v", err)
	}
}

// TestNewBreachRetributionSkipsDustHtlcs ensures that in the case of a
// contract breach, all dust HTLCs are ignored and not reflected in the
// produced BreachRetribution struct. We ignore these HTLCs as they aren't
// actually manifested on the commitment transaction, as a result we can't
// actually revoked them.
func TestNewBreachRetributionSkipsDustHtlcs(t *testing.T) {
	t.Parallel()

	// We'll kick off the test by creating our channels which both are
	// loaded with 5 DCR each.
	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	var fakeOnionBlob [lnwire.OnionPacketSize]byte
	copy(fakeOnionBlob[:], bytes.Repeat([]byte{0x05}, lnwire.OnionPacketSize))

	// We'll modify the dust settings on both channels to be a predictable
	// value for the prurpose of the test.
	dustValue := dcrutil.Amount(200)
	aliceChannel.channelState.LocalChanCfg.DustLimit = dustValue
	aliceChannel.channelState.RemoteChanCfg.DustLimit = dustValue
	bobChannel.channelState.LocalChanCfg.DustLimit = dustValue
	bobChannel.channelState.RemoteChanCfg.DustLimit = dustValue

	// We'll now create a series of dust HTLC's, and send then from Alice
	// to Bob, finally locking both of them in.
	var bobPreimage [32]byte
	copy(bobPreimage[:], bytes.Repeat([]byte{0xbb}, 32))
	for i := 0; i < 3; i++ {
		rHash := sha256.Sum256(bobPreimage[:])
		h := &lnwire.UpdateAddHTLC{
			PaymentHash: rHash,
			Amount:      lnwire.NewMAtomsFromAtoms(dustValue),
			Expiry:      uint32(10),
			OnionBlob:   fakeOnionBlob,
		}

		htlcIndex, err := aliceChannel.AddHTLC(h, nil)
		if err != nil {
			t.Fatalf("unable to add bob's htlc: %v", err)
		}

		h.ID = htlcIndex
		if _, err := bobChannel.ReceiveHTLC(h); err != nil {
			t.Fatalf("unable to recv bob's htlc: %v", err)
		}
	}

	// With the HTLC's applied to both update logs, we'll initiate a state
	// transition from Alice.
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// At this point, we'll capture the current state number, as well as
	// the current commitment.
	revokedStateNum := aliceChannel.channelState.LocalCommitment.CommitHeight

	// We'll now have Bob settle those HTLC's to Alice and then advance
	// forward to a new state.
	for i := 0; i < 3; i++ {
		err := bobChannel.SettleHTLC(bobPreimage, uint64(i), nil, nil, nil)
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}
		err = aliceChannel.ReceiveHTLCSettle(bobPreimage, uint64(i))
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}
	}
	if err := ForceStateTransition(bobChannel, aliceChannel); err != nil {
		t.Fatalf("unable to complete bob's state transition: %v", err)
	}

	// At this point, we'll now simulate a contract breach by Bob using the
	// NewBreachRetribution method.
	breachRet, err := NewBreachRetribution(
		aliceChannel.channelState, revokedStateNum, 100,
	)
	if err != nil {
		t.Fatalf("unable to create breach retribution: %v", err)
	}

	// The retribution shouldn't have any HTLCs set as they were all below
	// dust for both parties.
	if len(breachRet.HtlcRetributions) != 0 {
		t.Fatalf("zero HTLC retributions should have been created, "+
			"instead %v were", len(breachRet.HtlcRetributions))
	}
}

// compareHtlcs compares two PaymentDescriptors.
func compareHtlcs(htlc1, htlc2 *PaymentDescriptor) error {
	if htlc1.LogIndex != htlc2.LogIndex {
		return fmt.Errorf("htlc log index did not match")
	}
	if htlc1.HtlcIndex != htlc2.HtlcIndex {
		return fmt.Errorf("htlc index did not match")
	}
	if htlc1.ParentIndex != htlc2.ParentIndex {
		return fmt.Errorf("htlc parent index did not match")
	}

	if htlc1.RHash != htlc2.RHash {
		return fmt.Errorf("htlc rhash did not match")
	}
	return nil
}

// compareIndexes is a helper method to compare two index maps.
func compareIndexes(a, b map[uint64]*list.Element) error {
	for k1, e1 := range a {
		e2, ok := b[k1]
		if !ok {
			return fmt.Errorf("element with key %d "+
				"not found in b", k1)
		}
		htlc1, htlc2 := e1.Value.(*PaymentDescriptor), e2.Value.(*PaymentDescriptor)
		if err := compareHtlcs(htlc1, htlc2); err != nil {
			return err
		}
	}

	for k1, e1 := range b {
		e2, ok := a[k1]
		if !ok {
			return fmt.Errorf("element with key %d not "+
				"found in a", k1)
		}
		htlc1, htlc2 := e1.Value.(*PaymentDescriptor), e2.Value.(*PaymentDescriptor)
		if err := compareHtlcs(htlc1, htlc2); err != nil {
			return err
		}
	}

	return nil
}

// compareLogs is a helper method to compare two updateLogs.
func compareLogs(a, b *updateLog) error {
	if a.logIndex != b.logIndex {
		return fmt.Errorf("log indexes don't match: %d vs %d",
			a.logIndex, b.logIndex)
	}

	if a.htlcCounter != b.htlcCounter {
		return fmt.Errorf("htlc counters don't match: %d vs %d",
			a.htlcCounter, b.htlcCounter)
	}

	if err := compareIndexes(a.updateIndex, b.updateIndex); err != nil {
		return fmt.Errorf("update indexes don't match: %v", err)
	}
	if err := compareIndexes(a.htlcIndex, b.htlcIndex); err != nil {
		return fmt.Errorf("htlc indexes don't match: %v", err)
	}

	if a.Len() != b.Len() {
		return fmt.Errorf("list lengths not equal: %d vs %d",
			a.Len(), b.Len())
	}

	e1, e2 := a.Front(), b.Front()
	for ; e1 != nil; e1, e2 = e1.Next(), e2.Next() {
		htlc1, htlc2 := e1.Value.(*PaymentDescriptor), e2.Value.(*PaymentDescriptor)
		if err := compareHtlcs(htlc1, htlc2); err != nil {
			return err
		}
	}

	return nil
}

// TestChannelRestoreUpdateLogs makes sure we are able to properly restore the
// update logs in the case where a different number of HTLCs are locked in on
// the local, remote and pending remote commitment.
func TestChannelRestoreUpdateLogs(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First, we'll add an HTLC from Alice to Bob, which we will lock in on
	// Bob's commit, but not on Alice's.
	htlcAmount := lnwire.NewMAtomsFromAtoms(20000)
	htlcAlice, _ := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlcAlice); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}

	// Let Alice sign a new state, which will include the HTLC just sent.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// Bob receives this commitment signature, and revokes his old state.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("unable to receive commitment: %v", err)
	}
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke commitment: %v", err)
	}

	// When Alice now receives this revocation, she will advance her remote
	// commitment chain to the commitment which includes the HTLC just
	// sent. However her local commitment chain still won't include the
	// state with the HTLC, since she hasn't received a new commitment
	// signature from Bob yet.
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("unable to recive revocation: %v", err)
	}

	// Now make Alice send and sign an additional HTLC. We don't let Bob
	// receive it. We do this since we want to check that update logs are
	// restored properly below, and we'll only restore updates that have
	// been ACKed.
	htlcAlice, _ = createHTLC(1, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}

	// Send the signature covering the HTLC. This is okay, since the local
	// and remote commit chains are updated in an async fashion. Since the
	// remote chain was updated with the latest state (since Bob sent the
	// revocation earlier) we can keep advancing the remote commit chain.
	_, _, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// After Alice has signed this commitment, her local commitment will
	// contain no HTLCs, her remote commitment will contain an HTLC with
	// index 0, and the pending remote commitment (a signed remote
	// commitment which is not AKCed yet) will contain an additional HTLC
	// with index 1.

	// We now re-create the channels, mimicking a restart. This should sync
	// the update logs up to the correct state set up above.
	newAliceChannel, err := NewLightningChannel(
		aliceChannel.Signer, aliceChannel.channelState,
		aliceChannel.sigPool,
	)
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}

	newBobChannel, err := NewLightningChannel(
		bobChannel.Signer, bobChannel.channelState,
		bobChannel.sigPool,
	)
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}

	// compare all the logs between the old and new channels, to make sure
	// they all got restored properly.
	err = compareLogs(aliceChannel.localUpdateLog,
		newAliceChannel.localUpdateLog)
	if err != nil {
		t.Fatalf("alice local log not restored: %v", err)
	}

	err = compareLogs(aliceChannel.remoteUpdateLog,
		newAliceChannel.remoteUpdateLog)
	if err != nil {
		t.Fatalf("alice remote log not restored: %v", err)
	}

	err = compareLogs(bobChannel.localUpdateLog,
		newBobChannel.localUpdateLog)
	if err != nil {
		t.Fatalf("bob local log not restored: %v", err)
	}

	err = compareLogs(bobChannel.remoteUpdateLog,
		newBobChannel.remoteUpdateLog)
	if err != nil {
		t.Fatalf("bob remote log not restored: %v", err)
	}
}

// fetchNumUpdates counts the number of updateType in the log.
func fetchNumUpdates(t updateType, log *updateLog) int {
	num := 0
	for e := log.Front(); e != nil; e = e.Next() {
		htlc := e.Value.(*PaymentDescriptor)
		if htlc.EntryType == t {
			num++
		}
	}
	return num
}

// assertInLog checks that the given log contains the expected number of Adds
// and Fails.
func assertInLog(t *testing.T, log *updateLog, numAdds, numFails int) {
	adds := fetchNumUpdates(Add, log)
	if adds != numAdds {
		t.Fatalf("expected %d adds, found %d", numAdds, adds)
	}
	fails := fetchNumUpdates(Fail, log)
	if fails != numFails {
		t.Fatalf("expected %d fails, found %d", numFails, fails)
	}
}

// assertInLogs asserts that the expected number of Adds and Fails occurs in
// the local and remote update log of the given channel.
func assertInLogs(t *testing.T, channel *LightningChannel, numAddsLocal,
	numFailsLocal, numAddsRemote, numFailsRemote int) {
	assertInLog(t, channel.localUpdateLog, numAddsLocal, numFailsLocal)
	assertInLog(t, channel.remoteUpdateLog, numAddsRemote, numFailsRemote)
}

// restoreAndAssert creates a new LightningChannel from the given channel's
// state, and asserts that the new channel has had its logs restored to the
// expected state.
func restoreAndAssert(t *testing.T, channel *LightningChannel, numAddsLocal,
	numFailsLocal, numAddsRemote, numFailsRemote int) {

	newChannel, err := NewLightningChannel(
		channel.Signer, channel.channelState,
		channel.sigPool,
	)
	if err != nil {
		t.Fatalf("unable to create new channel: %v", err)
	}

	assertInLog(t, newChannel.localUpdateLog, numAddsLocal, numFailsLocal)
	assertInLog(t, newChannel.remoteUpdateLog, numAddsRemote, numFailsRemote)
}

// TesstChannelRestoreUpdateLogsFailedHTLC runs through a scenario where an
// HTLC is added and failed, and asserts along the way that we would restore
// the update logs of the channel to the expected state at any point.
func TestChannelRestoreUpdateLogsFailedHTLC(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First, we'll add an HTLC from Alice to Bob, and lock it in for both.
	htlcAmount := lnwire.NewMAtomsFromAtoms(20000)
	htlcAlice, _ := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	// The htlc Alice sent should be in her local update log.
	assertInLogs(t, aliceChannel, 1, 0, 0, 0)

	// A restore at this point should NOT restore this update, as it is not
	// locked in anywhere yet.
	restoreAndAssert(t, aliceChannel, 0, 0, 0, 0)

	if _, err := bobChannel.ReceiveHTLC(htlcAlice); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}

	// Lock in the Add on both sides.
	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}

	// Since it is locked in, Alice should have the Add in the local log,
	// and it should be restored during restoration.
	assertInLogs(t, aliceChannel, 1, 0, 0, 0)
	restoreAndAssert(t, aliceChannel, 1, 0, 0, 0)

	// Now we make Bob fail this HTLC.
	err = bobChannel.FailHTLC(0, []byte("failreason"), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}

	err = aliceChannel.ReceiveFailHTLC(0, []byte("failreason"))
	if err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// This Fail update should have been added to Alice's remote update log.
	assertInLogs(t, aliceChannel, 1, 0, 0, 1)

	// Restoring should restore the HTLC added to Alice's local log, but
	// NOT the Fail sent by Bob, since it is not locked in.
	restoreAndAssert(t, aliceChannel, 1, 0, 0, 0)

	// Bob sends a signature.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("unable to receive commitment: %v", err)
	}

	// When Alice receives Bob's new commitment, the logs will stay the
	// same until she revokes her old state. The Fail will still not be
	// restored during a restoration.
	assertInLogs(t, aliceChannel, 1, 0, 0, 1)
	restoreAndAssert(t, aliceChannel, 1, 0, 0, 0)

	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke commitment: %v", err)
	}
	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("bob unable to process alice's revocation: %v", err)
	}

	// At this point Alice has advanced her local commitment chain to a
	// commitment with no HTLCs left. The current state on her remote
	// commitment chain, however, still has the HTLC active, as she hasn't
	// sent a new signature yet.
	assertInLogs(t, aliceChannel, 1, 0, 0, 1)
	restoreAndAssert(t, aliceChannel, 1, 0, 0, 0)

	// Now send a signature from Alice. This will give Bob a new commitment
	// where the HTLC is removed.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("unable to receive commitment: %v", err)
	}

	// When sending a new commitment, Alice will add a pending commit to
	// here remote chain. In this case it doesn't contain any new updates,
	// so it won't affect the restoration.
	assertInLogs(t, aliceChannel, 1, 0, 0, 1)
	restoreAndAssert(t, aliceChannel, 1, 0, 0, 0)

	// When Alice receives Bob's revocation, the Fail is irrevocably locked
	// in on both sides. She should compact the logs, removing the HTLC and
	// the corresponding Fail from the local update log.
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke commitment: %v", err)
	}
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("unable to receive revocation: %v", err)
	}

	assertInLogs(t, aliceChannel, 0, 0, 0, 0)
	restoreAndAssert(t, aliceChannel, 0, 0, 0, 0)
}

// TestDuplicateFailRejection tests that if either party attempts to fail an
// HTLC twice, then we'll reject the second fail attempt.
func TestDuplicateFailRejection(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First, we'll add an HTLC from Alice to Bob, and lock it in for both
	// parties.
	htlcAmount := lnwire.NewMAtomsFromAtoms(20000)
	htlcAlice, _ := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	_, err = bobChannel.ReceiveHTLC(htlcAlice)
	if err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}

	// With the HTLC locked in, we'll now have Bob fail the HTLC back to
	// Alice.
	err = bobChannel.FailHTLC(0, []byte("failreason"), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	if err := aliceChannel.ReceiveFailHTLC(0, []byte("bad")); err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// If we attempt to fail it AGAIN, then both sides should reject this
	// second failure attempt.
	err = bobChannel.FailHTLC(0, []byte("failreason"), nil, nil, nil)
	if err == nil {
		t.Fatalf("duplicate HTLC failure attempt should have failed")
	}
	if err := aliceChannel.ReceiveFailHTLC(0, []byte("bad")); err == nil {
		t.Fatalf("duplicate HTLC failure attempt should have failed")
	}

	// We'll now have Bob sign a new commitment to lock in the HTLC fail
	// for Alice.
	_, _, _, err = bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commit: %v", err)
	}

	// We'll now force a restart for Bob and Alice, so we can test the
	// persistence related portion of this assertion.
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}

	// If we try to fail the same HTLC again, then we should get an error.
	err = bobChannel.FailHTLC(0, []byte("failreason"), nil, nil, nil)
	if err == nil {
		t.Fatalf("duplicate HTLC failure attempt should have failed")
	}

	// Alice on the other hand should accept the failure again, as she
	// dropped all items in the logs which weren't committed.
	if err := aliceChannel.ReceiveFailHTLC(0, []byte("bad")); err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}
}

// TestDuplicateSettleRejection tests that if either party attempts to settle
// an HTLC twice, then we'll reject the second settle attempt.
func TestDuplicateSettleRejection(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// First, we'll add an HTLC from Alice to Bob, and lock it in for both
	// parties.
	htlcAmount := lnwire.NewMAtomsFromAtoms(20000)
	htlcAlice, alicePreimage := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	_, err = bobChannel.ReceiveHTLC(htlcAlice)
	if err != nil {
		t.Fatalf("unable to recv htlc: %v", err)
	}

	if err := ForceStateTransition(aliceChannel, bobChannel); err != nil {
		t.Fatalf("unable to complete state update: %v", err)
	}

	// With the HTLC locked in, we'll now have Bob settle the HTLC back to
	// Alice.
	err = bobChannel.SettleHTLC(alicePreimage, uint64(0), nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to cancel HTLC: %v", err)
	}
	err = aliceChannel.ReceiveHTLCSettle(alicePreimage, uint64(0))
	if err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}

	// If we attempt to fail it AGAIN, then both sides should reject this
	// second failure attempt.
	err = bobChannel.SettleHTLC(alicePreimage, uint64(0), nil, nil, nil)
	if err == nil {
		t.Fatalf("duplicate HTLC failure attempt should have failed")
	}
	err = aliceChannel.ReceiveHTLCSettle(alicePreimage, uint64(0))
	if err == nil {
		t.Fatalf("duplicate HTLC failure attempt should have failed")
	}

	// We'll now have Bob sign a new commitment to lock in the HTLC fail
	// for Alice.
	_, _, _, err = bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commit: %v", err)
	}

	// We'll now force a restart for Bob and Alice, so we can test the
	// persistence related portion of this assertion.
	bobChannel, err = restartChannel(bobChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}
	aliceChannel, err = restartChannel(aliceChannel)
	if err != nil {
		t.Fatalf("unable to restart channel: %v", err)
	}

	// If we try to fail the same HTLC again, then we should get an error.
	err = bobChannel.SettleHTLC(alicePreimage, uint64(0), nil, nil, nil)
	if err == nil {
		t.Fatalf("duplicate HTLC failure attempt should have failed")
	}

	// Alice on the other hand should accept the failure again, as she
	// dropped all items in the logs which weren't committed.
	err = aliceChannel.ReceiveHTLCSettle(alicePreimage, uint64(0))
	if err != nil {
		t.Fatalf("unable to recv htlc cancel: %v", err)
	}
}

// TestChannelRestoreCommitHeight tests that the local and remote commit
// heights of HTLCs are set correctly across restores.
func TestChannelRestoreCommitHeight(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// helper method to check add heights of the htlcs found in the given
	// log after a restore.
	restoreAndAssertCommitHeights := func(t *testing.T,
		channel *LightningChannel, remoteLog bool, htlcIndex uint64,
		expLocal, expRemote uint64) *LightningChannel {

		newChannel, err := NewLightningChannel(
			channel.Signer, channel.channelState, channel.sigPool,
		)
		if err != nil {
			t.Fatalf("unable to create new channel: %v", err)
		}

		var pd *PaymentDescriptor
		if remoteLog {
			if newChannel.localUpdateLog.lookupHtlc(htlcIndex) != nil {
				t.Fatalf("htlc found in wrong log")
			}
			pd = newChannel.remoteUpdateLog.lookupHtlc(htlcIndex)

		} else {
			if newChannel.remoteUpdateLog.lookupHtlc(htlcIndex) != nil {
				t.Fatalf("htlc found in wrong log")
			}
			pd = newChannel.localUpdateLog.lookupHtlc(htlcIndex)
		}
		if pd == nil {
			t.Fatalf("htlc not found in log")
		}

		if pd.addCommitHeightLocal != expLocal {
			t.Fatalf("expected local add height to be %d, was %d",
				expLocal, pd.addCommitHeightLocal)
		}
		if pd.addCommitHeightRemote != expRemote {
			t.Fatalf("expected remote add height to be %d, was %d",
				expRemote, pd.addCommitHeightRemote)
		}
		return newChannel
	}

	// We'll send an HtLC from Alice to Bob.
	htlcAmount := lnwire.NewMAtomsFromAtoms(100000000)
	htlcAlice, _ := createHTLC(0, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlcAlice); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}

	// Let Alice sign a new state, which will include the HTLC just sent.
	aliceSig, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// The HTLC should only be on the pending remote commitment, so the
	// only the remote add height should be set during a restore.
	aliceChannel = restoreAndAssertCommitHeights(t, aliceChannel, false,
		0, 0, 1)

	// Bob receives this commitment signature, and revokes his old state.
	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("unable to receive commitment: %v", err)
	}
	bobRevocation, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke commitment: %v", err)
	}

	// Now the HTLC is locked into Bob's commitment, a restoration should
	// set only the local commit height, as it is not locked into Alice's
	// yet.
	bobChannel = restoreAndAssertCommitHeights(t, bobChannel, true, 0, 1, 0)

	// Alice receives the revocation, ACKing her pending commitment.
	_, _, _, _, err = aliceChannel.ReceiveRevocation(bobRevocation)
	if err != nil {
		t.Fatalf("unable to recive revocation: %v", err)
	}

	// However, the HTLC is still not locked into her local commitment, so
	// the local add height should still be 0 after a restoration.
	aliceChannel = restoreAndAssertCommitHeights(t, aliceChannel, false,
		0, 0, 1)

	// Now let Bob send the commitment signature making the HTLC lock in on
	// Alice's commitment.
	bobSig, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// At this stage Bob has a pending remote commitment. Make sure
	// restoring at this stage correcly restores the HTLC add commit
	// heights.
	bobChannel = restoreAndAssertCommitHeights(t, bobChannel, true, 0, 1, 1)

	err = aliceChannel.ReceiveNewCommitment(bobSig, bobHtlcSigs)
	if err != nil {
		t.Fatalf("unable to receive commitment: %v", err)
	}
	aliceRevocation, _, err := aliceChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke commitment: %v", err)
	}

	// Now both the local and remote add heights should be properly set.
	aliceChannel = restoreAndAssertCommitHeights(t, aliceChannel, false,
		0, 1, 1)

	_, _, _, _, err = bobChannel.ReceiveRevocation(aliceRevocation)
	if err != nil {
		t.Fatalf("unable to recive revocation: %v", err)
	}

	// Alice ACKing Bob's pending commitment shouldn't change the heights
	// restored.
	bobChannel = restoreAndAssertCommitHeights(t, bobChannel, true, 0, 1, 1)

	// Send andother HTLC from Alice to Bob, to test whether already
	// existing HTLCs (the HTLC with index 0) keep getting the add heights
	// restored properly.
	htlcAlice, _ = createHTLC(1, htlcAmount)
	if _, err := aliceChannel.AddHTLC(htlcAlice, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlcAlice); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}

	// Send a new signature from Alice to Bob, making Alice have a pending
	// remote commitment.
	aliceSig, aliceHtlcSigs, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// A restoration should keep the add heights iof the first HTLC, and
	// the new HTLC should have a remote add height 2.
	restoreAndAssertCommitHeights(t, aliceChannel, false, 0, 1, 1)
	restoreAndAssertCommitHeights(t, aliceChannel, false, 1, 0, 2)

	err = bobChannel.ReceiveNewCommitment(aliceSig, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("unable to receive commitment: %v", err)
	}
	_, _, err = bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke commitment: %v", err)
	}

	// Since Bob just revoked another commitment, a restoration should
	// increase the add height of the firt HTLC to 2, as we only keep the
	// last unrevoked commitment. The new HTLC will also have a local add
	// height of 2.
	bobChannel = restoreAndAssertCommitHeights(t, bobChannel, true, 0, 2, 1)
	bobChannel = restoreAndAssertCommitHeights(t, bobChannel, true, 1, 2, 0)

	// Sign a new state for Alice, making Bob have a pending remote
	// commitment.
	_, _, _, err = bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commitment: %v", err)
	}

	// The signing of a new commitment for Alice should have given the new
	// HTLC an add height.
	restoreAndAssertCommitHeights(t, bobChannel, true, 0, 2, 1)
	restoreAndAssertCommitHeights(t, bobChannel, true, 1, 2, 2)
}

// TestForceCloseFailLocalDataLoss tests that we don't allow a force close of a
// channel that's in a non-default state.
func TestForceCloseFailLocalDataLoss(t *testing.T) {
	t.Parallel()

	aliceChannel, _, cleanUp, err := CreateTestChannels(false)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Now that we have our set of channels, we'll modify the channel state
	// to have a non-default channel flag.
	err = aliceChannel.channelState.ApplyChanStatus(
		channeldb.ChanStatusLocalDataLoss,
	)
	if err != nil {
		t.Fatalf("unable to apply channel state: %v", err)
	}

	// Due to the change above, if we attempt to force close this
	// channel, we should fail as it isn't safe to force close a
	// channel that isn't in the pure default state.
	_, err = aliceChannel.ForceClose()
	if err == nil {
		t.Fatalf("expected force close to fail due to non-default " +
			"chan state")
	}
}

// TestForceCloseBorkedState tests that once we force close a channel, it's
// marked as borked in the database. Additionally, all calls to mutate channel
// state should also fail.
func TestForceCloseBorkedState(t *testing.T) {
	t.Parallel()

	aliceChannel, bobChannel, cleanUp, err := CreateTestChannels(false)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// Do the commitment dance until Bob sends a revocation so Alice is
	// able to receive the revocation, and then also make a new state
	// herself.
	aliceSigs, aliceHtlcSigs, _, err := aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commit: %v", err)
	}
	err = bobChannel.ReceiveNewCommitment(aliceSigs, aliceHtlcSigs)
	if err != nil {
		t.Fatalf("unable to receive commitment: %v", err)
	}
	revokeMsg, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke bob commitment: %v", err)
	}
	bobSigs, bobHtlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("unable to sign commit: %v", err)
	}
	err = aliceChannel.ReceiveNewCommitment(bobSigs, bobHtlcSigs)
	if err != nil {
		t.Fatalf("unable to receive commitment: %v", err)
	}

	// Now that we have a new Alice channel, we'll force close once to
	// trigger the update on disk to mark the channel as borked.
	if _, err := aliceChannel.ForceClose(); err != nil {
		t.Fatalf("unable to force close channel: %v", err)
	}

	// Next we'll mark the channel as borked before we proceed.
	err = aliceChannel.channelState.ApplyChanStatus(
		channeldb.ChanStatusBorked,
	)
	if err != nil {
		t.Fatalf("unable to apply chan status: %v", err)
	}

	// The on-disk state should indicate that the channel is now borked.
	if !aliceChannel.channelState.HasChanStatus(
		channeldb.ChanStatusBorked,
	) {
		t.Fatalf("chan status not updated as borked")
	}

	// At this point, all channel mutating methods should now fail as they
	// shouldn't be able to proceed if the channel is borked.
	_, _, _, _, err = aliceChannel.ReceiveRevocation(revokeMsg)
	if err != channeldb.ErrChanBorked {
		t.Fatalf("advance commitment tail should have failed")
	}

	// We manually advance the commitment tail here since the above
	// ReceiveRevocation call will fail before it's actually advanced.
	aliceChannel.remoteCommitChain.advanceTail()
	_, _, _, err = aliceChannel.SignNextCommitment()
	if err != channeldb.ErrChanBorked {
		t.Fatalf("sign commitment should have failed: %v", err)
	}
	_, _, err = aliceChannel.RevokeCurrentCommitment()
	if err != channeldb.ErrChanBorked {
		t.Fatalf("append remove chain tail should have failed")
	}
}

// TestChannelMaxFeeRate ensures we correctly compute a channel initiator's max
// fee rate based on an allocation and its available balance. It should never
// dip below the established fee floor.
func TestChannelMaxFeeRate(t *testing.T) {
	t.Parallel()

	aliceChannel, _, cleanUp, err := CreateTestChannels(true)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	assertMaxFeeRate := func(maxAlloc float64, expFeeRate AtomPerKByte) {
		maxFeeRate := aliceChannel.MaxFeeRate(maxAlloc)
		if maxFeeRate != expFeeRate {
			t.Fatalf("expected max fee rate of %v with max "+
				"allocation of %v, got %v", expFeeRate,
				maxAlloc, maxFeeRate)
		}
	}

	// Calculate the estimated fee rate when attempting to use the full
	// channel balance of the initiator as fee.
	commitSize := input.EstimateCommitmentTxSize(0)
	chanBalance := float64(dcrutil.Amount(5 * dcrutil.AtomsPerCoin))
	fullMaxFee := AtomPerKByte(chanBalance / (float64(commitSize) / 1000.0))

	// Assert the estimation is the expected one. This helps ensure any
	// changes to how the test balance, fee or commitment size are
	// calculated breaks this test.
	wantMaxFee := AtomPerKByte(1373626373)
	if fullMaxFee != wantMaxFee {
		t.Fatalf("unexpected full max fee (want=%s got=%s)", wantMaxFee,
			fullMaxFee)
	}

	assertMaxFeeRate(1.0, fullMaxFee)
	assertMaxFeeRate(0.001, fullMaxFee/1000)
	assertMaxFeeRate(0.00001, fullMaxFee/100000)
	assertMaxFeeRate(0.0000001, FeePerKBFloor)
}
