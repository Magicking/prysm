package db

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	ethpb "github.com/prysmaticlabs/prysm/proto/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/testutil"
)

func init() {
	featureconfig.InitFeatureConfig(&featureconfig.FeatureFlagConfig{
		DisableHistoricalStatePruning: false,
	})
}

func TestInitializeState_OK(t *testing.T) {
	db := setupDB(t)
	defer teardownDB(t, db)
	ctx := context.Background()

	genesisTime := uint64(time.Now().Unix())
	deposits, _ := testutil.SetupInitialDeposits(t, 10)
	if err := db.InitializeState(context.Background(), genesisTime, deposits, &ethpb.Eth1Data{}); err != nil {
		t.Fatalf("Failed to initialize state: %v", err)
	}
	b, err := db.ChainHead()
	if err != nil {
		t.Fatalf("Failed to get chain head: %v", err)
	}
	if b.GetSlot() != 0 {
		t.Fatalf("Expected block height to equal 1. Got %d", b.GetSlot())
	}

	beaconState, err := db.HeadState(ctx)
	if err != nil {
		t.Fatalf("Failed to get state: %v", err)
	}
	if beaconState == nil {
		t.Fatalf("Failed to retrieve state: %v", beaconState)
	}
	beaconStateEnc, err := proto.Marshal(beaconState)
	if err != nil {
		t.Fatalf("Failed to encode state: %v", err)
	}

	statePrime, err := db.HeadState(ctx)
	if err != nil {
		t.Fatalf("Failed to get state: %v", err)
	}
	statePrimeEnc, err := proto.Marshal(statePrime)
	if err != nil {
		t.Fatalf("Failed to encode state: %v", err)
	}

	if !bytes.Equal(beaconStateEnc, statePrimeEnc) {
		t.Fatalf("Expected %#x and %#x to be equal", beaconStateEnc, statePrimeEnc)
	}
}

func TestFinalizeState_OK(t *testing.T) {
	db := setupDB(t)
	defer teardownDB(t, db)

	genesisTime := uint64(time.Now().Unix())
	deposits, _ := testutil.SetupInitialDeposits(t, 20)
	if err := db.InitializeState(context.Background(), genesisTime, deposits, &ethpb.Eth1Data{}); err != nil {
		t.Fatalf("Failed to initialize state: %v", err)
	}

	state, err := db.HeadState(context.Background())
	if err != nil {
		t.Fatalf("Failed to retrieve state: %v", err)
	}

	if err := db.SaveFinalizedState(state); err != nil {
		t.Fatalf("Unable to save finalized state")
	}

	fState, err := db.FinalizedState()
	if err != nil {
		t.Fatalf("Unable to retrieve finalized state")
	}

	if !proto.Equal(fState, state) {
		t.Error("Retrieved and saved finalized are unequal")
	}
}

func BenchmarkState_ReadingFromCache(b *testing.B) {
	db := setupDB(b)
	defer teardownDB(b, db)
	ctx := context.Background()

	genesisTime := uint64(time.Now().Unix())
	deposits, _ := testutil.SetupInitialDeposits(b, 10)
	if err := db.InitializeState(context.Background(), genesisTime, deposits, &ethpb.Eth1Data{}); err != nil {
		b.Fatalf("Failed to initialize state: %v", err)
	}

	state, err := db.HeadState(ctx)
	if err != nil {
		b.Fatalf("Could not read DV beacon state from DB: %v", err)
	}
	state.Slot++
	err = db.SaveStateDeprecated(ctx, state)
	if err != nil {
		b.Fatalf("Could not save beacon state to cache from DB: %v", err)
	}

	savedState := &pb.BeaconState{}
	savedState.Unmarshal(db.serializedState)

	if savedState.Slot != 1 {
		b.Fatal("cache should be prepared on state after saving to DB")
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := db.HeadState(ctx)
		if err != nil {
			b.Fatalf("Could not read beacon state from cache: %v", err)
		}
	}
}

func TestFinalizedState_NoneExists(t *testing.T) {
	db := setupDB(t)
	defer teardownDB(t, db)
	wanted := "no finalized state saved"
	_, err := db.FinalizedState()
	if !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected: %s, received: %s", wanted, err.Error())
	}
}

func TestJustifiedState_CanSaveRetrieve(t *testing.T) {
	db := setupDB(t)
	defer teardownDB(t, db)

	stateSlot := uint64(10)
	state := &pb.BeaconState{
		Slot: stateSlot,
	}

	if err := db.SaveJustifiedState(state); err != nil {
		t.Fatalf("could not save justified state: %v", err)
	}

	justifiedState, err := db.JustifiedState()
	if err != nil {
		t.Fatalf("could not get justified state: %v", err)
	}
	if justifiedState.Slot != stateSlot {
		t.Errorf("Saved state does not have the slot from which it was requested, wanted: %d, got: %d",
			stateSlot, justifiedState.Slot)
	}
}

func TestJustifiedState_NoneExists(t *testing.T) {
	db := setupDB(t)
	defer teardownDB(t, db)
	wanted := "no justified state saved"
	_, err := db.JustifiedState()
	if !strings.Contains(err.Error(), wanted) {
		t.Errorf("Expected: %s, received: %s", wanted, err.Error())
	}
}

func TestFinalizedState_CanSaveRetrieve(t *testing.T) {
	db := setupDB(t)
	defer teardownDB(t, db)

	stateSlot := uint64(10)
	state := &pb.BeaconState{
		Slot: stateSlot,
	}

	if err := db.SaveFinalizedState(state); err != nil {
		t.Fatalf("could not save finalized state: %v", err)
	}

	finalizedState, err := db.FinalizedState()
	if err != nil {
		t.Fatalf("could not get finalized state: %v", err)
	}
	if finalizedState.Slot != stateSlot {
		t.Errorf("Saved state does not have the slot from which it was requested, wanted: %d, got: %d",
			stateSlot, finalizedState.Slot)
	}
}

func TestHistoricalState_CanSaveRetrieve(t *testing.T) {
	db := setupDB(t)
	defer teardownDB(t, db)
	ctx := context.Background()

	tests := []struct {
		state *pb.BeaconState
	}{
		{
			state: &pb.BeaconState{
				Slot:                66,
				FinalizedCheckpoint: &ethpb.Checkpoint{Epoch: 1},
			},
		},
		{
			state: &pb.BeaconState{
				Slot:                72,
				FinalizedCheckpoint: &ethpb.Checkpoint{Epoch: 1},
			},
		},
		{
			state: &pb.BeaconState{
				Slot:                96,
				FinalizedCheckpoint: &ethpb.Checkpoint{Epoch: 1},
			},
		},
		{
			state: &pb.BeaconState{
				Slot:                130,
				FinalizedCheckpoint: &ethpb.Checkpoint{Epoch: 2},
			},
		},
		{
			state: &pb.BeaconState{
				Slot:                300,
				FinalizedCheckpoint: &ethpb.Checkpoint{Epoch: 4},
			},
		},
	}

	for _, tt := range tests {
		if err := db.SaveFinalizedState(tt.state); err != nil {
			t.Fatalf("could not save finalized state: %v", err)
		}
		if err := db.SaveHistoricalState(context.Background(), tt.state, [32]byte{}); err != nil {
			t.Fatalf("could not save historical state: %v", err)
		}

		retState, err := db.HistoricalStateFromSlot(ctx, tt.state.Slot, [32]byte{})
		if err != nil {
			t.Fatalf("Unable to retrieve state %v", err)
		}

		if !proto.Equal(tt.state, retState) {
			t.Errorf("Saved and retrieved states are not equal got\n %v but wanted\n %v", proto.MarshalTextString(retState), proto.MarshalTextString(tt.state))
		}
	}
}

func TestHistoricalState_Pruning(t *testing.T) {
	db := setupDB(t)
	defer teardownDB(t, db)
	ctx := context.Background()

	epochSize := params.BeaconConfig().SlotsPerEpoch

	tests := []struct {
		histState1 *pb.BeaconState
		histState2 *pb.BeaconState
	}{
		{
			histState1: &pb.BeaconState{
				Slot: 2 * epochSize,
			},
			histState2: &pb.BeaconState{
				Slot: 3 * epochSize,
			},
		},
		{
			histState1: &pb.BeaconState{
				Slot: 1 * epochSize,
			},
			histState2: &pb.BeaconState{
				Slot: 4 * epochSize,
			},
		},
		{
			histState1: &pb.BeaconState{
				Slot: 2 * epochSize,
			},
			histState2: &pb.BeaconState{
				Slot: 5 * epochSize,
			},
		},
		{
			histState1: &pb.BeaconState{
				Slot: 6 * epochSize,
			},
			histState2: &pb.BeaconState{
				Slot: 14 * epochSize,
			},
		},
		{
			histState1: &pb.BeaconState{
				Slot: 12 * epochSize,
			},
			histState2: &pb.BeaconState{
				Slot: 103 * epochSize,
			},
		},
		{
			histState1: &pb.BeaconState{
				Slot: 100 * epochSize,
			},
			histState2: &pb.BeaconState{
				Slot: 600 * epochSize,
			},
		},
	}

	for _, tt := range tests {
		root := [32]byte{}
		if err := db.SaveHistoricalState(context.Background(), tt.histState1, root); err != nil {
			t.Fatalf("could not save historical state: %v", err)
		}
		if err := db.SaveHistoricalState(context.Background(), tt.histState2, root); err != nil {
			t.Fatalf("could not save historical state: %v", err)
		}

		// Delete up to and including historical state 1.
		if err := db.deleteHistoricalStates(tt.histState1.Slot + 1); err != nil {
			t.Fatalf("Could not delete historical states %v", err)
		}

		// Save a dummy genesis state so that db doesnt return an error.
		if err := db.SaveHistoricalState(context.Background(), &pb.BeaconState{Slot: 0, FinalizedCheckpoint: &ethpb.Checkpoint{}}, root); err != nil {
			t.Fatalf("could not save historical state: %v", err)
		}

		retState, err := db.HistoricalStateFromSlot(ctx, tt.histState1.Slot, root)
		if err != nil {
			t.Errorf("Unable to retrieve state %v", err)
		}

		if proto.Equal(tt.histState1, retState) {
			t.Errorf("Saved and retrieved states are equal when they supposed to be different %d", tt.histState1.Slot)
		}

		retState, err = db.HistoricalStateFromSlot(ctx, tt.histState2.Slot, root)
		if err != nil {
			t.Errorf("Unable to retrieve state %v", err)
		}

		if !proto.Equal(tt.histState2, retState) {
			t.Errorf("Saved and retrieved states are not equal when they supposed to be for slot %d", tt.histState2.Slot)
		}

	}
}
