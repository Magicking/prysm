package rpc

import (
	"context"
	"sync"
	"testing"

	"github.com/gogo/protobuf/proto"
	"github.com/prysmaticlabs/go-ssz"
	"github.com/prysmaticlabs/prysm/beacon-chain/cache"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	db2 "github.com/prysmaticlabs/prysm/beacon-chain/db"
	"github.com/prysmaticlabs/prysm/beacon-chain/internal"
	pbp2p "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/rpc/v1"
	ethpb "github.com/prysmaticlabs/prysm/proto/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/params"
)

type mockBroadcaster struct{}

func (m *mockBroadcaster) Broadcast(ctx context.Context, msg proto.Message) error {
	return nil
}

func TestSubmitAttestation_OK(t *testing.T) {
	db := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, db)
	ctx := context.Background()

	mockOperationService := &mockOperationService{}
	attesterServer := &AttesterServer{
		operationService: mockOperationService,
		p2p:              &mockBroadcaster{},
		beaconDB:         db,
		cache:            cache.NewAttestationCache(),
	}
	head := &ethpb.BeaconBlock{
		Slot:       999,
		ParentRoot: []byte{'a'},
	}
	if err := attesterServer.beaconDB.SaveBlock(ctx, head); err != nil {
		t.Fatal(err)
	}
	root, err := ssz.SigningRoot(head)
	if err != nil {
		t.Fatal(err)
	}

	validators := make([]*ethpb.Validator, params.BeaconConfig().MinGenesisActiveValidatorCount/16)
	for i := 0; i < len(validators); i++ {
		validators[i] = &ethpb.Validator{
			ExitEpoch:        params.BeaconConfig().FarFutureEpoch,
			EffectiveBalance: params.BeaconConfig().MaxEffectiveBalance,
		}
	}

	state := &pbp2p.BeaconState{
		Slot:             params.BeaconConfig().SlotsPerEpoch + 1,
		Validators:       validators,
		RandaoMixes:      make([][]byte, params.BeaconConfig().EpochsPerHistoricalVector),
		ActiveIndexRoots: make([][]byte, params.BeaconConfig().EpochsPerHistoricalVector),
	}

	if err := db.SaveStateDeprecated(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	req := &ethpb.Attestation{
		Data: &ethpb.AttestationData{
			BeaconBlockRoot: root[:],
			Crosslink: &ethpb.Crosslink{
				Shard:    935,
				DataRoot: []byte{'a'},
			},
			Source: &ethpb.Checkpoint{},
			Target: &ethpb.Checkpoint{},
		},
	}
	if _, err := attesterServer.SubmitAttestation(context.Background(), req); err != nil {
		t.Errorf("Could not attest head correctly: %v", err)
	}
}

func TestRequestAttestation_OK(t *testing.T) {
	db := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, db)
	ctx := context.Background()

	block := &ethpb.BeaconBlock{
		Slot: 3*params.BeaconConfig().SlotsPerEpoch + 1,
	}
	targetBlock := &ethpb.BeaconBlock{
		Slot: 1 * params.BeaconConfig().SlotsPerEpoch,
	}
	justifiedBlock := &ethpb.BeaconBlock{
		Slot: 2 * params.BeaconConfig().SlotsPerEpoch,
	}
	blockRoot, err := ssz.SigningRoot(block)
	if err != nil {
		t.Fatalf("Could not hash beacon block: %v", err)
	}
	justifiedRoot, err := ssz.SigningRoot(justifiedBlock)
	if err != nil {
		t.Fatalf("Could not get signing root for justified block: %v", err)
	}
	targetRoot, err := ssz.SigningRoot(targetBlock)
	if err != nil {
		t.Fatalf("Could not get signing root for target block: %v", err)
	}

	beaconState := &pbp2p.BeaconState{
		Slot:       3*params.BeaconConfig().SlotsPerEpoch + 1,
		BlockRoots: make([][]byte, params.BeaconConfig().SlotsPerHistoricalRoot),
		CurrentCrosslinks: []*ethpb.Crosslink{
			{
				DataRoot: []byte("A"),
			},
		},
		PreviousCrosslinks: []*ethpb.Crosslink{
			{
				DataRoot: []byte("A"),
			},
		},
		CurrentJustifiedCheckpoint: &ethpb.Checkpoint{
			Epoch: 2,
			Root:  justifiedRoot[:],
		},
	}
	beaconState.BlockRoots[1] = blockRoot[:]
	beaconState.BlockRoots[1*params.BeaconConfig().SlotsPerEpoch] = targetRoot[:]
	beaconState.BlockRoots[2*params.BeaconConfig().SlotsPerEpoch] = justifiedRoot[:]
	attesterServer := &AttesterServer{
		beaconDB: db,
		p2p:      &mockBroadcaster{},
		cache:    cache.NewAttestationCache(),
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(targetBlock); err != nil {
		t.Fatalf("Could not save block in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).UpdateChainHead(ctx, targetBlock, beaconState); err != nil {
		t.Fatalf("Could not update chain head in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(justifiedBlock); err != nil {
		t.Fatalf("Could not save block in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).UpdateChainHead(ctx, justifiedBlock, beaconState); err != nil {
		t.Fatalf("Could not update chain head in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(block); err != nil {
		t.Fatalf("Could not save block in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).UpdateChainHead(ctx, block, beaconState); err != nil {
		t.Fatalf("Could not update chain head in test db: %v", err)
	}
	req := &pb.AttestationRequest{
		Shard: 0,
		Slot:  3*params.BeaconConfig().SlotsPerEpoch + 1,
	}
	res, err := attesterServer.RequestAttestation(context.Background(), req)
	if err != nil {
		t.Fatalf("Could not get attestation info at slot: %v", err)
	}

	crosslinkRoot, err := ssz.HashTreeRoot(beaconState.CurrentCrosslinks[req.Shard])
	if err != nil {
		t.Fatal(err)
	}

	expectedInfo := &ethpb.AttestationData{
		BeaconBlockRoot: blockRoot[:],
		Source: &ethpb.Checkpoint{
			Epoch: 2,
			Root:  justifiedRoot[:],
		},
		Target: &ethpb.Checkpoint{
			Epoch: 3,
		},
		Crosslink: &ethpb.Crosslink{
			EndEpoch:   3,
			ParentRoot: crosslinkRoot[:],
			DataRoot:   params.BeaconConfig().ZeroHash[:],
		},
	}

	if !proto.Equal(res, expectedInfo) {
		t.Errorf("Expected attestation info to match, received %v, wanted %v", res, expectedInfo)
	}
}

func TestAttestationDataAtSlot_handlesFarAwayJustifiedEpoch(t *testing.T) {
	// Scenario:
	//
	// State slot = 10000
	// Last justified slot = epoch start of 1500
	// HistoricalRootsLimit = 8192
	//
	// More background: https://github.com/prysmaticlabs/prysm/issues/2153
	db := internal.SetupDBDeprecated(t)
	defer internal.TeardownDBDeprecated(t, db)
	// This test breaks if it doesnt use mainnet config
	params.OverrideBeaconConfig(params.MainnetConfig())
	defer params.OverrideBeaconConfig(params.MinimalSpecConfig())
	ctx := context.Background()

	// Ensure HistoricalRootsLimit matches scenario
	cfg := params.BeaconConfig()
	cfg.HistoricalRootsLimit = 8192
	params.OverrideBeaconConfig(cfg)

	block := &ethpb.BeaconBlock{
		Slot: 10000,
	}
	epochBoundaryBlock := &ethpb.BeaconBlock{
		Slot: helpers.StartSlot(helpers.SlotToEpoch(10000)),
	}
	justifiedBlock := &ethpb.BeaconBlock{
		Slot: helpers.StartSlot(helpers.SlotToEpoch(1500)) - 2, // Imagine two skip block
	}
	blockRoot, err := ssz.SigningRoot(block)
	if err != nil {
		t.Fatalf("Could not hash beacon block: %v", err)
	}
	justifiedBlockRoot, err := ssz.SigningRoot(justifiedBlock)
	if err != nil {
		t.Fatalf("Could not hash justified block: %v", err)
	}
	epochBoundaryRoot, err := ssz.SigningRoot(epochBoundaryBlock)
	if err != nil {
		t.Fatalf("Could not hash justified block: %v", err)
	}
	beaconState := &pbp2p.BeaconState{
		Slot:       10000,
		BlockRoots: make([][]byte, params.BeaconConfig().SlotsPerHistoricalRoot),
		PreviousCrosslinks: []*ethpb.Crosslink{
			{
				DataRoot: []byte("A"),
			},
		},
		CurrentCrosslinks: []*ethpb.Crosslink{
			{
				DataRoot: []byte("A"),
			},
		},
		CurrentJustifiedCheckpoint: &ethpb.Checkpoint{
			Epoch: helpers.SlotToEpoch(1500),
			Root:  justifiedBlockRoot[:],
		},
	}
	beaconState.BlockRoots[1] = blockRoot[:]
	beaconState.BlockRoots[1*params.BeaconConfig().SlotsPerEpoch] = epochBoundaryRoot[:]
	beaconState.BlockRoots[2*params.BeaconConfig().SlotsPerEpoch] = justifiedBlockRoot[:]
	attesterServer := &AttesterServer{
		beaconDB: db,
		p2p:      &mockBroadcaster{},
		cache:    cache.NewAttestationCache(),
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(epochBoundaryBlock); err != nil {
		t.Fatalf("Could not save block in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).UpdateChainHead(ctx, epochBoundaryBlock, beaconState); err != nil {
		t.Fatalf("Could not update chain head in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(justifiedBlock); err != nil {
		t.Fatalf("Could not save block in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).UpdateChainHead(ctx, justifiedBlock, beaconState); err != nil {
		t.Fatalf("Could not update chain head in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).SaveBlockDeprecated(block); err != nil {
		t.Fatalf("Could not save block in test db: %v", err)
	}
	if err := attesterServer.beaconDB.(*db2.BeaconDB).UpdateChainHead(ctx, block, beaconState); err != nil {
		t.Fatalf("Could not update chain head in test db: %v", err)
	}
	req := &pb.AttestationRequest{
		Shard: 0,
		Slot:  10000,
	}
	res, err := attesterServer.RequestAttestation(context.Background(), req)
	if err != nil {
		t.Fatalf("Could not get attestation info at slot: %v", err)
	}

	crosslinkRoot, err := ssz.HashTreeRoot(beaconState.CurrentCrosslinks[req.Shard])
	if err != nil {
		t.Fatal(err)
	}

	expectedInfo := &ethpb.AttestationData{
		BeaconBlockRoot: blockRoot[:],
		Source: &ethpb.Checkpoint{
			Epoch: helpers.SlotToEpoch(1500),
			Root:  justifiedBlockRoot[:],
		},
		Target: &ethpb.Checkpoint{
			Epoch: 156,
		},
		Crosslink: &ethpb.Crosslink{
			ParentRoot: crosslinkRoot[:],
			EndEpoch:   params.BeaconConfig().SlotsPerEpoch,
			DataRoot:   params.BeaconConfig().ZeroHash[:],
		},
	}

	if !proto.Equal(res, expectedInfo) {
		t.Errorf("Expected attestation info to match, received %v, wanted %v", res, expectedInfo)
	}
}

func TestAttestationDataAtSlot_handlesInProgressRequest(t *testing.T) {
	// Cache toggled by feature flag for now. See https://github.com/prysmaticlabs/prysm/issues/3106.
	featureconfig.InitFeatureConfig(&featureconfig.FeatureFlagConfig{
		EnableAttestationCache: true,
	})
	defer func() {
		featureconfig.InitFeatureConfig(nil)
	}()

	ctx := context.Background()
	server := &AttesterServer{
		cache: cache.NewAttestationCache(),
	}

	req := &pb.AttestationRequest{
		Shard: 1,
		Slot:  2,
	}

	res := &ethpb.AttestationData{
		Target: &ethpb.Checkpoint{Epoch: 55},
	}

	if err := server.cache.MarkInProgress(req); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		response, err := server.RequestAttestation(ctx, req)
		if err != nil {
			t.Error(err)
		}
		if !proto.Equal(res, response) {
			t.Error("Expected  equal responses from cache")
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := server.cache.Put(ctx, req, res); err != nil {
			t.Error(err)
		}
		if err := server.cache.MarkNotInProgress(req); err != nil {
			t.Error(err)
		}
	}()

	wg.Wait()
}
