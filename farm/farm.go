// Package farm provides a robust API for CRDTs on top of multiple clusters.
package farm

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/soundcloud/roshi/cluster"
	"github.com/soundcloud/roshi/common"
	"github.com/soundcloud/roshi/instrumentation"
)

const (
	// MaxInt is what you think it is. Unfortunately not provided
	// by the go math package.
	MaxInt = int(^uint(0) >> 1)
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Farm implements CRDT-enabled ZSET methods over many clusters.
type Farm struct {
	clusters        []cluster.Cluster
	writeQuorum     int
	readStrategy    coreReadStrategy
	repairer        coreRepairer
	walkCompleted   chan bool
	ratePolice      RatePolice
	instrumentation instrumentation.Instrumentation
}

// New creates and returns a new Farm.
//
// Writes are always sent to all clusters, and writeQuorum determines how many
// individual successful responses need to be received before the client
// receives an overall success. Reads are sent to clusters according to the
// passed ReadStrategy.
//
// The repairer handles read-repairs (may be nil for no repairs).
//
// The walkerRate defines the max number of keys the data walker reads
// per second. If 0, no data walk will happen.
//
// When the data walker finishes a walk of the whole farm, it will
// send true to the walkCompleted channel (but only if it is ready to
// receive at that moment). (Use nil if you don't want to receive
// anything.)
//
// The RatePolice is used to limit the walk rate so that the sum of
// keys read by the walker and keys read by actual queries does not
// exceed the walkerRate. (rp can be nil, in which case no limits will
// be imposed. Only do that with a walkerRate of 0.)
//
// Set instr to nil if you don't need instrumentation.
func New(
	clusters []cluster.Cluster,
	writeQuorum int,
	readStrategy ReadStrategy,
	repairer Repairer,
	walkerRate int,
	walkCompleted chan bool,
	rp RatePolice,
	instr instrumentation.Instrumentation,
) *Farm {
	if rp == nil {
		rp = NewNoPolice()
	}
	if instr == nil {
		instr = instrumentation.NopInstrumentation{}
	}
	if repairer == nil {
		repairer = NopRepairer
	}
	farm := &Farm{
		clusters:        clusters,
		writeQuorum:     writeQuorum,
		walkCompleted:   walkCompleted,
		ratePolice:      rp,
		instrumentation: instr,
	}
	farm.readStrategy = readStrategy(farm)
	farm.repairer = repairer(farm)
	go farm.startWalker(walkerRate)
	return farm
}

// Insert adds each tuple into each underlying cluster, if the scores are
// greater than the already-stored scores. As long as over half of the clusters
// succeed to write all tuples, the overall write succeeds.
func (f *Farm) Insert(tuples []common.KeyScoreMember) error {
	return f.write(
		tuples,
		func(c cluster.Cluster, a []common.KeyScoreMember) error { return c.Insert(a) },
		insertInstrumentation{f.instrumentation},
	)
}

// Selecter defines a synchronous Select API, implemented by Farm.
type Selecter interface {
	Select(keys []string, offset, limit int) (map[string][]common.KeyScoreMember, error)
}

// Select invokes the ReadStrategy of the farm.
func (f *Farm) Select(keys []string, offset, limit int) (map[string][]common.KeyScoreMember, error) {
	// High performance optimization.
	if len(keys) <= 0 {
		return map[string][]common.KeyScoreMember{}, nil
	}
	f.ratePolice.Report(len(keys))
	return f.readStrategy(keys, offset, limit)
}

// Delete removes each tuple from the underlying clusters, if the score is
// greater than the already-stored scores.
func (f *Farm) Delete(tuples []common.KeyScoreMember) error {
	return f.write(
		tuples,
		func(c cluster.Cluster, a []common.KeyScoreMember) error { return c.Delete(a) },
		deleteInstrumentation{f.instrumentation},
	)
}

func (f *Farm) startWalker(walkerRate int) {
	if walkerRate == 0 {
		return
	}
	keyChannel := make(chan string)
	walkCompleted := make(chan bool, 1)

	// Start a goroutine that endlessly iterates through all
	// clusters in random order. (It will visit each cluster
	// exactly once before starting over.) The keys of each
	// cluster are sent one after another to the keyChannel.
	go func() {
		for {
			anythingSent := false
			for _, i := range rand.Perm(len(f.clusters)) {
				for key := range f.clusters[i].Keys() {
					keyChannel <- key
					anythingSent = true
				}
			}
			f.instrumentation.KeysFarmCompleted()
			// Report completed if not already done so.
			select {
			case walkCompleted <- true:
			default:
			}
			if !anythingSent {
				// Don't spin too quickly in case of an
				// empty cluster.
				time.Sleep(time.Second)
			}
		}
	}()

	for {
		batchSize := f.ratePolice.Request(walkerRate)
		if batchSize <= 0 {
			// Too much traffic. Wait for a sec and try again.
			f.instrumentation.KeysThrottled()
			time.Sleep(time.Second)
			continue
		}
		// Safeguard against excessive batchSize.
		if batchSize > 10*walkerRate {
			batchSize = 10 * walkerRate
		}
		keys := []string{}
		for ; batchSize > 0; batchSize-- {
			keys = append(keys, <-keyChannel)
		}
		// We are only interested in triggering the
		// read repair, so we throw away the results
		// and don't check for errors.
		f.Select(keys, 0, MaxInt)
		// Report completion if we were asked to do so _and_ a
		// walk was actually reported _and_ the report channel
		// is ready to receive.
		if f.walkCompleted != nil {
			select {
			case <-walkCompleted:
				select {
				case f.walkCompleted <- true:
				default:
				}
			default:
			}
		}
	}
}

func (f *Farm) write(
	tuples []common.KeyScoreMember,
	action func(cluster.Cluster, []common.KeyScoreMember) error,
	instr writeInstrumentation,
) error {
	// High performance optimization.
	if len(tuples) <= 0 {
		return nil
	}
	instr.call()
	instr.recordCount(len(tuples))
	defer func(began time.Time) {
		d := time.Now().Sub(began)
		instr.callDuration(d)
		instr.recordDuration(d / time.Duration(len(tuples)))
	}(time.Now())

	// Scatter
	errChan := make(chan error, len(f.clusters))
	for _, c := range f.clusters {
		go func(c cluster.Cluster) {
			errChan <- action(c, tuples)
		}(c)
	}

	// Gather
	errors, got, need := []string{}, 0, f.writeQuorum
	haveQuorum := func() bool { return got-len(errors) >= need }
	for i := 0; i < cap(errChan); i++ {
		err := <-errChan
		if err != nil {
			errors = append(errors, err.Error())
		}
		got++
		if haveQuorum() {
			break
		}
	}

	// Report
	if !haveQuorum() {
		instr.quorumFailure()
		return fmt.Errorf("no quorum (%s)", strings.Join(errors, "; "))
	}
	return nil
}

// unionDifference computes two sets of keys from the input sets. Union is
// defined to be every key-member and its best (highest) score. Difference is
// defined to be those key-members with imperfect agreement across all input
// sets.
func unionDifference(tupleSets []tupleSet) (tupleSet, keyMemberSet) {
	expectedCount := len(tupleSets)
	counts := map[common.KeyScoreMember]int{}
	scores := map[keyMember]float64{}
	for _, tupleSet := range tupleSets {
		for tuple := range tupleSet {
			// For union
			km := keyMember{Key: tuple.Key, Member: tuple.Member}
			if score, ok := scores[km]; !ok || tuple.Score > score {
				scores[km] = tuple.Score
			}
			// For difference
			counts[tuple]++
		}
	}

	union, difference := tupleSet{}, keyMemberSet{}
	for km, bestScore := range scores {
		union.add(common.KeyScoreMember{
			Key:    km.Key,
			Score:  bestScore,
			Member: km.Member,
		})
	}
	for ksm, count := range counts {
		if count < expectedCount {
			difference.add(keyMember{
				Key:    ksm.Key,
				Member: ksm.Member,
			})
		}
	}
	return union, difference
}

type tupleSet map[common.KeyScoreMember]struct{}

func makeSet(a []common.KeyScoreMember) tupleSet {
	s := make(tupleSet, len(a))
	for _, tuple := range a {
		s.add(tuple)
	}
	return s
}

func (s tupleSet) add(tuple common.KeyScoreMember) {
	s[tuple] = struct{}{}
}

func (s tupleSet) has(tuple common.KeyScoreMember) bool {
	_, ok := s[tuple]
	return ok
}

func (s tupleSet) addMany(other tupleSet) {
	for tuple := range other {
		s.add(tuple)
	}
}

func (s tupleSet) slice() []common.KeyScoreMember {
	a := make([]common.KeyScoreMember, 0, len(s))
	for tuple := range s {
		a = append(a, tuple)
	}
	return a
}

func (s tupleSet) orderedLimitedSlice(limit int) []common.KeyScoreMember {
	a := s.slice()
	sort.Sort(common.KeyScoreMembers(a))
	if len(a) > limit {
		a = a[:limit]
	}
	return a
}

type keyMemberSet map[keyMember]struct{}

func (s keyMemberSet) add(km keyMember) {
	s[km] = struct{}{}
}

func (s keyMemberSet) addMany(other keyMemberSet) {
	for km := range other {
		s.add(km)
	}
}

func (s keyMemberSet) slice() []keyMember {
	a := make([]keyMember, 0, len(s))
	for km := range s {
		a = append(a, km)
	}
	return a
}

type writeInstrumentation interface {
	call()
	recordCount(int)
	callDuration(time.Duration)
	recordDuration(time.Duration)
	quorumFailure()
}

type insertInstrumentation struct {
	instrumentation.Instrumentation
}

func (i insertInstrumentation) call()                          { i.InsertCall() }
func (i insertInstrumentation) recordCount(n int)              { i.InsertRecordCount(n) }
func (i insertInstrumentation) callDuration(d time.Duration)   { i.InsertCallDuration(d) }
func (i insertInstrumentation) recordDuration(d time.Duration) { i.InsertRecordDuration(d) }
func (i insertInstrumentation) quorumFailure()                 { i.InsertQuorumFailure() }

type deleteInstrumentation struct {
	instrumentation.Instrumentation
}

func (i deleteInstrumentation) call()                          { i.DeleteCall() }
func (i deleteInstrumentation) recordCount(n int)              { i.DeleteRecordCount(n) }
func (i deleteInstrumentation) callDuration(d time.Duration)   { i.DeleteCallDuration(d) }
func (i deleteInstrumentation) recordDuration(d time.Duration) { i.DeleteRecordDuration(d) }
func (i deleteInstrumentation) quorumFailure()                 { i.DeleteQuorumFailure() }
