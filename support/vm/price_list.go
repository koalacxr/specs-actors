package vm

import (
	"fmt"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/specs-actors/v7/actors/builtin"
	"github.com/filecoin-project/specs-actors/v7/actors/runtime/proof"
)

type Pricelist interface {
	// OnChainMessage returns the gas used for storing a message of a given size in the chain.
	OnChainMessage(msgSize int) GasCharge
	// OnChainReturnValue returns the gas used for storing the response of a message in the chain.
	OnChainReturnValue(dataSize int) GasCharge

	// OnMethodInvocation returns the gas used when invoking a method.
	OnMethodInvocation(value abi.TokenAmount, methodNum abi.MethodNum) GasCharge

	// OnIpldGet returns the gas used for storing an object
	OnIpldGet() GasCharge
	// OnIpldPut returns the gas used for storing an object
	OnIpldPut(dataSize int) GasCharge

	// OnCreateActor returns the gas used for creating an actor
	OnCreateActor() GasCharge
	// OnDeleteActor returns the gas used for deleting an actor
	OnDeleteActor() GasCharge

	OnVerifySignature(sigType crypto.SigType, planTextSize int) (GasCharge, error)
	OnHashing(dataSize int) GasCharge
	OnComputeUnsealedSectorCid(proofType abi.RegisteredSealProof, pieces []abi.PieceInfo) GasCharge
	OnVerifySeal(info proof.SealVerifyInfo) GasCharge
	OnVerifyPost(info proof.WindowPoStVerifyInfo) GasCharge
	OnVerifyConsensusFault() GasCharge
}

type GasCharge struct {
	Name  string
	Extra interface{}

	ComputeGas int64
	StorageGas int64

	VirtualCompute int64
	VirtualStorage int64
}

func (g GasCharge) Total() int64 {
	return g.ComputeGas + g.StorageGas
}
func (g GasCharge) WithVirtual(compute, storage int64) GasCharge {
	out := g
	out.VirtualCompute = compute
	out.VirtualStorage = storage
	return out
}

func (g GasCharge) WithExtra(extra interface{}) GasCharge {
	out := g
	out.Extra = extra
	return out
}

func newGasCharge(name string, computeGas int64, storageGas int64) GasCharge {
	return GasCharge{
		Name:       name,
		ComputeGas: computeGas,
		StorageGas: storageGas,
	}
}

type scalingCost struct {
	flat  int64
	scale int64
}

type pricelist struct {
	computeGasMulti int64
	storageGasMulti int64
	///////////////////////////////////////////////////////////////////////////
	// System operations
	///////////////////////////////////////////////////////////////////////////

	// Gas cost charged to the originator of an on-chain message (regardless of
	// whether it succeeds or fails in application) is given by:
	//   OnChainMessageBase + len(serialized message)*OnChainMessagePerByte
	// Together, these account for the cost of message propagation and validation,
	// up to but excluding any actual processing by the VM.
	// This is the cost a block producer burns when including an invalid message.
	onChainMessageComputeBase    int64
	onChainMessageStorageBase    int64
	onChainMessageStoragePerByte int64

	// Gas cost charged to the originator of a non-nil return value produced
	// by an on-chain message is given by:
	//   len(return value)*OnChainReturnValuePerByte
	onChainReturnValuePerByte int64

	// Gas cost for any message send execution(including the top-level one
	// initiated by an on-chain message).
	// This accounts for the cost of loading sender and receiver actors and
	// (for top-level messages) incrementing the sender's sequence number.
	// Load and store of actor sub-state is charged separately.
	sendBase int64

	// Gas cost charged, in addition to SendBase, if a message send
	// is accompanied by any nonzero currency amount.
	// Accounts for writing receiver's new balance (the sender's state is
	// already accounted for).
	sendTransferFunds int64

	// Gsa cost charged, in addition to SendBase, if message only transfers funds.
	sendTransferOnlyPremium int64

	// Gas cost charged, in addition to SendBase, if a message invokes
	// a method on the receiver.
	// Accounts for the cost of loading receiver code and method dispatch.
	sendInvokeMethod int64

	// Gas cost for any Get operation to the IPLD store
	// in the runtime VM context.
	ipldGetBase int64

	// Gas cost (Base + len*PerByte) for any Put operation to the IPLD store
	// in the runtime VM context.
	//
	// Note: these costs should be significantly higher than the costs for Get
	// operations, since they reflect not only serialization/deserialization
	// but also persistent storage of chain data.
	ipldPutBase    int64
	ipldPutPerByte int64

	// Gas cost for creating a new actor (via InitActor's Exec method).
	//
	// Note: this costs assume that the extra will be partially or totally refunded while
	// the base is covering for the put.
	createActorCompute int64
	createActorStorage int64

	// Gas cost for deleting an actor.
	//
	// Note: this partially refunds the create cost to incentivise the deletion of the actors.
	deleteActor int64

	verifySignature map[crypto.SigType]int64

	hashingBase int64

	computeUnsealedSectorCidBase int64
	verifySealBase               int64
	verifyPostLookup             map[abi.RegisteredPoStProof]scalingCost
	verifyPostDiscount           bool
	verifyConsensusFault         int64
}

var _ Pricelist = (*pricelist)(nil)

// OnChainMessage returns the gas used for storing a message of a given size in the chain.
func (pl *pricelist) OnChainMessage(msgSize int) GasCharge {
	return newGasCharge("OnChainMessage", pl.onChainMessageComputeBase,
		(pl.onChainMessageStorageBase+pl.onChainMessageStoragePerByte*int64(msgSize))*pl.storageGasMulti)
}

// OnChainReturnValue returns the gas used for storing the response of a message in the chain.
func (pl *pricelist) OnChainReturnValue(dataSize int) GasCharge {
	return newGasCharge("OnChainReturnValue", 0, int64(dataSize)*pl.onChainReturnValuePerByte*pl.storageGasMulti)
}

// OnMethodInvocation returns the gas used when invoking a method.
func (pl *pricelist) OnMethodInvocation(value abi.TokenAmount, methodNum abi.MethodNum) GasCharge {
	ret := pl.sendBase
	extra := ""

	if big.Cmp(value, abi.NewTokenAmount(0)) != 0 {
		ret += pl.sendTransferFunds
		if methodNum == builtin.MethodSend {
			// transfer only
			ret += pl.sendTransferOnlyPremium
		}
		extra += "t"
	}

	if methodNum != builtin.MethodSend {
		extra += "i"
		// running actors is cheaper becase we hand over to actors
		ret += pl.sendInvokeMethod
	}
	return newGasCharge("OnMethodInvocation", ret, 0).WithExtra(extra)
}

// OnIpldGet returns the gas used for storing an object
func (pl *pricelist) OnIpldGet() GasCharge {
	return newGasCharge("OnIpldGet", pl.ipldGetBase, 0).WithVirtual(114617, 0)
}

// OnIpldPut returns the gas used for storing an object
func (pl *pricelist) OnIpldPut(dataSize int) GasCharge {
	return newGasCharge("OnIpldPut", pl.ipldPutBase, int64(dataSize)*pl.ipldPutPerByte*pl.storageGasMulti).
		WithExtra(dataSize).WithVirtual(400000, int64(dataSize)*1300)
}

// OnCreateActor returns the gas used for creating an actor
func (pl *pricelist) OnCreateActor() GasCharge {
	return newGasCharge("OnCreateActor", pl.createActorCompute, pl.createActorStorage*pl.storageGasMulti)
}

// OnDeleteActor returns the gas used for deleting an actor
func (pl *pricelist) OnDeleteActor() GasCharge {
	return newGasCharge("OnDeleteActor", 0, pl.deleteActor*pl.storageGasMulti)
}

// OnVerifySignature

func (pl *pricelist) OnVerifySignature(sigType crypto.SigType, planTextSize int) (GasCharge, error) {
	cost, ok := pl.verifySignature[sigType]
	if !ok {
		return GasCharge{}, fmt.Errorf("cost function for signature type %d not supported", sigType)
	}

	sigName, _ := sigType.Name()
	return newGasCharge("OnVerifySignature", cost, 0).
		WithExtra(map[string]interface{}{
			"type": sigName,
			"size": planTextSize,
		}), nil
}

// OnHashing
func (pl *pricelist) OnHashing(dataSize int) GasCharge {
	return newGasCharge("OnHashing", pl.hashingBase, 0).WithExtra(dataSize)
}

// OnComputeUnsealedSectorCid
func (pl *pricelist) OnComputeUnsealedSectorCid(proofType abi.RegisteredSealProof, pieces []abi.PieceInfo) GasCharge {
	return newGasCharge("OnComputeUnsealedSectorCid", pl.computeUnsealedSectorCidBase, 0)
}

// OnVerifySeal
func (pl *pricelist) OnVerifySeal(info proof.SealVerifyInfo) GasCharge {
	// TODO: this needs more cost tunning, check with @lotus
	// this is not used
	return newGasCharge("OnVerifySeal", pl.verifySealBase, 0)
}

// OnVerifyPost
func (pl *pricelist) OnVerifyPost(info proof.WindowPoStVerifyInfo) GasCharge {
	sectorSize := "unknown"
	var proofType abi.RegisteredPoStProof

	if len(info.Proofs) != 0 {
		proofType = info.Proofs[0].PoStProof
		ss, err := info.Proofs[0].PoStProof.SectorSize()
		if err == nil {
			sectorSize = ss.ShortString()
		}
	}

	cost, ok := pl.verifyPostLookup[proofType]
	if !ok {
		cost = pl.verifyPostLookup[abi.RegisteredPoStProof_StackedDrgWindow512MiBV1]
	}

	gasUsed := cost.flat + int64(len(info.ChallengedSectors))*cost.scale
	if pl.verifyPostDiscount {
		gasUsed /= 2 // XXX: this is an artificial discount
	}

	return newGasCharge("OnVerifyPost", gasUsed, 0).
		WithVirtual(117680921+43780*int64(len(info.ChallengedSectors)), 0).
		WithExtra(map[string]interface{}{
			"type": sectorSize,
			"size": len(info.ChallengedSectors),
		})
}

// OnVerifyConsensusFault
func (pl *pricelist) OnVerifyConsensusFault() GasCharge {
	return newGasCharge("OnVerifyConsensusFault", pl.verifyConsensusFault, 0)
}

// gas prices as of filecoin v13
// Note this should be updated to latest next upgrade pricelist before conformance
// test vector generation to ensure it is up to date with latest protocol.
// Source of truth here: https://github.com/filecoin-project/lotus/blob/master/chain/vm/gas.go#L82
var v13PriceList = pricelist{
	computeGasMulti: 1,
	storageGasMulti: 1300,

	onChainMessageComputeBase:    38863,
	onChainMessageStorageBase:    36,
	onChainMessageStoragePerByte: 1,

	onChainReturnValuePerByte: 1,

	sendBase:                29233,
	sendTransferFunds:       27500,
	sendTransferOnlyPremium: 159672,
	sendInvokeMethod:        -5377,

	ipldGetBase:    114617,
	ipldPutBase:    353640,
	ipldPutPerByte: 1,

	createActorCompute: 1108454,
	createActorStorage: 36 + 40,
	deleteActor:        -(36 + 40), // -createActorStorage

	verifySignature: map[crypto.SigType]int64{
		crypto.SigTypeBLS:       16598605,
		crypto.SigTypeSecp256k1: 1637292,
	},

	hashingBase:                  31355,
	computeUnsealedSectorCidBase: 98647,
	verifySealBase:               2000, // TODO gas , it VerifySeal syscall is not used
	verifyPostLookup: map[abi.RegisteredPoStProof]scalingCost{
		abi.RegisteredPoStProof_StackedDrgWindow512MiBV1: {
			flat:  117680921,
			scale: 43780,
		},
		abi.RegisteredPoStProof_StackedDrgWindow32GiBV1: {
			flat:  117680921,
			scale: 43780,
		},
		abi.RegisteredPoStProof_StackedDrgWindow64GiBV1: {
			flat:  117680921,
			scale: 43780,
		},
	},
	verifyPostDiscount:   false,
	verifyConsensusFault: 495422,
}
