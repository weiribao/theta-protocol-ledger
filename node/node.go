package node

import (
	"context"
	"log"
	"reflect"
	"sync"

	"github.com/spf13/viper"
	"github.com/thetatoken/theta-protocol-ledger/blockchain"
	"github.com/thetatoken/theta-protocol-ledger/common"
	"github.com/thetatoken/theta-protocol-ledger/consensus"
	"github.com/thetatoken/theta-protocol-ledger/core"
	"github.com/thetatoken/theta-protocol-ledger/crypto"
	dp "github.com/thetatoken/theta-protocol-ledger/dispatcher"
	ld "github.com/thetatoken/theta-protocol-ledger/ledger"
	mp "github.com/thetatoken/theta-protocol-ledger/mempool"
	"github.com/thetatoken/theta-protocol-ledger/netsync"
	"github.com/thetatoken/theta-protocol-ledger/p2p"
	"github.com/thetatoken/theta-protocol-ledger/p2pl"
	rp "github.com/thetatoken/theta-protocol-ledger/report"
	"github.com/thetatoken/theta-protocol-ledger/rpc"
	"github.com/thetatoken/theta-protocol-ledger/snapshot"
	"github.com/thetatoken/theta-protocol-ledger/store"
	"github.com/thetatoken/theta-protocol-ledger/store/database"
	"github.com/thetatoken/theta-protocol-ledger/store/kvstore"
)

type Node struct {
	Store            store.Store
	Chain            *blockchain.Chain
	Consensus        *consensus.ConsensusEngine
	ValidatorManager core.ValidatorManager
	SyncManager      *netsync.SyncManager
	Dispatcher       *dp.Dispatcher
	Ledger           core.Ledger
	Mempool          *mp.Mempool
	RPC              *rpc.ThetaRPCServer
	reporter         *rp.Reporter

	// Life cycle
	wg      *sync.WaitGroup
	quit    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
}

type Params struct {
	ChainID             string
	PrivateKey          *crypto.PrivateKey
	Root                *core.Block
	NetworkOld          p2p.Network
	Network             p2pl.Network
	DB                  database.Database
	SnapshotPath        string
	ChainImportDirPath  string
	ChainCorrectionPath string
}

func NewNode(params *Params) *Node {
	store := kvstore.NewKVStore(params.DB)
	chain := blockchain.NewChain(params.ChainID, store, params.Root)
	validatorManager := consensus.NewRotatingValidatorManager()
	dispatcher := dp.NewDispatcher(params.NetworkOld, params.Network)
	consensus := consensus.NewConsensusEngine(params.PrivateKey, store, chain, dispatcher, validatorManager)
	reporter := rp.NewReporter(dispatcher, consensus, chain)

	// TODO: check if this is a guardian node
	syncMgr := netsync.NewSyncManager(chain, consensus, params.NetworkOld, params.Network, dispatcher, consensus, reporter)
	mempool := mp.CreateMempool(dispatcher)
	ledger := ld.NewLedger(params.ChainID, params.DB, chain, consensus, validatorManager, mempool)

	validatorManager.SetConsensusEngine(consensus)
	consensus.SetLedger(ledger)
	mempool.SetLedger(ledger)
	txMsgHandler := mp.CreateMempoolMessageHandler(mempool)

	if !reflect.ValueOf(params.Network).IsNil() {
		params.Network.RegisterMessageHandler(txMsgHandler)
	}
	if !reflect.ValueOf(params.NetworkOld).IsNil() {
		params.NetworkOld.RegisterMessageHandler(txMsgHandler)
	}

	currentHeight := consensus.GetLastFinalizedBlock().Height
	if currentHeight <= params.Root.Height {
		snapshotPath := params.SnapshotPath
		chainImportDirPath := params.ChainImportDirPath
		chainCorrectionPath := params.ChainCorrectionPath
		var lastCC *core.ExtendedBlock
		var err error
		if _, lastCC, err = snapshot.ImportSnapshot(snapshotPath, chainImportDirPath, chainCorrectionPath, chain, params.DB, ledger); err != nil {
			log.Fatalf("Failed to load snapshot: %v, err: %v", snapshotPath, err)
		}
		if lastCC != nil {
			state := consensus.State()
			state.SetLastFinalizedBlock(lastCC)
			state.SetHighestCCBlock(lastCC)
			state.SetLastVote(core.Vote{})
			state.SetLastProposal(core.Proposal{})
		}
	}

	node := &Node{
		Store:            store,
		Chain:            chain,
		Consensus:        consensus,
		ValidatorManager: validatorManager,
		SyncManager:      syncMgr,
		Dispatcher:       dispatcher,
		Ledger:           ledger,
		Mempool:          mempool,
		reporter:         reporter,
	}

	if viper.GetBool(common.CfgRPCEnabled) {
		node.RPC = rpc.NewThetaRPCServer(mempool, ledger, dispatcher, chain, consensus)
	}
	return node
}

// Start starts sub components and kick off the main loop.
func (n *Node) Start(ctx context.Context) {
	c, cancel := context.WithCancel(ctx)
	n.ctx = c
	n.cancel = cancel

	n.Consensus.Start(n.ctx)
	n.SyncManager.Start(n.ctx)
	n.Dispatcher.Start(n.ctx)
	n.Mempool.Start(n.ctx)
	n.reporter.Start(n.ctx)

	if viper.GetBool(common.CfgRPCEnabled) {
		n.RPC.Start(n.ctx)
	}
}

// Stop notifies all sub components to stop without blocking.
func (n *Node) Stop() {
	n.cancel()
}

// Wait blocks until all sub components stop.
func (n *Node) Wait() {
	n.Consensus.Wait()
	n.SyncManager.Wait()
	if n.RPC != nil {
		n.RPC.Wait()
	}
}
