// Package initialsync is run by the beacon node when the local chain is
// behind the network's longest chain. Initial sync works as follows:
// The node requests for the slot number of the most recent finalized block.
// The node then builds from the most recent finalized block by requesting for subsequent
// blocks by slot number. Once the service detects that the local chain is caught up with
// the network, the service hands over control to the regular sync service.
// Note: The behavior of initialsync will likely change as the specification changes.
// The most significant and highly probable change will be determining where to sync from.
// The beacon chain may sync from a block in the pasts X months in order to combat long-range attacks
// (see here: https://github.com/ethereum/wiki/wiki/Proof-of-Stake-FAQs#what-is-weak-subjectivity)
package initialsync

import (
	"context"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	peer "github.com/libp2p/go-libp2p-peer"
	"github.com/pkg/errors"
	"github.com/prysmaticlabs/go-ssz"
	"github.com/prysmaticlabs/prysm/beacon-chain/cache/depositcache"
	"github.com/prysmaticlabs/prysm/beacon-chain/db"
	blockchain "github.com/prysmaticlabs/prysm/beacon-chain/deprecated-blockchain"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	ethpb "github.com/prysmaticlabs/prysm/proto/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	deprecatedp2p "github.com/prysmaticlabs/prysm/shared/deprecated-p2p"
	"github.com/prysmaticlabs/prysm/shared/event"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("prefix", "initial-sync")

var (
	// ErrCanonicalStateMismatch can occur when the node has processed all blocks
	// from a peer, but arrived at a different state root.
	ErrCanonicalStateMismatch = errors.New("canonical state did not match after syncing with peer")
)

// Config defines the configurable properties of InitialSync.
//
type Config struct {
	SyncPollingInterval    time.Duration
	BatchedBlockBufferSize int
	StateBufferSize        int
	BeaconDB               *db.BeaconDB
	DepositCache           *depositcache.DepositCache
	P2P                    p2pAPI
	SyncService            syncService
	ChainService           chainService
	PowChain               powChainService
}

// DefaultConfig provides the default configuration for a sync service.
// SyncPollingInterval determines how frequently the service checks that initial sync is complete.
// BlockBufferSize determines that buffer size of the `blockBuf` channel.
// StateBufferSize determines the buffer size of the `stateBuf` channel.
func DefaultConfig() *Config {
	return &Config{
		SyncPollingInterval:    time.Duration(params.BeaconConfig().SyncPollingInterval) * time.Second,
		BatchedBlockBufferSize: params.BeaconConfig().DefaultBufferSize,
		StateBufferSize:        params.BeaconConfig().DefaultBufferSize,
	}
}

type p2pAPI interface {
	p2p.Sender
	p2p.DeprecatedSubscriber
}

type powChainService interface {
	BlockExists(ctx context.Context, hash common.Hash) (bool, *big.Int, error)
}

type chainService interface {
	blockchain.BlockProcessor
	blockchain.ForkChoice
}

// SyncService is the interface for the Sync service.
// InitialSync calls `Start` when initial sync completes.
type syncService interface {
	Start()
	ResumeSync()
}

// InitialSync defines the main class in this package.
// See the package comments for a general description of the service's functions.
type InitialSync struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	p2p                 p2pAPI
	syncService         syncService
	chainService        chainService
	db                  *db.BeaconDB
	depositCache        *depositcache.DepositCache
	powchain            powChainService
	batchedBlockBuf     chan deprecatedp2p.Message
	stateBuf            chan deprecatedp2p.Message
	syncPollingInterval time.Duration
	syncedFeed          *event.Feed
	stateReceived       bool
	mutex               *sync.Mutex
	nodeIsSynced        bool
}

// NewInitialSyncService constructs a new InitialSyncService.
// This method is normally called by the main node.
func NewInitialSyncService(ctx context.Context,
	cfg *Config,
) *InitialSync {
	ctx, cancel := context.WithCancel(ctx)

	stateBuf := make(chan deprecatedp2p.Message, cfg.StateBufferSize)
	batchedBlockBuf := make(chan deprecatedp2p.Message, cfg.BatchedBlockBufferSize)

	return &InitialSync{
		ctx:                 ctx,
		cancel:              cancel,
		p2p:                 cfg.P2P,
		syncService:         cfg.SyncService,
		db:                  cfg.BeaconDB,
		depositCache:        cfg.DepositCache,
		powchain:            cfg.PowChain,
		chainService:        cfg.ChainService,
		stateBuf:            stateBuf,
		batchedBlockBuf:     batchedBlockBuf,
		syncPollingInterval: cfg.SyncPollingInterval,
		syncedFeed:          new(event.Feed),
		stateReceived:       false,
		mutex:               new(sync.Mutex),
	}
}

// Start begins the goroutine.
func (s *InitialSync) Start(chainHeadResponses map[peer.ID]*pb.ChainHeadResponse) {
	go s.run(chainHeadResponses)
}

// Stop kills the initial sync goroutine.
func (s *InitialSync) Stop() error {
	log.Info("Stopping service")
	s.cancel()
	return nil
}

// NodeIsSynced checks that the node has been caught up with the network.
func (s *InitialSync) NodeIsSynced() bool {
	return s.nodeIsSynced
}

func (s *InitialSync) exitInitialSync(ctx context.Context, block *ethpb.BeaconBlock, chainHead *pb.ChainHeadResponse) error {
	if s.nodeIsSynced {
		return nil
	}
	parentRoot := bytesutil.ToBytes32(block.ParentRoot)
	parent, err := s.db.BlockDeprecated(parentRoot)
	if err != nil {
		return err
	}
	state, err := s.db.HistoricalStateFromSlot(ctx, parent.Slot, parentRoot)
	if err != nil {
		return err
	}
	if err := s.chainService.VerifyBlockValidity(ctx, block, state); err != nil {
		return err
	}
	if err := s.db.SaveBlockDeprecated(block); err != nil {
		return err
	}
	root, err := ssz.SigningRoot(block)
	if err != nil {
		return errors.Wrap(err, "failed to tree hash block")
	}
	if err := s.db.SaveAttestationTarget(ctx, &pb.AttestationTarget{
		Slot:            block.Slot,
		BeaconBlockRoot: root[:],
		ParentRoot:      block.ParentRoot,
	}); err != nil {
		return errors.Wrap(err, "failed to save attestation target")
	}
	state, err = s.chainService.AdvanceStateDeprecated(ctx, state, block)
	if err != nil {
		log.Error("OH NO - looks like you synced with a bad peer, try restarting your node!")
		switch err.(type) {
		case *blockchain.BlockFailedProcessingErr:
			// If the block fails processing, we delete it from our DB.
			if err := s.db.DeleteBlockDeprecated(block); err != nil {
				return errors.Wrap(err, "could not delete bad block from db")
			}
			return errors.Wrap(err, "could not apply block state transition")
		default:
			return errors.Wrap(err, "could not apply block state transition")
		}
	}
	if err := s.chainService.CleanupBlockOperations(ctx, block); err != nil {
		return err
	}
	if err := s.db.UpdateChainHead(ctx, block, state); err != nil {
		return err
	}

	stateRoot := s.db.HeadStateRoot()

	if stateRoot != bytesutil.ToBytes32(chainHead.CanonicalStateRootHash32) {
		log.Errorf(
			"Canonical state root %#x does not match highest observed root from peer %#x",
			stateRoot,
			chainHead.CanonicalStateRootHash32,
		)

		return ErrCanonicalStateMismatch
	}
	log.WithField("canonicalStateSlot", state.Slot).Info("Exiting init sync and starting regular sync")
	s.syncService.ResumeSync()
	s.cancel()
	s.nodeIsSynced = true
	return nil
}

// run is the main goroutine for the initial sync service.
// delayChan is explicitly passed into this function to facilitate tests that don't require a timeout.
// It is assumed that the goroutine `run` is only called once per instance.
func (s *InitialSync) run(chainHeadResponses map[peer.ID]*pb.ChainHeadResponse) {
	batchedBlocksub := s.p2p.Subscribe(&pb.BatchedBeaconBlockResponse{}, s.batchedBlockBuf)
	beaconStateSub := s.p2p.Subscribe(&pb.BeaconStateResponse{}, s.stateBuf)
	defer func() {
		beaconStateSub.Unsubscribe()
		batchedBlocksub.Unsubscribe()
		close(s.batchedBlockBuf)
		close(s.stateBuf)
	}()

	ctx := s.ctx

	var peers []peer.ID
	for k := range chainHeadResponses {
		peers = append(peers, k)
	}

	// Sort peers in descending order based on their canonical slot.
	sort.Slice(peers, func(i, j int) bool {
		return chainHeadResponses[peers[i]].CanonicalSlot > chainHeadResponses[peers[j]].CanonicalSlot
	})

	for _, peer := range peers {
		chainHead := chainHeadResponses[peer]
		if err := s.syncToPeer(ctx, chainHead, peer); err != nil {
			log.WithError(err).WithField("peer", peer.Pretty()).Warn("Failed to sync with peer, trying next best peer")
			continue
		}
		log.Info("Synced!")
		break
	}

	if !s.nodeIsSynced {
		log.Fatal("Failed to sync with anyone...")
	}
}

func (s *InitialSync) syncToPeer(ctx context.Context, chainHeadResponse *pb.ChainHeadResponse, peer peer.ID) error {
	fields := logrus.Fields{
		"peer":          peer.Pretty(),
		"canonicalSlot": chainHeadResponse.CanonicalSlot,
	}

	log.WithFields(fields).Info("Requesting state from peer")
	if err := s.requestStateFromPeer(ctx, bytesutil.ToBytes32(chainHeadResponse.FinalizedStateRootHash32S), peer); err != nil {
		log.Errorf("Could not request state from peer %v", err)
	}

	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():

			return ctx.Err()
		case msg := <-s.stateBuf:
			log.WithFields(fields).Info("Received state resp from peer")
			if err := s.processState(msg, chainHeadResponse); err != nil {
				return err
			}
		case msg := <-s.batchedBlockBuf:
			if msg.Peer != peer {
				continue
			}
			log.WithFields(fields).Info("Received batched blocks from peer")
			if err := s.processBatchedBlocks(msg, chainHeadResponse); err != nil {
				log.WithError(err).WithField("peer", peer).Error("Failed to sync with peer.")
				continue
			}
			if !s.nodeIsSynced {
				return errors.New("node still not in sync after receiving batch blocks")
			}
			return nil
		}
	}
}
