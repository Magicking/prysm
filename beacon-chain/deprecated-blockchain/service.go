// Package blockchain defines the life-cycle and status of the beacon chain
// as well as the Ethereum Serenity beacon chain fork-choice rule based on
// Casper Proof of Stake finality.
package blockchain

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/prysmaticlabs/go-ssz"
	"github.com/prysmaticlabs/prysm/beacon-chain/attestation"
	"github.com/prysmaticlabs/prysm/beacon-chain/cache/depositcache"
	b "github.com/prysmaticlabs/prysm/beacon-chain/core/blocks"
	"github.com/prysmaticlabs/prysm/beacon-chain/db"
	"github.com/prysmaticlabs/prysm/beacon-chain/operations"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p"
	"github.com/prysmaticlabs/prysm/beacon-chain/powchain"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	ethpb "github.com/prysmaticlabs/prysm/proto/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/event"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

var log = logrus.WithField("prefix", "blockchain")

// ChainFeeds interface defines the methods of the ChainService which provide
// information feeds.
type ChainFeeds interface {
	StateInitializedFeed() *event.Feed
}

// ChainService represents a service that handles the internal
// logic of managing the full PoS beacon chain.
type ChainService struct {
	ctx                  context.Context
	cancel               context.CancelFunc
	beaconDB             db.Database
	depositCache         *depositcache.DepositCache
	web3Service          *powchain.Web3Service
	attsService          attestation.TargetHandler
	opsPoolService       operations.OperationFeeds
	chainStartChan       chan time.Time
	canonicalBlockFeed   *event.Feed
	genesisTime          time.Time
	finalizedEpoch       uint64
	stateInitializedFeed *event.Feed
	p2p                  p2p.Broadcaster
	canonicalRoots       map[uint64][]byte
	canonicalRootsLock   sync.RWMutex
	receiveBlockLock     sync.Mutex
	maxRoutines          int64
	headSlot             uint64
}

// Config options for the service.
type Config struct {
	BeaconBlockBuf int
	Web3Service    *powchain.Web3Service
	AttsService    attestation.TargetHandler
	BeaconDB       db.Database
	DepositCache   *depositcache.DepositCache
	OpsPoolService operations.OperationFeeds
	DevMode        bool
	P2p            p2p.Broadcaster
	MaxRoutines    int64
}

// NewChainService instantiates a new service instance that will
// be registered into a running beacon node.
func NewChainService(ctx context.Context, cfg *Config) (*ChainService, error) {
	ctx, cancel := context.WithCancel(ctx)
	return &ChainService{
		ctx:                  ctx,
		cancel:               cancel,
		beaconDB:             cfg.BeaconDB,
		depositCache:         cfg.DepositCache,
		web3Service:          cfg.Web3Service,
		opsPoolService:       cfg.OpsPoolService,
		attsService:          cfg.AttsService,
		canonicalBlockFeed:   new(event.Feed),
		chainStartChan:       make(chan time.Time),
		stateInitializedFeed: new(event.Feed),
		p2p:                  cfg.P2p,
		canonicalRoots:       make(map[uint64][]byte),
		maxRoutines:          cfg.MaxRoutines,
	}, nil
}

// Start a blockchain service's main event loop.
func (c *ChainService) Start() {
	beaconState, err := c.beaconDB.HeadState(c.ctx)
	if err != nil {
		log.Fatalf("Could not fetch beacon state: %v", err)
	}
	// If the chain has already been initialized, simply start the block processing routine.
	if beaconState != nil {
		log.Info("Beacon chain data already exists, starting service")
		c.genesisTime = time.Unix(int64(beaconState.GenesisTime), 0)
		c.finalizedEpoch = beaconState.FinalizedCheckpoint.Epoch
	} else {
		log.Info("Waiting for ChainStart log from the Validator Deposit Contract to start the beacon chain...")
		if c.web3Service == nil {
			log.Fatal("Not configured web3Service for POW chain")
			return // return need for TestStartUninitializedChainWithoutConfigPOWChain.
		}
		subChainStart := c.web3Service.ChainStartFeed().Subscribe(c.chainStartChan)
		go func() {
			genesisTime := <-c.chainStartChan
			c.processChainStartTime(genesisTime, subChainStart)
			return
		}()
	}
}

// processChainStartTime initializes a series of deposits from the ChainStart deposits in the eth1
// deposit contract, initializes the beacon chain's state, and kicks off the beacon chain.
func (c *ChainService) processChainStartTime(genesisTime time.Time, chainStartSub event.Subscription) {
	initialDeposits := c.web3Service.ChainStartDeposits()
	beaconState, err := c.initializeBeaconChain(genesisTime, initialDeposits, c.web3Service.ChainStartETH1Data())
	if err != nil {
		log.Fatalf("Could not initialize beacon chain: %v", err)
	}
	c.finalizedEpoch = beaconState.FinalizedCheckpoint.Epoch
	c.stateInitializedFeed.Send(genesisTime)
	chainStartSub.Unsubscribe()
}

// initializes the state and genesis block of the beacon chain to persistent storage
// based on a genesis timestamp value obtained from the ChainStart event emitted
// by the ETH1.0 Deposit Contract and the POWChain service of the node.
func (c *ChainService) initializeBeaconChain(genesisTime time.Time, deposits []*ethpb.Deposit, eth1data *ethpb.Eth1Data) (*pb.BeaconState, error) {
	ctx, span := trace.StartSpan(context.Background(), "beacon-chain.ChainService.initializeBeaconChain")
	defer span.End()
	log.Info("ChainStart time reached, starting the beacon chain!")
	c.genesisTime = genesisTime
	unixTime := uint64(genesisTime.Unix())
	// TODO(3219): Use new fork choice service.
	db, isLegacyDB := c.beaconDB.(*db.BeaconDB)
	if !isLegacyDB {
		panic("Chain service cannot operate with the new database interface due to in-progress fork choice changes")
	}

	if err := db.InitializeState(ctx, unixTime, deposits, eth1data); err != nil {
		return nil, errors.Wrap(err, "could not initialize beacon state to disk")
	}
	beaconState, err := c.beaconDB.HeadState(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "could not attempt fetch beacon state")
	}

	stateRoot, err := ssz.HashTreeRoot(beaconState)
	if err != nil {
		return nil, errors.Wrap(err, "could not hash beacon state")
	}
	genBlock := b.NewGenesisBlock(stateRoot[:])
	genBlockRoot, err := ssz.SigningRoot(genBlock)
	if err != nil {
		return nil, errors.Wrap(err, "could not hash beacon block")
	}

	if err := c.beaconDB.SaveBlock(ctx, genBlock); err != nil {
		return nil, errors.Wrap(err, "could not save genesis block to disk")
	}

	if err := db.SaveAttestationTarget(ctx, &pb.AttestationTarget{
		Slot:            genBlock.Slot,
		BeaconBlockRoot: genBlockRoot[:],
		ParentRoot:      genBlock.ParentRoot,
	}); err != nil {
		return nil, errors.Wrap(err, "failed to save attestation target")
	}
	if err := db.UpdateChainHead(ctx, genBlock, beaconState); err != nil {
		return nil, errors.Wrap(err, "could not set chain head")
	}
	if err := db.SaveJustifiedBlock(genBlock); err != nil {
		return nil, errors.Wrap(err, "could not save genesis block as justified block")
	}
	if err := db.SaveFinalizedBlock(genBlock); err != nil {
		return nil, errors.Wrap(err, "could not save genesis block as finalized block")
	}
	if err := db.SaveJustifiedState(beaconState); err != nil {
		return nil, errors.Wrap(err, "could not save genesis state as justified state")
	}
	if err := db.SaveFinalizedState(beaconState); err != nil {
		return nil, errors.Wrap(err, "could not save genesis state as finalized state")
	}
	return beaconState, nil
}

// Stop the blockchain service's main event loop and associated goroutines.
func (c *ChainService) Stop() error {
	defer c.cancel()

	log.Info("Stopping service")
	return nil
}

// Status always returns nil.
// TODO(1202): Add service health checks.
func (c *ChainService) Status() error {
	if runtime.NumGoroutine() > int(c.maxRoutines) {
		return fmt.Errorf("too many goroutines %d", runtime.NumGoroutine())
	}
	return nil
}

// CanonicalBlockFeed returns a channel that is written to
// whenever a new block is determined to be canonical in the chain.
func (c *ChainService) CanonicalBlockFeed() *event.Feed {
	return c.canonicalBlockFeed
}

// StateInitializedFeed returns a feed that is written to
// when the beacon state is first initialized.
func (c *ChainService) StateInitializedFeed() *event.Feed {
	return c.stateInitializedFeed
}

// ChainHeadRoot returns the hash root of the last beacon block processed by the
// block chain service.
func (c *ChainService) ChainHeadRoot(ctx context.Context) ([32]byte, error) {
	head, err := c.beaconDB.HeadBlock(ctx)
	if err != nil {
		return [32]byte{}, errors.Wrap(err, "could not retrieve chain head")
	}

	root, err := ssz.SigningRoot(head)
	if err != nil {
		return [32]byte{}, errors.Wrap(err, "could not tree hash parent block")
	}
	return root, nil
}

// UpdateCanonicalRoots sets a new head into the canonical block roots map.
func (c *ChainService) UpdateCanonicalRoots(newHead *ethpb.BeaconBlock, newHeadRoot [32]byte) {
	c.canonicalRootsLock.Lock()
	defer c.canonicalRootsLock.Unlock()
	c.canonicalRoots[newHead.Slot] = newHeadRoot[:]
}

// IsCanonical returns true if the input block hash of the corresponding slot
// is part of the canonical chain. False otherwise.
func (c *ChainService) IsCanonical(slot uint64, hash []byte) bool {
	c.canonicalRootsLock.RLock()
	defer c.canonicalRootsLock.RUnlock()
	if canonicalHash, ok := c.canonicalRoots[slot]; ok {
		return bytes.Equal(canonicalHash, hash)
	}
	return false
}
