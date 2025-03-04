package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/gogo/protobuf/proto"
	"github.com/prysmaticlabs/go-bitfield"
	"github.com/prysmaticlabs/go-ssz"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/state"
	db2 "github.com/prysmaticlabs/prysm/beacon-chain/db"
	"github.com/prysmaticlabs/prysm/beacon-chain/internal"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	ethpb "github.com/prysmaticlabs/prysm/proto/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/bls"
	"github.com/prysmaticlabs/prysm/shared/hashutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/testutil"
	logTest "github.com/sirupsen/logrus/hooks/test"
)

// Ensure operations service implements intefaces.
var _ = OperationFeeds(&Service{})
var _ = Pool(&Service{})

type mockBroadcaster struct {
}

func (mb *mockBroadcaster) Broadcast(_ context.Context, _ proto.Message) error {
	return nil
}

func TestStop_OK(t *testing.T) {
	hook := logTest.NewGlobal()
	opsService := NewOpsPoolService(context.Background(), &Config{})

	if err := opsService.Stop(); err != nil {
		t.Fatalf("Unable to stop operation service: %v", err)
	}

	msg := hook.LastEntry().Message
	want := "Stopping service"
	if msg != want {
		t.Errorf("incorrect log, expected %s, got %s", want, msg)
	}

	// The context should have been canceled.
	if opsService.ctx.Err() != context.Canceled {
		t.Error("context was not canceled")
	}
	hook.Reset()
}

func TestServiceStatus_Error(t *testing.T) {
	service := NewOpsPoolService(context.Background(), &Config{})
	if service.Status() != nil {
		t.Errorf("service status should be nil to begin with, got: %v", service.error)
	}
	err := errors.New("error error error")
	service.error = err

	if service.Status() != err {
		t.Error("service status did not return wanted err")
	}
}

func TestIncomingExits_Ok(t *testing.T) {
	hook := logTest.NewGlobal()
	beaconDB := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, beaconDB)
	service := NewOpsPoolService(context.Background(), &Config{BeaconDB: beaconDB})

	exit := &ethpb.VoluntaryExit{Epoch: 100}
	if err := service.HandleValidatorExits(context.Background(), exit); err != nil {
		t.Error(err)
	}

	want := fmt.Sprintf("Exit request saved in DB")
	testutil.AssertLogsContain(t, hook, want)
}

func TestHandleAttestation_Saves_NewAttestation(t *testing.T) {
	beaconDB := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, beaconDB)
	broadcaster := &mockBroadcaster{}
	service := NewOpsPoolService(context.Background(), &Config{
		BeaconDB: beaconDB,
		P2P:      broadcaster,
	})

	deposits, privKeys := testutil.SetupInitialDeposits(t, 100)
	beaconState, err := state.GenesisBeaconState(deposits, uint64(0), &ethpb.Eth1Data{})
	if err != nil {
		t.Fatal(err)
	}

	att := &ethpb.Attestation{
		Data: &ethpb.AttestationData{
			Source: &ethpb.Checkpoint{Epoch: 0, Root: []byte("hello-world")},
			Target: &ethpb.Checkpoint{Epoch: 0},
			Crosslink: &ethpb.Crosslink{
				Shard:      0,
				StartEpoch: 0,
			},
		},
		AggregationBits: bitfield.Bitlist{0xC0, 0xC0, 0xC0, 0xC0, 0x01},
		CustodyBits:     bitfield.Bitlist{0x00, 0x00, 0x00, 0x00, 0x01},
	}

	attestingIndices, err := helpers.AttestingIndices(beaconState, att.Data, att.AggregationBits)
	if err != nil {
		t.Error(err)
	}
	dataAndCustodyBit := &pb.AttestationDataAndCustodyBit{
		Data:       att.Data,
		CustodyBit: false,
	}
	domain := helpers.Domain(beaconState, 0, params.BeaconConfig().DomainAttestation)
	sigs := make([]*bls.Signature, len(attestingIndices))
	for i, indice := range attestingIndices {
		hashTreeRoot, err := ssz.HashTreeRoot(dataAndCustodyBit)
		if err != nil {
			t.Error(err)
		}
		sig := privKeys[indice].Sign(hashTreeRoot[:], domain)
		sigs[i] = sig
	}
	att.Signature = bls.AggregateSignatures(sigs).Marshal()[:]

	beaconState.CurrentCrosslinks = []*ethpb.Crosslink{
		{
			Shard:      0,
			StartEpoch: 0,
		},
	}
	beaconState.CurrentJustifiedCheckpoint.Root = []byte("hello-world")
	beaconState.CurrentEpochAttestations = []*pb.PendingAttestation{}

	newBlock := &ethpb.BeaconBlock{
		Slot: 0,
	}
	if err := beaconDB.SaveBlockDeprecated(newBlock); err != nil {
		t.Fatal(err)
	}
	if err := beaconDB.UpdateChainHead(context.Background(), newBlock, beaconState); err != nil {
		t.Fatal(err)
	}
	beaconState.Slot += params.BeaconConfig().MinAttestationInclusionDelay

	encoded, err := ssz.HashTreeRoot(beaconState.CurrentCrosslinks[0])
	if err != nil {
		t.Fatal(err)
	}
	att.Data.Crosslink.ParentRoot = encoded[:]
	att.Data.Crosslink.DataRoot = params.BeaconConfig().ZeroHash[:]

	if err := service.HandleAttestation(context.Background(), att); err != nil {
		t.Error(err)
	}
}

func TestHandleAttestation_Aggregates_SameAttestationData(t *testing.T) {
	beaconDB := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, beaconDB)
	ctx := context.Background()
	broadcaster := &mockBroadcaster{}
	service := NewOpsPoolService(context.Background(), &Config{
		BeaconDB: beaconDB,
		P2P:      broadcaster,
	})

	deposits, privKeys := testutil.SetupInitialDeposits(t, 200)
	beaconState, err := state.GenesisBeaconState(deposits, uint64(0), &ethpb.Eth1Data{})
	if err != nil {
		t.Fatal(err)
	}

	beaconState.CurrentCrosslinks = []*ethpb.Crosslink{
		{
			Shard:      0,
			StartEpoch: 0,
		},
	}
	beaconState.CurrentJustifiedCheckpoint.Root = []byte("hello-world")
	beaconState.CurrentEpochAttestations = []*pb.PendingAttestation{}

	att1 := &ethpb.Attestation{
		Data: &ethpb.AttestationData{
			Source: &ethpb.Checkpoint{Epoch: 0, Root: []byte("hello-world")},
			Target: &ethpb.Checkpoint{Epoch: 0},
			Crosslink: &ethpb.Crosslink{
				Shard:      1,
				StartEpoch: 0,
			},
		},
		CustodyBits: bitfield.Bitlist{0x00, 0x00, 0x00, 0x00, 0x01},
	}

	encoded, err := ssz.HashTreeRoot(beaconState.CurrentCrosslinks[0])
	if err != nil {
		t.Fatal(err)
	}
	att1.Data.Crosslink.ParentRoot = encoded[:]
	att1.Data.Crosslink.DataRoot = params.BeaconConfig().ZeroHash[:]

	committee, err := helpers.CrosslinkCommittee(beaconState, att1.Data.Target.Epoch, att1.Data.Crosslink.Shard)
	if err != nil {
		t.Error(err)
	}
	aggregationBits := bitfield.NewBitlist(uint64(len(committee)))
	aggregationBits.SetBitAt(0, true)
	att1.AggregationBits = aggregationBits

	dataAndCustodyBit := &pb.AttestationDataAndCustodyBit{
		Data:       att1.Data,
		CustodyBit: false,
	}
	hashTreeRoot, err := ssz.HashTreeRoot(dataAndCustodyBit)
	if err != nil {
		t.Error(err)
	}
	domain := helpers.Domain(beaconState, 0, params.BeaconConfig().DomainAttestation)
	att1.Signature = privKeys[committee[0]].Sign(hashTreeRoot[:], domain).Marshal()

	att2 := &ethpb.Attestation{
		Data: &ethpb.AttestationData{
			Source: &ethpb.Checkpoint{Epoch: 0, Root: []byte("hello-world")},
			Target: &ethpb.Checkpoint{Epoch: 0},
			Crosslink: &ethpb.Crosslink{
				Shard:      1,
				StartEpoch: 0,
			},
		},
		CustodyBits: bitfield.Bitlist{0x00, 0x00, 0x00, 0x00, 0x01},
	}
	aggregationBits = bitfield.NewBitlist(uint64(len(committee)))
	aggregationBits.SetBitAt(1, true)
	att2.AggregationBits = aggregationBits

	att2.Data.Crosslink.ParentRoot = encoded[:]
	att2.Data.Crosslink.DataRoot = params.BeaconConfig().ZeroHash[:]
	att2.Signature = privKeys[committee[1]].Sign(hashTreeRoot[:], domain).Marshal()

	newBlock := &ethpb.BeaconBlock{
		Slot: 0,
	}
	if err := beaconDB.SaveBlockDeprecated(newBlock); err != nil {
		t.Fatal(err)
	}
	if err := beaconDB.UpdateChainHead(context.Background(), newBlock, beaconState); err != nil {
		t.Fatal(err)
	}
	beaconState.Slot += params.BeaconConfig().MinAttestationInclusionDelay

	if err := service.HandleAttestation(context.Background(), att1); err != nil {
		t.Error(err)
	}

	if err := service.HandleAttestation(context.Background(), att2); err != nil {
		t.Error(err)
	}

	attDataHash, err := hashutil.HashProto(att2.Data)
	if err != nil {
		t.Error(err)
	}
	dbAtt, err := service.beaconDB.Attestation(ctx, attDataHash)
	if err != nil {
		t.Error(err)
	}

	dbAttBits := dbAtt.AggregationBits.Bytes()
	aggregatedBits := att1.AggregationBits.Or(att2.AggregationBits).Bytes()
	if !bytes.Equal(dbAttBits, aggregatedBits) {
		t.Error("Expected aggregation bits to be equal.")
	}

	att1Sig, err := bls.SignatureFromBytes(att1.Signature)
	if err != nil {
		t.Error(err)
	}
	att2Sig, err := bls.SignatureFromBytes(att2.Signature)
	if err != nil {
		t.Error(err)
	}
	aggregatedSig := bls.AggregateSignatures([]*bls.Signature{att1Sig, att2Sig})
	if !bytes.Equal(dbAtt.Signature, aggregatedSig.Marshal()) {
		t.Error("Expected aggregated signatures to be equal")
	}
}

func TestHandleAttestation_Skips_PreviouslyAggregatedAttestations(t *testing.T) {
	beaconDB := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, beaconDB)
	ctx := context.Background()
	helpers.ClearAllCaches()
	broadcaster := &mockBroadcaster{}
	service := NewOpsPoolService(context.Background(), &Config{
		BeaconDB: beaconDB,
		P2P:      broadcaster,
	})

	deposits, privKeys := testutil.SetupInitialDeposits(t, 200)
	beaconState, err := state.GenesisBeaconState(deposits, uint64(0), &ethpb.Eth1Data{})
	if err != nil {
		t.Fatal(err)
	}

	beaconState.CurrentCrosslinks = []*ethpb.Crosslink{
		{
			Shard:      0,
			StartEpoch: 0,
		},
	}
	beaconState.CurrentJustifiedCheckpoint.Root = []byte("hello-world")
	beaconState.CurrentEpochAttestations = []*pb.PendingAttestation{}

	att1 := &ethpb.Attestation{
		Data: &ethpb.AttestationData{
			Source: &ethpb.Checkpoint{Epoch: 0, Root: []byte("hello-world")},
			Target: &ethpb.Checkpoint{Epoch: 0},
			Crosslink: &ethpb.Crosslink{
				Shard:      0,
				StartEpoch: 0,
			},
		},
		CustodyBits: bitfield.Bitlist{0x00, 0x00, 0x00, 0x00, 0x01},
	}

	encoded, err := ssz.HashTreeRoot(beaconState.CurrentCrosslinks[0])
	if err != nil {
		t.Fatal(err)
	}
	att1.Data.Crosslink.ParentRoot = encoded[:]
	att1.Data.Crosslink.DataRoot = params.BeaconConfig().ZeroHash[:]

	committee, err := helpers.CrosslinkCommittee(beaconState, att1.Data.Target.Epoch, att1.Data.Crosslink.Shard)
	if err != nil {
		t.Error(err)
	}
	aggregationBits := bitfield.NewBitlist(uint64(len(committee)))
	aggregationBits.SetBitAt(0, true)
	att1.AggregationBits = aggregationBits

	dataAndCustodyBit := &pb.AttestationDataAndCustodyBit{
		Data:       att1.Data,
		CustodyBit: false,
	}
	hashTreeRoot, err := ssz.HashTreeRoot(dataAndCustodyBit)
	if err != nil {
		t.Error(err)
	}
	domain := helpers.Domain(beaconState, 0, params.BeaconConfig().DomainAttestation)
	att1.Signature = privKeys[committee[0]].Sign(hashTreeRoot[:], domain).Marshal()

	att2 := &ethpb.Attestation{
		Data: &ethpb.AttestationData{
			Source: &ethpb.Checkpoint{Epoch: 0, Root: []byte("hello-world")},
			Target: &ethpb.Checkpoint{Epoch: 0},
			Crosslink: &ethpb.Crosslink{
				Shard:      0,
				StartEpoch: 0,
			},
		},
		CustodyBits: bitfield.Bitlist{0x00, 0x00, 0x00, 0x00, 0x01},
	}
	aggregationBits = bitfield.NewBitlist(uint64(len(committee)))
	aggregationBits.SetBitAt(1, true)
	att2.AggregationBits = aggregationBits

	att2.Data.Crosslink.ParentRoot = encoded[:]
	att2.Data.Crosslink.DataRoot = params.BeaconConfig().ZeroHash[:]
	att2.Signature = privKeys[committee[1]].Sign(hashTreeRoot[:], domain).Marshal()

	att3 := &ethpb.Attestation{
		Data: &ethpb.AttestationData{
			Source: &ethpb.Checkpoint{Epoch: 0, Root: []byte("hello-world")},
			Target: &ethpb.Checkpoint{Epoch: 0},
			Crosslink: &ethpb.Crosslink{
				Shard:      0,
				StartEpoch: 0,
			},
		},
		CustodyBits: bitfield.Bitlist{0x00, 0x00, 0x00, 0x00, 0x01},
	}
	aggregationBits = bitfield.NewBitlist(uint64(len(committee)))
	aggregationBits.SetBitAt(0, true)
	aggregationBits.SetBitAt(1, true)
	att3.AggregationBits = aggregationBits

	att3.Data.Crosslink.ParentRoot = encoded[:]
	att3.Data.Crosslink.DataRoot = params.BeaconConfig().ZeroHash[:]
	att3Sig1 := privKeys[committee[0]].Sign(hashTreeRoot[:], domain)
	att3Sig2 := privKeys[committee[1]].Sign(hashTreeRoot[:], domain)
	aggregatedSig := bls.AggregateSignatures([]*bls.Signature{att3Sig1, att3Sig2}).Marshal()
	att3.Signature = aggregatedSig[:]

	newBlock := &ethpb.BeaconBlock{
		Slot: 0,
	}
	if err := beaconDB.SaveBlockDeprecated(newBlock); err != nil {
		t.Fatal(err)
	}
	if err := beaconDB.UpdateChainHead(context.Background(), newBlock, beaconState); err != nil {
		t.Fatal(err)
	}
	beaconState.Slot += params.BeaconConfig().MinAttestationInclusionDelay

	if err := service.HandleAttestation(context.Background(), att1); err != nil {
		t.Error(err)
	}

	if err := service.HandleAttestation(context.Background(), att2); err != nil {
		t.Error(err)
	}

	if err := service.HandleAttestation(context.Background(), att1); err != nil {
		t.Error(err)
	}

	attDataHash, err := hashutil.HashProto(att2.Data)
	if err != nil {
		t.Error(err)
	}
	dbAtt, err := service.beaconDB.Attestation(ctx, attDataHash)
	if err != nil {
		t.Error(err)
	}
	dbAttBits := dbAtt.AggregationBits.Bytes()
	aggregatedBits := att1.AggregationBits.Or(att2.AggregationBits).Bytes()
	if !bytes.Equal(dbAttBits, aggregatedBits) {
		t.Error("Expected aggregation bits to be equal.")
	}

	if !bytes.Equal(dbAtt.Signature, aggregatedSig) {
		t.Error("Expected aggregated signatures to be equal")
	}

	if err := service.HandleAttestation(context.Background(), att2); err != nil {
		t.Error(err)
	}
	dbAtt, err = service.beaconDB.Attestation(ctx, attDataHash)
	if err != nil {
		t.Error(err)
	}
	dbAttBits = dbAtt.AggregationBits.Bytes()
	if !bytes.Equal(dbAttBits, aggregatedBits) {
		t.Error("Expected aggregation bits to be equal.")
	}

	if !bytes.Equal(dbAtt.Signature, aggregatedSig) {
		t.Error("Expected aggregated signatures to be equal")
	}

	if err := service.HandleAttestation(context.Background(), att3); err != nil {
		t.Error(err)
	}
	dbAtt, err = service.beaconDB.Attestation(ctx, attDataHash)
	if err != nil {
		t.Error(err)
	}
	dbAttBits = dbAtt.AggregationBits.Bytes()
	if !bytes.Equal(dbAttBits, aggregatedBits) {
		t.Error("Expected aggregation bits to be equal.")
	}

	if !bytes.Equal(dbAtt.Signature, aggregatedSig) {
		t.Error("Expected aggregated signatures to be equal")
	}
}

func TestRetrieveAttestations_OK(t *testing.T) {
	helpers.ClearAllCaches()

	beaconDB := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, beaconDB)
	service := NewOpsPoolService(context.Background(), &Config{BeaconDB: beaconDB})

	// Save 140 attestations for test. During 1st retrieval we should get slot:1 - slot:61 attestations.
	// The 1st retrieval is set at slot 64.
	origAttestations := make([]*ethpb.Attestation, 140)
	for i := 0; i < len(origAttestations); i++ {
		origAttestations[i] = &ethpb.Attestation{
			Data: &ethpb.AttestationData{
				Crosslink: &ethpb.Crosslink{
					Shard: uint64(i),
				},
				Source: &ethpb.Checkpoint{},
				Target: &ethpb.Checkpoint{},
			},
		}
		if err := service.beaconDB.SaveAttestation(context.Background(), origAttestations[i]); err != nil {
			t.Fatalf("Failed to save attestation: %v", err)
		}
	}
	if err := beaconDB.SaveStateDeprecated(context.Background(), &pb.BeaconState{
		Slot: 64,
		CurrentCrosslinks: []*ethpb.Crosslink{{
			StartEpoch: 0,
			DataRoot:   params.BeaconConfig().ZeroHash[:]}}}); err != nil {
		t.Fatal(err)
	}
	// Test we can retrieve attestations from slot1 - slot61.
	attestations, err := service.AttestationPool(context.Background(), 64)
	if err != nil {
		t.Fatalf("Could not retrieve attestations: %v", err)
	}
	sort.Slice(attestations, func(i, j int) bool {
		return attestations[i].Data.Crosslink.Shard < attestations[j].Data.Crosslink.Shard
	})

	if !reflect.DeepEqual(attestations, origAttestations[0:127]) {
		t.Error("Retrieved attestations did not match")
	}
}

func TestRetrieveAttestations_PruneInvalidAtts(t *testing.T) {
	helpers.ClearAllCaches()

	beaconDB := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, beaconDB)
	service := NewOpsPoolService(context.Background(), &Config{BeaconDB: beaconDB})

	// Save 140 attestations for slots 0 to 139.
	origAttestations := make([]*ethpb.Attestation, 140)
	shardDiff := uint64(192)
	for i := 0; i < len(origAttestations); i++ {
		origAttestations[i] = &ethpb.Attestation{
			Data: &ethpb.AttestationData{
				Crosslink: &ethpb.Crosslink{
					Shard: uint64(i) - shardDiff,
				},
				Source: &ethpb.Checkpoint{},
				Target: &ethpb.Checkpoint{},
			},
		}
		if err := service.beaconDB.SaveAttestation(context.Background(), origAttestations[i]); err != nil {
			t.Fatalf("Failed to save attestation: %v", err)
		}
	}

	// At slot 200 only attestations up to from slot 137 to 139 are valid attestations.
	if err := beaconDB.SaveStateDeprecated(context.Background(), &pb.BeaconState{
		Slot: 200,
		CurrentCrosslinks: []*ethpb.Crosslink{{
			StartEpoch: 2,
			DataRoot:   params.BeaconConfig().ZeroHash[:]}}}); err != nil {
		t.Fatal(err)
	}
	attestations, err := service.AttestationPool(context.Background(), 200)
	if err != nil {
		t.Fatalf("Could not retrieve attestations: %v", err)
	}

	if !reflect.DeepEqual(attestations, origAttestations[137:]) {
		t.Error("Incorrect pruned attestations")
	}

	// Verify the invalid attestations are deleted.
	hash, err := hashutil.HashProto(origAttestations[1])
	if err != nil {
		t.Fatal(err)
	}
	if service.beaconDB.HasAttestation(context.Background(), hash) {
		t.Error("Invalid attestation is not deleted")
	}
}

func TestRemoveProcessedAttestations_Ok(t *testing.T) {
	db := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, db)
	s := NewOpsPoolService(context.Background(), &Config{BeaconDB: db})

	attestations := make([]*ethpb.Attestation, 10)
	for i := 0; i < len(attestations); i++ {
		attestations[i] = &ethpb.Attestation{
			Data: &ethpb.AttestationData{
				Crosslink: &ethpb.Crosslink{
					Shard: uint64(i),
				},
				Source: &ethpb.Checkpoint{},
				Target: &ethpb.Checkpoint{},
			},
		}
		if err := s.beaconDB.SaveAttestation(context.Background(), attestations[i]); err != nil {
			t.Fatalf("Failed to save attestation: %v", err)
		}
	}
	if err := db.SaveStateDeprecated(context.Background(), &pb.BeaconState{
		Slot: 15,
		CurrentCrosslinks: []*ethpb.Crosslink{{
			StartEpoch: 0,
			DataRoot:   params.BeaconConfig().ZeroHash[:]}}}); err != nil {
		t.Fatal(err)
	}

	retrievedAtts, err := s.AttestationPool(context.Background(), 15)
	if err != nil {
		t.Fatalf("Could not retrieve attestations: %v", err)
	}
	if !reflect.DeepEqual(attestations, retrievedAtts) {
		t.Error("Retrieved attestations did not match prev generated attestations")
	}

	if err := s.removeAttestationsFromPool(context.Background(), attestations); err != nil {
		t.Fatalf("Could not remove attestations: %v", err)
	}

	retrievedAtts, _ = s.AttestationPool(context.Background(), 15)
	if len(retrievedAtts) != 0 {
		t.Errorf("Attestation pool should be empty but got a length of %d", len(retrievedAtts))
	}
}

func TestReceiveBlkRemoveOps_Ok(t *testing.T) {
	db := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, db)
	s := NewOpsPoolService(context.Background(), &Config{BeaconDB: db})

	attestations := make([]*ethpb.Attestation, 10)
	for i := 0; i < len(attestations); i++ {
		attestations[i] = &ethpb.Attestation{
			Data: &ethpb.AttestationData{
				Crosslink: &ethpb.Crosslink{
					Shard: uint64(i),
				},
				Source: &ethpb.Checkpoint{},
				Target: &ethpb.Checkpoint{},
			},
		}
		if err := s.beaconDB.SaveAttestation(context.Background(), attestations[i]); err != nil {
			t.Fatalf("Failed to save attestation: %v", err)
		}
	}

	if err := db.SaveStateDeprecated(context.Background(), &pb.BeaconState{
		Slot: 15,
		CurrentCrosslinks: []*ethpb.Crosslink{{
			StartEpoch: 0,
			DataRoot:   params.BeaconConfig().ZeroHash[:]}}}); err != nil {
		t.Fatal(err)
	}

	atts, _ := s.AttestationPool(context.Background(), 15)
	if len(atts) != len(attestations) {
		t.Errorf("Attestation pool should be %d but got a length of %d",
			len(attestations), len(atts))
	}

	block := &ethpb.BeaconBlock{
		Body: &ethpb.BeaconBlockBody{
			Attestations: attestations,
		},
	}

	s.incomingProcessedBlock <- block
	if err := s.handleProcessedBlock(context.Background(), block); err != nil {
		t.Error(err)
	}

	atts, _ = s.AttestationPool(context.Background(), 15)
	if len(atts) != 0 {
		t.Errorf("Attestation pool should be empty but got a length of %d", len(atts))
	}
}

func TestIsCanonical_CanGetCanonical(t *testing.T) {
	t.Skip()
	// TODO(#2307): This will be irrelevant after the revamp of our DB package post v0.6.
	db := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, db)
	s := NewOpsPoolService(context.Background(), &Config{BeaconDB: db})

	cb1 := &ethpb.BeaconBlock{Slot: 999, ParentRoot: []byte{'A'}}
	if err := s.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(cb1); err != nil {
		t.Fatal(err)
	}
	if err := s.beaconDB.(*db2.BeaconDB).UpdateChainHead(context.Background(), cb1, &pb.BeaconState{}); err != nil {
		t.Fatal(err)
	}
	r1, err := ssz.SigningRoot(cb1)
	if err != nil {
		t.Fatal(err)
	}
	att1 := &ethpb.Attestation{Data: &ethpb.AttestationData{BeaconBlockRoot: r1[:]}}
	canonical, err := s.IsAttCanonical(context.Background(), att1)
	if err != nil {
		t.Fatal(err)
	}
	if !canonical {
		t.Error("Attestation should be canonical")
	}

	cb2 := &ethpb.BeaconBlock{Slot: 1000, ParentRoot: []byte{'B'}}
	if err := s.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(cb2); err != nil {
		t.Fatal(err)
	}
	if err := s.beaconDB.(*db2.BeaconDB).UpdateChainHead(context.Background(), cb2, &pb.BeaconState{}); err != nil {
		t.Fatal(err)
	}
	canonical, err = s.IsAttCanonical(context.Background(), att1)
	if err != nil {
		t.Fatal(err)
	}
	if canonical {
		t.Error("Attestation should not be canonical")
	}
}

func TestIsCanonical_NilBlocks(t *testing.T) {
	db := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, db)
	s := NewOpsPoolService(context.Background(), &Config{BeaconDB: db})

	canonical, err := s.IsAttCanonical(context.Background(), &ethpb.Attestation{Data: &ethpb.AttestationData{}})
	if err != nil {
		t.Fatal(err)
	}
	if canonical {
		t.Error("Attestation shouldn't be canonical")
	}

	cb1 := &ethpb.BeaconBlock{Slot: 999, ParentRoot: []byte{'A'}}
	if err := s.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(cb1); err != nil {
		t.Fatal(err)
	}
	r1, err := ssz.SigningRoot(cb1)
	if err != nil {
		t.Fatal(err)
	}
	att1 := &ethpb.Attestation{Data: &ethpb.AttestationData{BeaconBlockRoot: r1[:]}}
	canonical, err = s.IsAttCanonical(context.Background(), att1)
	if err != nil {
		t.Fatal(err)
	}
	if canonical {
		t.Error("Attestation shouldn't be canonical")
	}
}
