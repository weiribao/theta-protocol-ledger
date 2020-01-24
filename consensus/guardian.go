package consensus

import (
	log "github.com/sirupsen/logrus"
	"github.com/thetatoken/theta-protocol-ledger/common"
	"github.com/thetatoken/theta-protocol-ledger/common/util"
	"github.com/thetatoken/theta-protocol-ledger/core"
	"github.com/thetatoken/theta-protocol-ledger/crypto/bls"
)

const (
	maxLogNeighbors uint32 = 3 // Estimated number of neighbors during gossip = 2**3 = 8
	maxRound               = 10
)

type GuardianEngine struct {
	logger *log.Entry

	engine  *ConsensusEngine
	privKey *bls.SecretKey

	// State for current voting
	block       common.Hash
	round       uint32
	currVote    *core.AggregatedVotes
	nextVote    *core.AggregatedVotes
	gcp         *core.GuardianCandidatePool
	gcpHash     common.Hash
	signerIndex int // Signer's index in current gcp
}

func NewGuardianEngine(c *ConsensusEngine, privateKey *bls.SecretKey) *GuardianEngine {
	return &GuardianEngine{
		logger:  util.GetLoggerForModule("guardian"),
		engine:  c,
		privKey: privateKey,
	}
}

func (g *GuardianEngine) IsGuardian() bool {
	return g.signerIndex >= 0
}

func (g *GuardianEngine) StartNewBlock(block common.Hash) {
	g.block = block
	g.nextVote = nil
	g.currVote = nil
	g.round = 1

	gcp, err := g.engine.GetLedger().GetGuardianCandidatePool(block)
	if err != nil {
		// Should not happen
		g.logger.Panic(err)
	}
	g.gcp = gcp
	g.gcpHash = gcp.Hash()
	g.signerIndex = gcp.WithStake().Index(g.privKey.PublicKey())

	g.logger.WithFields(log.Fields{
		"block":       block.Hex(),
		"gcp":         g.gcpHash.Hex(),
		"signerIndex": g.signerIndex,
	}).Debug("Starting new block")

	if g.IsGuardian() {
		g.nextVote = core.NewAggregateVotes(block, gcp)
		g.nextVote.Sign(g.privKey, g.signerIndex)
		g.currVote = g.nextVote.Copy()
	} else {
		g.nextVote = nil
		g.currVote = nil
	}

}

func (g *GuardianEngine) StartNewRound() {
	if g.round < maxRound {
		g.round++
		g.currVote = g.nextVote.Copy()
	}
}

func (g *GuardianEngine) GetVoteToBroadcast() *core.AggregatedVotes {
	return g.currVote
}

func (g *GuardianEngine) GetBestVote() *core.AggregatedVotes {
	return g.nextVote
}

func (g *GuardianEngine) HandleVote(vote *core.AggregatedVotes) {
	if !g.validateVote(vote) {
		return
	}

	if g.nextVote == nil {
		g.nextVote = vote
		return
	}

	mergedVote, err := g.nextVote.Merge(vote)
	if err != nil {
		g.logger.WithFields(log.Fields{
			"g.block":               g.block.Hex(),
			"g.round":               g.round,
			"vote.block":            vote.Block.Hex(),
			"vote.Mutiplies":        vote.Multiplies,
			"vote.GCP":              vote.Gcp.Hex(),
			"g.nextVote.Multiplies": g.nextVote.Multiplies,
			"g.nextVote.GCP":        g.nextVote.Gcp.Hex(),
			"g.nextVote.Block":      g.nextVote.Block.Hex(),
			"error":                 err.Error(),
		}).Info("Failed to merge guardian vote")
	}
	if mergedVote == nil {
		// Incoming vote is subset of the current nextVote.
		return
	}
	if !g.checkMultipliesForRound(mergedVote, g.round+1) {
		g.logger.WithFields(log.Fields{
			"local.block":           g.block.Hex(),
			"local.round":           g.round,
			"vote.block":            vote.Block.Hex(),
			"vote.Mutiplies":        vote.Multiplies,
			"local.vote.Multiplies": g.nextVote.Multiplies,
		}).Info("Skipping vote: merged vote overflows")
		return
	}

	g.nextVote = mergedVote

	g.logger.WithFields(log.Fields{
		"local.block":           g.block.Hex(),
		"local.round":           g.round,
		"local.vote.Multiplies": g.nextVote.Multiplies,
	}).Info("Merged guardian vote")
}

func (g *GuardianEngine) validateVote(vote *core.AggregatedVotes) (res bool) {
	if g.block.IsEmpty() {
		g.logger.WithFields(log.Fields{
			"local.block":    g.block.Hex(),
			"local.round":    g.round,
			"vote.block":     vote.Block.Hex(),
			"vote.Mutiplies": vote.Multiplies,
		}).Info("Ignoring guardian vote: local not ready")
		return
	}
	if vote.Block != g.block {
		g.logger.WithFields(log.Fields{
			"local.block":    g.block.Hex(),
			"local.round":    g.round,
			"vote.block":     vote.Block.Hex(),
			"vote.Mutiplies": vote.Multiplies,
		}).Info("Ignoring guardian vote: block hash does not match with local candidate")
		return
	}
	if vote.Gcp != g.gcpHash {
		g.logger.WithFields(log.Fields{
			"local.block":    g.block.Hex(),
			"local.round":    g.round,
			"vote.block":     vote.Block.Hex(),
			"vote.Mutiplies": vote.Multiplies,
			"vote.gcp":       vote.Gcp.Hex(),
			"local.gcp":      g.gcpHash.Hex(),
		}).Info("Ignoring guardian vote: gcp hash does not match with local value")
		return
	}
	if !g.checkMultipliesForRound(vote, g.round) {
		g.logger.WithFields(log.Fields{
			"local.block":    g.block.Hex(),
			"local.round":    g.round,
			"vote.block":     vote.Block.Hex(),
			"vote.Mutiplies": vote.Multiplies,
			"vote.gcp":       vote.Gcp.Hex(),
			"local.gcp":      g.gcpHash.Hex(),
		}).Info("Ignoring guardian vote: mutiplies exceed limit for round")
		return
	}
	if result := vote.Validate(g.gcp); result.IsError() {
		g.logger.WithFields(log.Fields{
			"local.block":    g.block.Hex(),
			"local.round":    g.round,
			"vote.block":     vote.Block.Hex(),
			"vote.Mutiplies": vote.Multiplies,
			"vote.gcp":       vote.Gcp.Hex(),
			"local.gcp":      g.gcpHash.Hex(),
			"error":          result.Message,
		}).Info("Ignoring guardian vote: invalid vote")
		return
	}
	res = true
	return
}

func (g *GuardianEngine) checkMultipliesForRound(vote *core.AggregatedVotes, k uint32) bool {
	// for _, m := range vote.Multiplies {
	// 	if m > g.maxMultiply(k) {
	// 		return false
	// 	}
	// }
	return true
}

func (g *GuardianEngine) maxMultiply(k uint32) uint32 {
	return 1 << (k * maxLogNeighbors)
}
