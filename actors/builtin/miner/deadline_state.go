package miner

import (
	"bytes"
	"errors"

	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	xc "github.com/filecoin-project/go-state-types/exitcode"
	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/specs-actors/v7/actors/builtin"
	"github.com/filecoin-project/specs-actors/v7/actors/runtime/proof"
	"github.com/filecoin-project/specs-actors/v7/actors/util/adt"
)

// Deadlines contains Deadline objects, describing the sectors due at the given
// deadline and their state (faulty, terminated, recovering, etc.).
type Deadlines struct {
	// Note: we could inline part of the deadline struct (e.g., active/assigned sectors)
	// to make new sector assignment cheaper. At the moment, assigning a sector requires
	// loading all deadlines to figure out where best to assign new sectors.
	Due [WPoStPeriodDeadlines]cid.Cid // []Deadline
}

// Deadline holds the state for all sectors due at a specific deadline.
type Deadline struct {
	// Partitions in this deadline, in order.
	// The keys of this AMT are always sequential integers beginning with zero.
	Partitions cid.Cid // AMT[PartitionNumber]Partition

	// Maps epochs to partitions that _may_ have sectors that expire in or
	// before that epoch, either on-time or early as faults.
	// Keys are quantized to final epochs in each proving deadline.
	//
	// NOTE: Partitions MUST NOT be removed from this queue (until the
	// associated epoch has passed) even if they no longer have sectors
	// expiring at that epoch. Sectors expiring at this epoch may later be
	// recovered, and this queue will not be updated at that time.
	ExpirationsEpochs cid.Cid // AMT[ChainEpoch]BitField

	// Partitions that have been proved by window PoSts so far during the
	// current challenge window.
	// NOTE: This bitfield includes both partitions whose proofs
	// were optimistically accepted and stored in
	// OptimisticPoStSubmissions, and those whose proofs were
	// verified on-chain.
	PartitionsPoSted bitfield.BitField

	// Partitions with sectors that terminated early.
	EarlyTerminations bitfield.BitField

	// The number of non-terminated sectors in this deadline (incl faulty).
	LiveSectors uint64

	// The total number of sectors in this deadline (incl dead).
	TotalSectors uint64

	// Memoized sum of faulty power in partitions.
	FaultyPower PowerPair

	// AMT of optimistically accepted WindowPoSt proofs, submitted during
	// the current challenge window. At the end of the challenge window,
	// this AMT will be moved to PoStSubmissionsSnapshot. WindowPoSt proofs
	// verified on-chain do not appear in this AMT.
	OptimisticPoStSubmissions cid.Cid // AMT[]WindowedPoSt

	// Snapshot of partition state at the end of the previous challenge
	// window for this deadline.
	PartitionsSnapshot cid.Cid
	// Snapshot of the proofs submitted by the end of the previous challenge
	// window for this deadline.
	//
	// These proofs may be disputed via DisputeWindowedPoSt. Successfully
	// disputed window PoSts are removed from the snapshot.
	OptimisticPoStSubmissionsSnapshot cid.Cid
}

type WindowedPoSt struct {
	// Partitions proved by this WindowedPoSt.
	Partitions bitfield.BitField
	// Array of proofs, one per distinct registered proof type present in
	// the sectors being proven. In the usual case of a single proof type,
	// this array will always have a single element (independent of number
	// of partitions).
	Proofs []proof.PoStProof
}

// Bitwidth of AMTs determined empirically from mutation patterns and projections of mainnet data.
const DeadlinePartitionsAmtBitwidth = 3 // Usually a small array
const DeadlineExpirationAmtBitwidth = 5

// Given that 4 partitions can be proven in one post, this AMT's height will
// only exceed the partition AMT's height at ~0.75EiB of storage.
const DeadlineOptimisticPoStSubmissionsAmtBitwidth = 2

//
// Deadlines (plural)
//

func ConstructDeadlines(emptyDeadlineCid cid.Cid) *Deadlines {
	d := new(Deadlines)
	for i := range d.Due {
		d.Due[i] = emptyDeadlineCid
	}
	return d
}

func (d *Deadlines) LoadDeadline(store adt.Store, dlIdx uint64) (*Deadline, error) {
	if dlIdx >= uint64(len(d.Due)) {
		return nil, xc.ErrIllegalArgument.Wrapf("invalid deadline %d", dlIdx)
	}
	deadline := new(Deadline)
	err := store.Get(store.Context(), d.Due[dlIdx], deadline)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to lookup deadline %d: %w", dlIdx, err)
	}
	return deadline, nil
}

func (d *Deadlines) ForEach(store adt.Store, cb func(dlIdx uint64, dl *Deadline) error) error {
	for dlIdx := range d.Due {
		dl, err := d.LoadDeadline(store, uint64(dlIdx))
		if err != nil {
			return err
		}
		err = cb(uint64(dlIdx), dl)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Deadlines) UpdateDeadline(store adt.Store, dlIdx uint64, deadline *Deadline) error {
	if dlIdx >= uint64(len(d.Due)) {
		return xerrors.Errorf("invalid deadline %d", dlIdx)
	}

	if err := deadline.ValidateState(); err != nil {
		return err
	}

	dlCid, err := store.Put(store.Context(), deadline)
	if err != nil {
		return err
	}
	d.Due[dlIdx] = dlCid

	return nil
}

//
// Deadline (singular)
//

func ConstructDeadline(store adt.Store) (*Deadline, error) {
	emptyPartitionsArrayCid, err := adt.StoreEmptyArray(store, DeadlinePartitionsAmtBitwidth)
	if err != nil {
		return nil, xerrors.Errorf("failed to construct empty partitions array: %w", err)
	}
	emptyDeadlineExpirationArrayCid, err := adt.StoreEmptyArray(store, DeadlineExpirationAmtBitwidth)
	if err != nil {
		return nil, xerrors.Errorf("failed to construct empty deadline expiration array: %w", err)
	}

	emptyPoStSubmissionsArrayCid, err := adt.StoreEmptyArray(store, DeadlineOptimisticPoStSubmissionsAmtBitwidth)
	if err != nil {
		return nil, xerrors.Errorf("failed to construct empty proofs array: %w", err)
	}

	return &Deadline{
		Partitions:                        emptyPartitionsArrayCid,
		ExpirationsEpochs:                 emptyDeadlineExpirationArrayCid,
		EarlyTerminations:                 bitfield.New(),
		LiveSectors:                       0,
		TotalSectors:                      0,
		FaultyPower:                       NewPowerPairZero(),
		PartitionsPoSted:                  bitfield.New(),
		OptimisticPoStSubmissions:         emptyPoStSubmissionsArrayCid,
		PartitionsSnapshot:                emptyPartitionsArrayCid,
		OptimisticPoStSubmissionsSnapshot: emptyPoStSubmissionsArrayCid,
	}, nil
}

func (d *Deadline) PartitionsArray(store adt.Store) (*adt.Array, error) {
	arr, err := adt.AsArray(store, d.Partitions, DeadlinePartitionsAmtBitwidth)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to load partitions: %w", err)
	}
	return arr, nil
}

func (d *Deadline) OptimisticProofsArray(store adt.Store) (*adt.Array, error) {
	arr, err := adt.AsArray(store, d.OptimisticPoStSubmissions, DeadlineOptimisticPoStSubmissionsAmtBitwidth)
	if err != nil {
		return nil, xerrors.Errorf("failed to load proofs: %w", err)
	}
	return arr, nil
}

func (d *Deadline) PartitionsSnapshotArray(store adt.Store) (*adt.Array, error) {
	arr, err := adt.AsArray(store, d.PartitionsSnapshot, DeadlinePartitionsAmtBitwidth)
	if err != nil {
		return nil, xerrors.Errorf("failed to load partitions snapshot: %w", err)
	}
	return arr, nil
}

func (d *Deadline) OptimisticProofsSnapshotArray(store adt.Store) (*adt.Array, error) {
	arr, err := adt.AsArray(store, d.OptimisticPoStSubmissionsSnapshot, DeadlineOptimisticPoStSubmissionsAmtBitwidth)
	if err != nil {
		return nil, xerrors.Errorf("failed to load proofs snapshot: %w", err)
	}
	return arr, nil
}

func (d *Deadline) LoadPartition(store adt.Store, partIdx uint64) (*Partition, error) {
	partitions, err := d.PartitionsArray(store)
	if err != nil {
		return nil, err
	}
	var partition Partition
	found, err := partitions.Get(partIdx, &partition)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to lookup partition %d: %w", partIdx, err)
	}
	if !found {
		return nil, xc.ErrNotFound.Wrapf("no partition %d", partIdx)
	}
	return &partition, nil
}

func (d *Deadline) LoadPartitionSnapshot(store adt.Store, partIdx uint64) (*Partition, error) {
	partitions, err := d.PartitionsSnapshotArray(store)
	if err != nil {
		return nil, err
	}
	var partition Partition
	found, err := partitions.Get(partIdx, &partition)
	if err != nil {
		return nil, xerrors.Errorf("failed to lookup partition %d: %w", partIdx, err)
	}
	if !found {
		return nil, xc.ErrNotFound.Wrapf("no partition %d", partIdx)
	}
	return &partition, nil
}

// Adds some partition numbers to the set expiring at an epoch.
func (d *Deadline) AddExpirationPartitions(store adt.Store, expirationEpoch abi.ChainEpoch, partitions []uint64, quant builtin.QuantSpec) error {
	// Avoid doing any work if there's nothing to reschedule.
	if len(partitions) == 0 {
		return nil
	}

	queue, err := LoadBitfieldQueue(store, d.ExpirationsEpochs, quant, DeadlineExpirationAmtBitwidth)
	if err != nil {
		return xerrors.Errorf("failed to load expiration queue: %w", err)
	}
	if err = queue.AddToQueueValues(expirationEpoch, partitions...); err != nil {
		return xerrors.Errorf("failed to mutate expiration queue: %w", err)
	}
	if d.ExpirationsEpochs, err = queue.Root(); err != nil {
		return xerrors.Errorf("failed to save expiration queue: %w", err)
	}
	return nil
}

// PopExpiredSectors terminates expired sectors from all partitions.
// Returns the expired sector aggregates.
func (dl *Deadline) PopExpiredSectors(store adt.Store, until abi.ChainEpoch, quant builtin.QuantSpec) (*ExpirationSet, error) {
	expiredPartitions, modified, err := dl.popExpiredPartitions(store, until, quant)
	if err != nil {
		return nil, err
	} else if !modified {
		return NewExpirationSetEmpty(), nil // nothing to do.
	}

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return nil, err
	}

	var onTimeSectors []bitfield.BitField
	var earlySectors []bitfield.BitField
	allOnTimePledge := big.Zero()
	allActivePower := NewPowerPairZero()
	allFaultyPower := NewPowerPairZero()
	var partitionsWithEarlyTerminations []uint64

	// For each partition with an expiry, remove and collect expirations from the partition queue.
	if err = expiredPartitions.ForEach(func(partIdx uint64) error {
		var partition Partition
		if found, err := partitions.Get(partIdx, &partition); err != nil {
			return err
		} else if !found {
			return xerrors.Errorf("missing expected partition %d", partIdx)
		}

		partExpiration, err := partition.PopExpiredSectors(store, until, quant)
		if err != nil {
			return xerrors.Errorf("failed to pop expired sectors from partition %d: %w", partIdx, err)
		}

		onTimeSectors = append(onTimeSectors, partExpiration.OnTimeSectors)
		earlySectors = append(earlySectors, partExpiration.EarlySectors)
		allActivePower = allActivePower.Add(partExpiration.ActivePower)
		allFaultyPower = allFaultyPower.Add(partExpiration.FaultyPower)
		allOnTimePledge = big.Add(allOnTimePledge, partExpiration.OnTimePledge)

		if empty, err := partExpiration.EarlySectors.IsEmpty(); err != nil {
			return xerrors.Errorf("failed to count early expirations from partition %d: %w", partIdx, err)
		} else if !empty {
			partitionsWithEarlyTerminations = append(partitionsWithEarlyTerminations, partIdx)
		}

		return partitions.Set(partIdx, &partition)
	}); err != nil {
		return nil, err
	}

	if dl.Partitions, err = partitions.Root(); err != nil {
		return nil, err
	}

	// Update early expiration bitmap.
	for _, partIdx := range partitionsWithEarlyTerminations {
		dl.EarlyTerminations.Set(partIdx)
	}

	allOnTimeSectors, err := bitfield.MultiMerge(onTimeSectors...)
	if err != nil {
		return nil, err
	}
	allEarlySectors, err := bitfield.MultiMerge(earlySectors...)
	if err != nil {
		return nil, err
	}

	// Update live sector count.
	onTimeCount, err := allOnTimeSectors.Count()
	if err != nil {
		return nil, xerrors.Errorf("failed to count on-time expired sectors: %w", err)
	}
	earlyCount, err := allEarlySectors.Count()
	if err != nil {
		return nil, xerrors.Errorf("failed to count early expired sectors: %w", err)
	}
	dl.LiveSectors -= onTimeCount + earlyCount

	dl.FaultyPower = dl.FaultyPower.Sub(allFaultyPower)

	return NewExpirationSet(allOnTimeSectors, allEarlySectors, allOnTimePledge, allActivePower, allFaultyPower), nil
}

// Adds sectors to a deadline. It's the caller's responsibility to make sure
// that this deadline isn't currently "open" (i.e., being proved at this point
// in time).
// The sectors are assumed to be non-faulty.
// Returns the power of the added sectors (which is active yet if proven=false).
func (dl *Deadline) AddSectors(
	store adt.Store, partitionSize uint64, proven bool, sectors []*SectorOnChainInfo,
	ssize abi.SectorSize, quant builtin.QuantSpec,
) (PowerPair, error) {
	totalPower := NewPowerPairZero()
	if len(sectors) == 0 {
		return totalPower, nil
	}

	// First update partitions, consuming the sectors
	partitionDeadlineUpdates := make(map[abi.ChainEpoch][]uint64)
	dl.LiveSectors += uint64(len(sectors))
	dl.TotalSectors += uint64(len(sectors))

	{
		partitions, err := dl.PartitionsArray(store)
		if err != nil {
			return NewPowerPairZero(), err
		}

		partIdx := partitions.Length()
		if partIdx > 0 {
			partIdx -= 1 // try filling up the last partition first.
		}

		for ; len(sectors) > 0; partIdx++ {
			// Get/create partition to update.
			partition := new(Partition)
			if found, err := partitions.Get(partIdx, partition); err != nil {
				return NewPowerPairZero(), err
			} else if !found {
				// This case will usually happen zero times.
				// It would require adding more than a full partition in one go to happen more than once.
				partition, err = ConstructPartition(store)
				if err != nil {
					return NewPowerPairZero(), err
				}
			}

			// Figure out which (if any) sectors we want to add to this partition.
			sectorCount, err := partition.Sectors.Count()
			if err != nil {
				return NewPowerPairZero(), err
			}
			if sectorCount >= partitionSize {
				continue
			}

			size := min64(partitionSize-sectorCount, uint64(len(sectors)))
			partitionNewSectors := sectors[:size]
			sectors = sectors[size:]

			// Add sectors to partition.
			partitionPower, err := partition.AddSectors(store, proven, partitionNewSectors, ssize, quant)
			if err != nil {
				return NewPowerPairZero(), err
			}
			totalPower = totalPower.Add(partitionPower)

			// Save partition back.
			err = partitions.Set(partIdx, partition)
			if err != nil {
				return NewPowerPairZero(), err
			}

			// Record deadline -> partition mapping so we can later update the deadlines.
			for _, sector := range partitionNewSectors {
				partitionUpdate := partitionDeadlineUpdates[sector.Expiration]
				// Record each new partition once.
				if len(partitionUpdate) > 0 && partitionUpdate[len(partitionUpdate)-1] == partIdx {
					continue
				}
				partitionDeadlineUpdates[sector.Expiration] = append(partitionUpdate, partIdx)
			}
		}

		// Save partitions back.
		dl.Partitions, err = partitions.Root()
		if err != nil {
			return NewPowerPairZero(), err
		}
	}

	// Next, update the expiration queue.
	{
		deadlineExpirations, err := LoadBitfieldQueue(store, dl.ExpirationsEpochs, quant, DeadlineExpirationAmtBitwidth)
		if err != nil {
			return NewPowerPairZero(), xerrors.Errorf("failed to load expiration epochs: %w", err)
		}

		if err = deadlineExpirations.AddManyToQueueValues(partitionDeadlineUpdates); err != nil {
			return NewPowerPairZero(), xerrors.Errorf("failed to add expirations for new deadlines: %w", err)
		}

		if dl.ExpirationsEpochs, err = deadlineExpirations.Root(); err != nil {
			return NewPowerPairZero(), err
		}
	}

	return totalPower, nil
}

func (dl *Deadline) PopEarlyTerminations(store adt.Store, maxPartitions, maxSectors uint64) (result TerminationResult, hasMore bool, err error) {
	stopErr := errors.New("stop error")

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return TerminationResult{}, false, err
	}

	var partitionsFinished []uint64
	if err = dl.EarlyTerminations.ForEach(func(partIdx uint64) error {
		// Load partition.
		var partition Partition
		found, err := partitions.Get(partIdx, &partition)
		if err != nil {
			return xerrors.Errorf("failed to load partition %d: %w", partIdx, err)
		}

		if !found {
			// If the partition doesn't exist any more, no problem.
			// We don't expect this to happen (compaction should re-index altered partitions),
			// but it's not worth failing if it does.
			partitionsFinished = append(partitionsFinished, partIdx)
			return nil
		}

		// Pop early terminations.
		partitionResult, more, err := partition.PopEarlyTerminations(
			store, maxSectors-result.SectorsProcessed,
		)
		if err != nil {
			return xerrors.Errorf("failed to pop terminations from partition: %w", err)
		}

		err = result.Add(partitionResult)
		if err != nil {
			return xerrors.Errorf("failed to merge termination result: %w", err)
		}

		// If we've processed all of them for this partition, unmark it in the deadline.
		if !more {
			partitionsFinished = append(partitionsFinished, partIdx)
		}

		// Save partition
		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return xerrors.Errorf("failed to store partition %v", partIdx)
		}

		if result.BelowLimit(maxPartitions, maxSectors) {
			return nil
		}

		return stopErr
	}); err != nil && err != stopErr {
		return TerminationResult{}, false, xerrors.Errorf("failed to walk early terminations bitfield for deadlines: %w", err)
	}

	// Removed finished partitions from the index.
	for _, finished := range partitionsFinished {
		dl.EarlyTerminations.Unset(finished)
	}

	// Save deadline's partitions
	dl.Partitions, err = partitions.Root()
	if err != nil {
		return TerminationResult{}, false, xerrors.Errorf("failed to update partitions")
	}

	// Update global early terminations bitfield.
	noEarlyTerminations, err := dl.EarlyTerminations.IsEmpty()
	if err != nil {
		return TerminationResult{}, false, xerrors.Errorf("failed to count remaining early terminations partitions: %w", err)
	}

	return result, !noEarlyTerminations, nil
}

// Returns nil if nothing was popped.
func (dl *Deadline) popExpiredPartitions(store adt.Store, until abi.ChainEpoch, quant builtin.QuantSpec) (bitfield.BitField, bool, error) {
	expirations, err := LoadBitfieldQueue(store, dl.ExpirationsEpochs, quant, DeadlineExpirationAmtBitwidth)
	if err != nil {
		return bitfield.BitField{}, false, err
	}

	popped, modified, err := expirations.PopUntil(until)
	if err != nil {
		return bitfield.BitField{}, false, xerrors.Errorf("failed to pop expiring partitions: %w", err)
	}

	if modified {
		dl.ExpirationsEpochs, err = expirations.Root()
		if err != nil {
			return bitfield.BitField{}, false, err
		}
	}

	return popped, modified, nil
}

func (dl *Deadline) TerminateSectors(
	store adt.Store,
	sectors Sectors,
	epoch abi.ChainEpoch,
	partitionSectors PartitionSectorMap,
	ssize abi.SectorSize,
	quant builtin.QuantSpec,
) (powerLost PowerPair, err error) {

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return NewPowerPairZero(), err
	}

	powerLost = NewPowerPairZero()
	var partition Partition
	if err := partitionSectors.ForEach(func(partIdx uint64, sectorNos bitfield.BitField) error {
		if found, err := partitions.Get(partIdx, &partition); err != nil {
			return xerrors.Errorf("failed to load partition %d: %w", partIdx, err)
		} else if !found {
			return xc.ErrNotFound.Wrapf("failed to find partition %d", partIdx)
		}

		removed, err := partition.TerminateSectors(store, sectors, epoch, sectorNos, ssize, quant)
		if err != nil {
			return xerrors.Errorf("failed to terminate sectors in partition %d: %w", partIdx, err)
		}

		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return xerrors.Errorf("failed to store updated partition %d: %w", partIdx, err)
		}

		if count, err := removed.Count(); err != nil {
			return xerrors.Errorf("failed to count terminated sectors in partition %d: %w", partIdx, err)
		} else if count > 0 {
			// Record that partition now has pending early terminations.
			dl.EarlyTerminations.Set(partIdx)
			// Record change to sectors and power
			dl.LiveSectors -= count
		} // note: we should _always_ have early terminations, unless the early termination bitfield is empty.

		dl.FaultyPower = dl.FaultyPower.Sub(removed.FaultyPower)

		// Aggregate power lost from active sectors
		powerLost = powerLost.Add(removed.ActivePower)
		return nil
	}); err != nil {
		return NewPowerPairZero(), err
	}

	// save partitions back
	dl.Partitions, err = partitions.Root()
	if err != nil {
		return NewPowerPairZero(), xerrors.Errorf("failed to persist partitions: %w", err)
	}

	return powerLost, nil
}

// RemovePartitions removes the specified partitions, shifting the remaining
// ones to the left, and returning the live and dead sectors they contained.
//
// Returns an error if any of the partitions contained faulty sectors or early
// terminations.
func (dl *Deadline) RemovePartitions(store adt.Store, toRemove bitfield.BitField, quant builtin.QuantSpec) (
	live, dead bitfield.BitField, removedPower PowerPair, err error,
) {
	oldPartitions, err := dl.PartitionsArray(store)
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to load partitions: %w", err)
	}

	partitionCount := oldPartitions.Length()
	toRemoveSet, err := toRemove.AllMap(partitionCount)
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xc.ErrIllegalArgument.Wrapf("failed to expand partitions into map: %w", err)
	}

	// Nothing to do.
	if len(toRemoveSet) == 0 {
		return bitfield.NewFromSet(nil), bitfield.NewFromSet(nil), NewPowerPairZero(), nil
	}

	for partIdx := range toRemoveSet { //nolint:nomaprange
		if partIdx >= partitionCount {
			return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xc.ErrIllegalArgument.Wrapf(
				"partition index %d out of range [0, %d)", partIdx, partitionCount,
			)
		}
	}

	// Should already be checked earlier, but we might as well check again.
	noEarlyTerminations, err := dl.EarlyTerminations.IsEmpty()
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to check for early terminations: %w", err)
	}
	if !noEarlyTerminations {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("cannot remove partitions from deadline with early terminations: %w", err)
	}

	newPartitions, err := adt.MakeEmptyArray(store, DeadlinePartitionsAmtBitwidth)
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to create empty array for initializing partitions: %w", err)
	}
	allDeadSectors := make([]bitfield.BitField, 0, len(toRemoveSet))
	allLiveSectors := make([]bitfield.BitField, 0, len(toRemoveSet))
	removedPower = NewPowerPairZero()

	// Define all of these out here to save allocations.
	var (
		lazyPartition cbg.Deferred
		byteReader    bytes.Reader
		partition     Partition
	)
	if err = oldPartitions.ForEach(&lazyPartition, func(partIdx int64) error {
		// If we're keeping the partition as-is, append it to the new partitions array.
		if _, ok := toRemoveSet[uint64(partIdx)]; !ok {
			return newPartitions.AppendContinuous(&lazyPartition)
		}

		// Ok, actually unmarshal the partition.
		byteReader.Reset(lazyPartition.Raw)
		err := partition.UnmarshalCBOR(&byteReader)
		byteReader.Reset(nil)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to decode partition %d: %w", partIdx, err)
		}

		// Don't allow removing partitions with faulty sectors.
		hasNoFaults, err := partition.Faults.IsEmpty()
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to decode faults for partition %d: %w", partIdx, err)
		}
		if !hasNoFaults {
			return xc.ErrIllegalArgument.Wrapf("cannot remove partition %d: has faults", partIdx)
		}

		// Don't allow removing partitions with unproven sectors.
		allProven, err := partition.Unproven.IsEmpty()
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to decode unproven for partition %d: %w", partIdx, err)
		}
		if !allProven {
			return xc.ErrIllegalArgument.Wrapf("cannot remove partition %d: has unproven sectors", partIdx)
		}

		// Get the live sectors.
		liveSectors, err := partition.LiveSectors()
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to calculate live sectors for partition %d: %w", partIdx, err)
		}

		allDeadSectors = append(allDeadSectors, partition.Terminated)
		allLiveSectors = append(allLiveSectors, liveSectors)
		removedPower = removedPower.Add(partition.LivePower)
		return nil
	}); err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("while removing partitions: %w", err)
	}

	dl.Partitions, err = newPartitions.Root()
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to persist new partition table: %w", err)
	}

	dead, err = bitfield.MultiMerge(allDeadSectors...)
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to merge dead sector bitfields: %w", err)
	}
	live, err = bitfield.MultiMerge(allLiveSectors...)
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to merge live sector bitfields: %w", err)
	}

	// Update sector counts.
	removedDeadSectors, err := dead.Count()
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to count dead sectors: %w", err)
	}

	removedLiveSectors, err := live.Count()
	if err != nil {
		return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to count live sectors: %w", err)
	}

	dl.LiveSectors -= removedLiveSectors
	dl.TotalSectors -= removedLiveSectors + removedDeadSectors

	// Update expiration bitfields.
	{
		expirationEpochs, err := LoadBitfieldQueue(store, dl.ExpirationsEpochs, quant, DeadlineExpirationAmtBitwidth)
		if err != nil {
			return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed to load expiration queue: %w", err)
		}

		err = expirationEpochs.Cut(toRemove)
		if err != nil {
			return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed cut removed partitions from deadline expiration queue: %w", err)
		}

		dl.ExpirationsEpochs, err = expirationEpochs.Root()
		if err != nil {
			return bitfield.BitField{}, bitfield.BitField{}, NewPowerPairZero(), xerrors.Errorf("failed persist deadline expiration queue: %w", err)
		}
	}

	return live, dead, removedPower, nil
}

func (dl *Deadline) RecordFaults(
	store adt.Store, sectors Sectors, ssize abi.SectorSize, quant builtin.QuantSpec,
	faultExpirationEpoch abi.ChainEpoch, partitionSectors PartitionSectorMap,
) (powerDelta PowerPair, err error) {
	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return NewPowerPairZero(), err
	}

	// Record partitions with some fault, for subsequently indexing in the deadline.
	// Duplicate entries don't matter, they'll be stored in a bitfield (a set).
	partitionsWithFault := make([]uint64, 0, len(partitionSectors))
	powerDelta = NewPowerPairZero()
	if err := partitionSectors.ForEach(func(partIdx uint64, sectorNos bitfield.BitField) error {
		var partition Partition
		if found, err := partitions.Get(partIdx, &partition); err != nil {
			return xc.ErrIllegalState.Wrapf("failed to load partition %d: %w", partIdx, err)
		} else if !found {
			return xc.ErrNotFound.Wrapf("no such partition %d", partIdx)
		}

		newFaults, partitionPowerDelta, partitionNewFaultyPower, err := partition.RecordFaults(
			store, sectors, sectorNos, faultExpirationEpoch, ssize, quant,
		)
		if err != nil {
			return xerrors.Errorf("failed to declare faults in partition %d: %w", partIdx, err)
		}
		dl.FaultyPower = dl.FaultyPower.Add(partitionNewFaultyPower)
		powerDelta = powerDelta.Add(partitionPowerDelta)
		if empty, err := newFaults.IsEmpty(); err != nil {
			return xerrors.Errorf("failed to count new faults: %w", err)
		} else if !empty {
			partitionsWithFault = append(partitionsWithFault, partIdx)
		}

		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to store partition %d: %w", partIdx, err)
		}

		return nil
	}); err != nil {
		return NewPowerPairZero(), err
	}

	dl.Partitions, err = partitions.Root()
	if err != nil {
		return NewPowerPairZero(), xc.ErrIllegalState.Wrapf("failed to store partitions root: %w", err)
	}

	err = dl.AddExpirationPartitions(store, faultExpirationEpoch, partitionsWithFault, quant)
	if err != nil {
		return NewPowerPairZero(), xc.ErrIllegalState.Wrapf("failed to update expirations for partitions with faults: %w", err)
	}

	return powerDelta, nil
}

func (dl *Deadline) DeclareFaultsRecovered(
	store adt.Store, sectors Sectors, ssize abi.SectorSize,
	partitionSectors PartitionSectorMap,
) (err error) {
	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return err
	}

	if err := partitionSectors.ForEach(func(partIdx uint64, sectorNos bitfield.BitField) error {
		var partition Partition
		if found, err := partitions.Get(partIdx, &partition); err != nil {
			return xc.ErrIllegalState.Wrapf("failed to load partition %d: %w", partIdx, err)
		} else if !found {
			return xc.ErrNotFound.Wrapf("no such partition %d", partIdx)
		}

		if err = partition.DeclareFaultsRecovered(sectors, ssize, sectorNos); err != nil {
			return xc.ErrIllegalState.Wrapf("failed to add recoveries: %w", err)
		}

		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to update partition %d: %w", partIdx, err)
		}
		return nil
	}); err != nil {
		return err
	}

	// Power is not regained until the deadline end, when the recovery is confirmed.

	dl.Partitions, err = partitions.Root()
	if err != nil {
		return xc.ErrIllegalState.Wrapf("failed to store partitions root: %w", err)
	}
	return nil
}

// ProcessDeadlineEnd processes all PoSt submissions, marking unproven sectors as
// faulty and clearing failed recoveries. It returns the power delta, and any
// power that should be penalized (new faults and failed recoveries).
func (dl *Deadline) ProcessDeadlineEnd(store adt.Store, quant builtin.QuantSpec, faultExpirationEpoch abi.ChainEpoch) (
	powerDelta, penalizedPower PowerPair, err error,
) {
	powerDelta = NewPowerPairZero()
	penalizedPower = NewPowerPairZero()

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return powerDelta, penalizedPower, xerrors.Errorf("failed to load partitions: %w", err)
	}

	detectedAny := false
	var rescheduledPartitions []uint64
	for partIdx := uint64(0); partIdx < partitions.Length(); partIdx++ {
		proven, err := dl.PartitionsPoSted.IsSet(partIdx)
		if err != nil {
			return powerDelta, penalizedPower, xerrors.Errorf("failed to check submission for partition %d: %w", partIdx, err)
		}
		if proven {
			continue
		}

		var partition Partition
		found, err := partitions.Get(partIdx, &partition)
		if err != nil {
			return powerDelta, penalizedPower, xerrors.Errorf("failed to load partition %d: %w", partIdx, err)
		}
		if !found {
			return powerDelta, penalizedPower, xerrors.Errorf("no partition %d", partIdx)
		}

		// If we have no recovering power/sectors, and all power is faulty, skip
		// this. This lets us skip some work if a miner repeatedly fails to PoSt.
		if partition.RecoveringPower.IsZero() && partition.FaultyPower.Equals(partition.LivePower) {
			continue
		}

		// Ok, we actually need to process this partition. Make sure we save the partition state back.
		detectedAny = true

		partPowerDelta, partPenalizedPower, partNewFaultyPower, err := partition.RecordMissedPost(store, faultExpirationEpoch, quant)
		if err != nil {
			return powerDelta, penalizedPower, xerrors.Errorf("failed to record missed PoSt for partition %v: %w", partIdx, err)
		}

		// We marked some sectors faulty, we need to record the new
		// expiration. We don't want to do this if we're just penalizing
		// the miner for failing to recover power.
		if !partNewFaultyPower.IsZero() {
			rescheduledPartitions = append(rescheduledPartitions, partIdx)
		}

		// Save new partition state.
		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return powerDelta, penalizedPower, xerrors.Errorf("failed to update partition %v: %w", partIdx, err)
		}

		dl.FaultyPower = dl.FaultyPower.Add(partNewFaultyPower)

		powerDelta = powerDelta.Add(partPowerDelta)
		penalizedPower = penalizedPower.Add(partPenalizedPower)
	}

	// Save modified deadline state.
	if detectedAny {
		dl.Partitions, err = partitions.Root()
		if err != nil {
			return powerDelta, penalizedPower, xc.ErrIllegalState.Wrapf("failed to store partitions: %w", err)
		}
	}

	err = dl.AddExpirationPartitions(store, faultExpirationEpoch, rescheduledPartitions, quant)
	if err != nil {
		return powerDelta, penalizedPower, xc.ErrIllegalState.Wrapf("failed to update deadline expiration queue: %w", err)
	}

	// Reset PoSt submissions, snapshot proofs.
	dl.PartitionsPoSted = bitfield.New()
	dl.PartitionsSnapshot = dl.Partitions
	dl.OptimisticPoStSubmissionsSnapshot = dl.OptimisticPoStSubmissions
	dl.OptimisticPoStSubmissions, err = adt.StoreEmptyArray(store, DeadlineOptimisticPoStSubmissionsAmtBitwidth)
	if err != nil {
		return powerDelta, penalizedPower, xerrors.Errorf("failed to clear pending proofs array: %w", err)
	}
	return powerDelta, penalizedPower, nil
}

type PoStResult struct {
	// Power activated or deactivated (positive or negative).
	PowerDelta PowerPair
	// Powers used for calculating penalties.
	NewFaultyPower, RetractedRecoveryPower, RecoveredPower PowerPair
	// Sectors is a bitfield of all sectors in the proven partitions.
	Sectors bitfield.BitField
	// IgnoredSectors is a subset of Sectors that should be ignored.
	IgnoredSectors bitfield.BitField
	// Bitfield of partitions that were proven.
	Partitions bitfield.BitField
}

// RecordProvenSectors processes a series of posts, recording proven partitions
// and marking skipped sectors as faulty.
//
// It returns a PoStResult containing the list of proven and skipped sectors and
// changes to power (newly faulty power, power that should have been proven
// recovered but wasn't, and newly recovered power).
//
// NOTE: This function does not actually _verify_ any proofs.
func (dl *Deadline) RecordProvenSectors(
	store adt.Store, sectors Sectors,
	ssize abi.SectorSize, quant builtin.QuantSpec, faultExpiration abi.ChainEpoch,
	postPartitions []PoStPartition,
) (*PoStResult, error) {

	partitionIndexes := bitfield.New()
	for _, partition := range postPartitions {
		partitionIndexes.Set(partition.Index)
	}
	if numPartitions, err := partitionIndexes.Count(); err != nil {
		return nil, xerrors.Errorf("failed to count posted partitions: %w", err)
	} else if numPartitions != uint64(len(postPartitions)) {
		return nil, xc.ErrIllegalArgument.Wrapf("duplicate partitions proven")
	}

	// First check to see if we're proving any already proven partitions.
	// This is faster than checking one by one.
	if alreadyProven, err := bitfield.IntersectBitField(dl.PartitionsPoSted, partitionIndexes); err != nil {
		return nil, xerrors.Errorf("failed to check proven partitions: %w", err)
	} else if empty, err := alreadyProven.IsEmpty(); err != nil {
		return nil, xerrors.Errorf("failed to check proven intersection is empty: %w", err)
	} else if !empty {
		return nil, xc.ErrIllegalArgument.Wrapf("partition already proven: %v", alreadyProven)
	}

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return nil, err
	}

	allSectors := make([]bitfield.BitField, 0, len(postPartitions))
	allIgnored := make([]bitfield.BitField, 0, len(postPartitions))
	newFaultyPowerTotal := NewPowerPairZero()
	retractedRecoveryPowerTotal := NewPowerPairZero()
	recoveredPowerTotal := NewPowerPairZero()
	powerDelta := NewPowerPairZero()
	var rescheduledPartitions []uint64

	// Accumulate sectors info for proof verification.
	for _, post := range postPartitions {
		var partition Partition
		found, err := partitions.Get(post.Index, &partition)
		if err != nil {
			return nil, xerrors.Errorf("failed to load partition %d: %w", post.Index, err)
		} else if !found {
			return nil, xc.ErrNotFound.Wrapf("no such partition %d", post.Index)
		}

		// Process new faults and accumulate new faulty power.
		// This updates the faults in partition state ahead of calculating the sectors to include for proof.
		newPowerDelta, newFaultPower, retractedRecoveryPower, hasNewFaults, err := partition.RecordSkippedFaults(
			store, sectors, ssize, quant, faultExpiration, post.Skipped,
		)
		if err != nil {
			return nil, xerrors.Errorf("failed to add skipped faults to partition %d: %w", post.Index, err)
		}

		// If we have new faulty power, we've added some faults. We need
		// to record the new expiration in the deadline.
		if hasNewFaults {
			rescheduledPartitions = append(rescheduledPartitions, post.Index)
		}

		recoveredPower, err := partition.RecoverFaults(store, sectors, ssize, quant)
		if err != nil {
			return nil, xerrors.Errorf("failed to recover faulty sectors for partition %d: %w", post.Index, err)
		}

		// Finally, activate power for newly proven sectors.
		newPowerDelta = newPowerDelta.Add(partition.ActivateUnproven())

		// This will be rolled back if the method aborts with a failed proof.
		err = partitions.Set(post.Index, &partition)
		if err != nil {
			return nil, xc.ErrIllegalState.Wrapf("failed to update partition %v: %w", post.Index, err)
		}

		newFaultyPowerTotal = newFaultyPowerTotal.Add(newFaultPower)
		retractedRecoveryPowerTotal = retractedRecoveryPowerTotal.Add(retractedRecoveryPower)
		recoveredPowerTotal = recoveredPowerTotal.Add(recoveredPower)
		powerDelta = powerDelta.Add(newPowerDelta).Add(recoveredPower)

		// Record the post.
		dl.PartitionsPoSted.Set(post.Index)

		// At this point, the partition faults represents the expected faults for the proof, with new skipped
		// faults and recoveries taken into account.
		allSectors = append(allSectors, partition.Sectors)
		allIgnored = append(allIgnored, partition.Faults)
		allIgnored = append(allIgnored, partition.Terminated)
	}

	err = dl.AddExpirationPartitions(store, faultExpiration, rescheduledPartitions, quant)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to update expirations for partitions with faults: %w", err)
	}

	// Save everything back.
	dl.FaultyPower = dl.FaultyPower.Sub(recoveredPowerTotal).Add(newFaultyPowerTotal)

	dl.Partitions, err = partitions.Root()
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to persist partitions: %w", err)
	}

	// Collect all sectors, faults, and recoveries for proof verification.
	allSectorNos, err := bitfield.MultiMerge(allSectors...)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to merge all sectors bitfields: %w", err)
	}
	allIgnoredSectorNos, err := bitfield.MultiMerge(allIgnored...)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to merge ignored sectors bitfields: %w", err)
	}

	return &PoStResult{
		Sectors:                allSectorNos,
		IgnoredSectors:         allIgnoredSectorNos,
		PowerDelta:             powerDelta,
		NewFaultyPower:         newFaultyPowerTotal,
		RecoveredPower:         recoveredPowerTotal,
		RetractedRecoveryPower: retractedRecoveryPowerTotal,
		Partitions:             partitionIndexes,
	}, nil
}

// RecordPoStProofs records a set of optimistically accepted PoSt proofs
// (usually one), associating them with the given partitions.
func (dl *Deadline) RecordPoStProofs(store adt.Store, partitions bitfield.BitField, proofs []proof.PoStProof) error {
	proofArr, err := dl.OptimisticProofsArray(store)
	if err != nil {
		return xerrors.Errorf("failed to load proofs: %w", err)
	}
	err = proofArr.AppendContinuous(&WindowedPoSt{
		Partitions: partitions,
		Proofs:     proofs,
	})
	if err != nil {
		return xerrors.Errorf("failed to store proof: %w", err)
	}

	root, err := proofArr.Root()
	if err != nil {
		return xerrors.Errorf("failed to save proofs: %w", err)
	}
	dl.OptimisticPoStSubmissions = root
	return nil
}

// TakePoStProofs removes and returns a PoSt proof by index, along with the
// associated partitions. This method takes the PoSt from the PoSt submissions
// snapshot.
func (dl *Deadline) TakePoStProofs(store adt.Store, idx uint64) (partitions bitfield.BitField, proofs []proof.PoStProof, err error) {
	proofArr, err := dl.OptimisticProofsSnapshotArray(store)
	if err != nil {
		return bitfield.New(), nil, xerrors.Errorf("failed to load proofs: %w", err)
	}

	// Extract and remove the proof from the proofs array, leaving a hole.
	// This will not affect concurrent attempts to refute other proofs.
	var post WindowedPoSt
	if found, err := proofArr.Pop(idx, &post); err != nil {
		return bitfield.New(), nil, xerrors.Errorf("failed to retrieve proof %d: %w", idx, err)
	} else if !found {
		return bitfield.New(), nil, xc.ErrIllegalArgument.Wrapf("proof %d not found", idx)
	}

	root, err := proofArr.Root()
	if err != nil {
		return bitfield.New(), nil, xerrors.Errorf("failed to save proofs: %w", err)
	}
	dl.OptimisticPoStSubmissionsSnapshot = root
	return post.Partitions, post.Proofs, nil
}

// DisputeInfo includes all the information necessary to dispute a post to the
// given partitions.
type DisputeInfo struct {
	AllSectorNos, IgnoredSectorNos bitfield.BitField
	DisputedSectors                PartitionSectorMap
	DisputedPower                  PowerPair
}

// LoadPartitionsForDispute
func (dl *Deadline) LoadPartitionsForDispute(store adt.Store, partitions bitfield.BitField) (*DisputeInfo, error) {
	partitionsSnapshot, err := dl.PartitionsSnapshotArray(store)
	if err != nil {
		return nil, xerrors.Errorf("failed to load partitions: %w", err)
	}

	var allSectors, allIgnored []bitfield.BitField
	disputedSectors := make(PartitionSectorMap)
	disputedPower := NewPowerPairZero()
	err = partitions.ForEach(func(partIdx uint64) error {
		var partitionSnapshot Partition
		if found, err := partitionsSnapshot.Get(partIdx, &partitionSnapshot); err != nil {
			return err
		} else if !found {
			return xerrors.Errorf("failed to find partition %d", partIdx)
		}

		// Record sectors for proof verification
		allSectors = append(allSectors, partitionSnapshot.Sectors)
		allIgnored = append(allIgnored, partitionSnapshot.Faults)
		allIgnored = append(allIgnored, partitionSnapshot.Terminated)
		allIgnored = append(allIgnored, partitionSnapshot.Unproven)

		// Record active sectors for marking faults.
		active, err := partitionSnapshot.ActiveSectors()
		if err != nil {
			return err
		}
		err = disputedSectors.Add(partIdx, active)
		if err != nil {
			return err
		}

		// Record disputed power for penalties.
		//
		// NOTE: This also includes power that was
		// activated at the end of the last challenge
		// window, and power from sectors that have since
		// expired.
		disputedPower = disputedPower.Add(partitionSnapshot.ActivePower())
		return nil
	})
	if err != nil {
		return nil, xerrors.Errorf("when disputing post: %w", err)
	}

	allSectorsNos, err := bitfield.MultiMerge(allSectors...)
	if err != nil {
		return nil, xerrors.Errorf("failed to merge sector bitfields: %w", err)
	}

	allIgnoredNos, err := bitfield.MultiMerge(allIgnored...)
	if err != nil {
		return nil, xerrors.Errorf("failed to merge fault bitfields: %w", err)
	}

	return &DisputeInfo{
		AllSectorNos:     allSectorsNos,
		IgnoredSectorNos: allIgnoredNos,
		DisputedSectors:  disputedSectors,
		DisputedPower:    disputedPower,
	}, nil
}

// IsLive returns true if the deadline has any live sectors or any other state that should be
// updated at the end of the challenge window.
func (d *Deadline) IsLive() (bool, error) {
	// If we have live sectors, we're definitely live.
	if d.LiveSectors > 0 {
		return true, nil
	}

	if hasNoProofs, err := d.PartitionsPoSted.IsEmpty(); err != nil {
		return true, xerrors.Errorf("invalid partitions posted bitfield: %w", err)
	} else if !hasNoProofs {
		// _This_ case should be impossible, but there's no good way to log from here. We
		// might as well just process the deadline end and move on.
		return true, nil
	}

	// If the partitions have changed, we may have work to do. We should at least update the
	// partitions snapshot one last time.
	if d.Partitions != d.PartitionsSnapshot {
		return true, nil
	}

	// If we don't have any proofs, and the proofs snapshot isn't the same as the current proofs
	// snapshot (which should be empty), we should update the deadline one last time to empty
	// the proofs snapshot.
	if d.OptimisticPoStSubmissions != d.OptimisticPoStSubmissionsSnapshot {
		return true, nil
	}

	// Otherwise, the deadline is definitely dead.
	return false, nil
}

func (d *Deadline) ValidateState() error {
	if d.LiveSectors > d.TotalSectors {
		return xerrors.Errorf("Deadline left with more live sectors than total: %v", d)
	}

	if d.FaultyPower.Raw.LessThan(big.Zero()) || d.FaultyPower.QA.LessThan(big.Zero()) {
		return xerrors.Errorf("Deadline left with negative faulty power: %v", d)
	}

	return nil
}
