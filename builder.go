package ring

import "time"

// 1 << 23 is 8388608 which, with 3 replicas, would use about 100M of memory
const _MAX_PARTITION_COUNT = 1 << 23

// Node is a single item assigned to a ring, usually a single device like a
// disk drive.
type Node interface {
	// NodeID uniquely identifies this node; this id is all that is retained
	// when a Builder returns a Ring representation of the data.
	NodeID() uint64
	Active() bool
	// Capacity indicates the amount of data that should be assigned to a node
	// relative to other nodes. It can be in any unit of designation as long as
	// all nodes use the same designation. Most commonly this is the number of
	// gigabytes the node can store, but could be based on CPU capacity or
	// another resource if that makes more sense to balance.
	Capacity() uint32
	// Tiers indicate the layout of the node with respect to other nodes. For
	// example, the lowest tier, tier 0, might be the server ip (where each
	// node represents a drive on that server). The next tier, 1, might then be
	// the power zone the server is in. The number of tiers is flexible, so
	// later an additional tier for geographic region could be added.
	// Here the tier values are represented by ints, presumably as indexes to
	// the actual values stored elsewhere. This is done for speed during
	// rebalancing.
	TierValues() []int
}

type Builder struct {
	version                       int64
	nodes                         []Node
	partitionBits                 uint16
	replicaToPartitionToNodeIndex [][]int32
	pointsAllowed                 int
}

func NewBuilder(replicaCount int) *Builder {
	b := &Builder{
		nodes: make([]Node, 0),
		replicaToPartitionToNodeIndex: make([][]int32, replicaCount),
		pointsAllowed:                 1,
	}
	for replica := 0; replica < replicaCount; replica++ {
		b.replicaToPartitionToNodeIndex[replica] = []int32{-1}
	}
	return b
}

// PointsAllowed is the number of percentage points over or under that the ring
// will try to keep data assignments within. The default is 1 for one percent
// extra or less data.
func (b *Builder) PointsAllowed() int {
	return b.pointsAllowed
}

func (b *Builder) SetPointsAllowed(points int) {
	b.pointsAllowed = points
}

func (b *Builder) NodeCount() int {
	return len(b.nodes)
}

func (b *Builder) Node(nodeIndex int) Node {
	return b.nodes[nodeIndex]
}

// Add will add the node to the builder's list and return its index in that
// list.
func (b *Builder) Add(node Node) int {
	b.nodes = append(b.nodes, node)
	return len(b.nodes) - 1
}

// Ring returns a Ring instance of the data defined by the builder. This will
// cause any pending rebalancing actions to be performed. The Ring returned
// will be immutable; to obtain updated ring data, Ring() must be called again.
// The localNodeID is so the Ring instance can provide local responsibility
// information; you can give 0 if you don't intended to use those features.
func (b *Builder) Ring(localNodeID uint64) Ring {
	if b.resizeIfNeeded() {
		b.version = time.Now().UnixNano()
	}
	if newRebalanceContext(b).rebalance() {
		b.version = time.Now().UnixNano()
	}
	localNodeIndex := int32(0)
	nodeIDs := make([]uint64, len(b.nodes))
	for i := 0; i < len(nodeIDs); i++ {
		nodeIDs[i] = b.nodes[i].NodeID()
		if nodeIDs[i] == localNodeID {
			localNodeIndex = int32(i)
		}
	}
	replicaToPartitionToNodeIndex := make([][]int32, len(b.replicaToPartitionToNodeIndex))
	for i := 0; i < len(replicaToPartitionToNodeIndex); i++ {
		replicaToPartitionToNodeIndex[i] = make([]int32, len(b.replicaToPartitionToNodeIndex[i]))
		copy(replicaToPartitionToNodeIndex[i], b.replicaToPartitionToNodeIndex[i])
	}
	return &ringImpl{
		version:        b.version,
		localNodeIndex: localNodeIndex,
		partitionBits:  b.partitionBits,
		nodeIDs:        nodeIDs,
		replicaToPartitionToNodeIndex: replicaToPartitionToNodeIndex,
	}
}

func (b *Builder) resizeIfNeeded() bool {
	replicaCount := len(b.replicaToPartitionToNodeIndex)
	// Calculate the partition count needed.
	// Each node is examined to see how much under or over weight it would be
	// and increasing the partition count until the difference is under the
	// points allowed.
	totalCapacity := uint64(0)
	for _, node := range b.nodes {
		if node.Active() {
			totalCapacity += (uint64)(node.Capacity())
		}
	}
	partitionCount := len(b.replicaToPartitionToNodeIndex[0])
	partitionBits := b.partitionBits
	pointsAllowed := float64(b.pointsAllowed) * 0.01
	done := false
	for !done {
		done = true
		for _, node := range b.nodes {
			if !node.Active() {
				continue
			}
			desiredPartitionCount := float64(partitionCount) * float64(replicaCount) * (float64(node.Capacity()) / float64(totalCapacity))
			under := (desiredPartitionCount - float64(int(desiredPartitionCount))) / desiredPartitionCount
			over := (float64(int(desiredPartitionCount)+1) - desiredPartitionCount) / desiredPartitionCount
			if under > pointsAllowed || over > pointsAllowed {
				partitionCount <<= 1
				partitionBits++
				if partitionCount >= _MAX_PARTITION_COUNT {
					done = true
					break
				} else {
					done = false
				}
			}
		}
	}
	// Grow the partitionToNodeIndex slices if the partition count grew.
	if partitionCount > len(b.replicaToPartitionToNodeIndex[0]) {
		shift := partitionBits - b.partitionBits
		for replica := 0; replica < replicaCount; replica++ {
			partitionToNodeIndex := make([]int32, partitionCount)
			for partition := 0; partition < partitionCount; partition++ {
				partitionToNodeIndex[partition] = b.replicaToPartitionToNodeIndex[replica][partition>>shift]
			}
			b.replicaToPartitionToNodeIndex[replica] = partitionToNodeIndex
		}
		b.partitionBits = partitionBits
		return true
	}
	// Shrinking the partitionToNodeIndex slices doesn't happen because it would
	// normally cause more data movements than it's worth. Perhaps in the
	// future we can add detection of cases when shrinking makes sense.
	return false
}

type BuilderStats struct {
	ReplicaCount      int
	NodeCount         int
	InactiveNodeCount int
	PartitionBits     uint16
	PartitionCount    int
	PointsAllowed     int
	TotalCapacity     uint64
	// MaxUnderNodePercentage is the percentage a node is underweight, or has
	// less data assigned to it than its capacity would indicate it desires.
	MaxUnderNodePercentage float64
	MaxUnderNodeIndex      int
	// MaxOverNodePercentage is the percentage a node is overweight, or has
	// more data assigned to it than its capacity would indicate it desires.
	MaxOverNodePercentage float64
	MaxOverNodeIndex      int
}

// Stats gives information about the builder and its health; note that this
// will call the Ring method and so could cause rebalancing. The MaxUnder and
// MaxOver values specifically indicate how balanced the builder is at this
// time.
func (b *Builder) Stats() *BuilderStats {
	ring := b.Ring(0)
	stats := &BuilderStats{
		ReplicaCount:      ring.ReplicaCount(),
		NodeCount:         b.NodeCount(),
		PartitionBits:     ring.PartitionBits(),
		PartitionCount:    1 << ring.PartitionBits(),
		PointsAllowed:     b.PointsAllowed(),
		MaxUnderNodeIndex: -1,
		MaxOverNodeIndex:  -1,
	}
	nodeIndexToPartitionCount := make([]int, stats.NodeCount)
	for _, partitionToNodeIndex := range b.replicaToPartitionToNodeIndex {
		for _, nodeIndex := range partitionToNodeIndex {
			nodeIndexToPartitionCount[nodeIndex]++
		}
	}
	for _, node := range b.nodes {
		if node.Active() {
			stats.TotalCapacity += (uint64)(node.Capacity())
		} else {
			stats.InactiveNodeCount++
		}
	}
	for nodeIndex, node := range b.nodes {
		if !node.Active() {
			continue
		}
		desiredPartitionCount := float64(node.Capacity()) / float64(stats.TotalCapacity) * float64(stats.PartitionCount) * float64(stats.ReplicaCount)
		actualPartitionCount := float64(nodeIndexToPartitionCount[nodeIndex])
		if desiredPartitionCount > actualPartitionCount {
			under := 100.0 * (desiredPartitionCount - actualPartitionCount) / desiredPartitionCount
			if under > stats.MaxUnderNodePercentage {
				stats.MaxUnderNodePercentage = under
				stats.MaxUnderNodeIndex = nodeIndex
			}
		} else if desiredPartitionCount < actualPartitionCount {
			over := 100.0 * (actualPartitionCount - desiredPartitionCount) / desiredPartitionCount
			if over > stats.MaxOverNodePercentage {
				stats.MaxOverNodePercentage = over
				stats.MaxOverNodeIndex = nodeIndex
			}
		}
	}
	return stats
}