package replica

// This file implements replica module.
// @decision (02/01/14):
// - Replica Epoch starts from 1. 0 is reserved for fresly created instance.
// - Now freshly created instance always has the minimum ballot.
// @decision(02/15/14):
// - An instance will always set dependency on its immediate precessor in the same
// - instance space. This enforces sequential execution in single instance space.
// @decision(02/17/14):
// - Add checkpoint cycle. Any instance conflicts with a checkpoint and vice versa.
// - This is used to decrease the size of conflict scanning space.

import (
	"container/list"
	"fmt"
	"time"

	"github.com/go-distributed/epaxos"
	"github.com/go-distributed/epaxos/data"
)

var _ = fmt.Printf

// ****************************
// *****  CONST ENUM **********
// ****************************
const (
	defaultInstancesLength = 1024 * 64
	conflictNotFound       = 0
	epochStart             = 1
	executeInterval        = 5 * time.Millisecond
)

// actions
const (
	noAction uint8 = iota + 1
	replyAction
	fastQuorumAction
	broadcastAction
)

// ****************************
// ***** TYPE STRUCT **********
// ****************************

type Replica struct {
	Id               uint8
	Size             uint8
	MaxInstanceNum   []uint64 // the highest instance number seen for each replica
	ProposeNum       uint64
	CheckpointCycle  uint64
	ExecutedUpTo     []uint64
	InstanceMatrix   [][]*Instance
	StateMachine     epaxos.StateMachine
	Epoch            uint32
	MessageEventChan chan *MessageEvent
	Transporter

	// tarjan SCC
	sccStack  *list.List
	sccResult *list.List
	sccIndex  int
}

type Param struct {
	ReplicaId       uint8
	Size            uint8
	StateMachine    epaxos.StateMachine
	CheckpointCycle uint64
}

type MessageEvent struct {
	From    uint8
	Message Message
}

//func New(replicaId, size uint8, sm epaxos.StateMachine) (r *Replica) {
func New(param *Param) (r *Replica) {
	replicaId := param.ReplicaId
	size := param.Size
	sm := param.StateMachine
	cycle := uint64(1024)

	if size%2 == 0 {
		// TODO: epaxos replica error
		panic("size should be an odd number")
	}
	r = &Replica{
		Id:               replicaId,
		Size:             size,
		MaxInstanceNum:   make([]uint64, size),
		ProposeNum:       1,
		CheckpointCycle:  cycle,
		ExecutedUpTo:     make([]uint64, size),
		InstanceMatrix:   make([][]*Instance, size),
		StateMachine:     sm,
		Epoch:            epochStart,
		MessageEventChan: make(chan *MessageEvent),
	}

	for i := uint8(0); i < size; i++ {
		r.InstanceMatrix[i] = make([]*Instance, defaultInstancesLength)
		r.MaxInstanceNum[i] = conflictNotFound
	}

	return r
}

// Start running the replica. It shouldn't stop at any time.
func (r *Replica) Start() {
	go r.eventLoop()
	go r.executeLoop()
}

// handling events
func (r *Replica) eventLoop() {
	go func() {
		for {
			// TODO: check timeout
			// add time.After for timeout checking
			select {
			case mevent := <-r.MessageEventChan:
				r.dispatch(mevent)
			}
		}
	}()
}

func (r *Replica) executeLoop() {
	go func() {
		for {
			time.Sleep(executeInterval)
			// execution of committed instances
			r.findAndExecute()
		}
	}()
}

// TODO: This must be done in a synchronized/atomic way.
func (r *Replica) Propose(cmds data.Commands) {
	mevent := &MessageEvent{
		From: r.Id,
		Message: &data.Propose{
			ReplicaId:  r.Id,
			InstanceId: r.ProposeNum,
			Cmds:       cmds,
		},
	}
	r.ProposeNum++
	if r.IsCheckpoint(r.ProposeNum) {
		r.ProposeNum++
	}
	r.MessageEventChan <- mevent
}

// *****************************
// ***** Message Handling ******
// *****************************

// This function is responsible for communicating with instance processing.
func (r *Replica) dispatch(mevent *MessageEvent) {
	eventMsg := mevent.Message
	replicaId := eventMsg.Replica()
	instanceId := eventMsg.Instance()

	if instanceId <= conflictNotFound {
		panic("")
	}

	if r.InstanceMatrix[replicaId][instanceId] == nil {
		r.InstanceMatrix[replicaId][instanceId] = NewInstance(r, replicaId, instanceId)
	}

	i := r.InstanceMatrix[replicaId][instanceId]
	var action uint8
	var msg Message

	switch i.status {
	case nilStatus:
		action, msg = i.nilStatusProcess(eventMsg)
	case preAccepted:
		action, msg = i.preAcceptedProcess(eventMsg)
	case accepted:
		action, msg = i.acceptedProcess(eventMsg)
	case committed:
		action, msg = i.committedProcess(eventMsg)
	default:
		panic("")
	}

	switch action {
	case noAction:
		return
	case replyAction:
		r.Transporter.Send(mevent.From, msg)
	case fastQuorumAction:
		r.Transporter.MulticastFastquorum(msg)
	case broadcastAction:
		r.Transporter.Broadcast(msg)
	default:
		panic("")

	}
}

// **************************
// Instance Related Fields
// **************************

func (r *Replica) fastQuorum() int {
	if r.Size < 2 {
		panic("")
	}
	return int(r.Size - 2)
}

func (r *Replica) quorum() int {
	return int(r.Size / 2)
}

func (r *Replica) makeInitialBallot() *data.Ballot {
	return data.NewBallot(r.Epoch, 0, r.Id)
}

func (r *Replica) makeInitialDeps() data.Dependencies {
	return make(data.Dependencies, r.Size)
}

// Thins func initiate a new instance, construct its commands and dependencies
func (r *Replica) initInstance(cmds data.Commands, i *Instance) {
	if i.rowId != r.Id {
		panic("")
	}

	deps := make(data.Dependencies, r.Size)

	for curr := range r.InstanceMatrix {
		instances := r.InstanceMatrix[curr]
		start := r.MaxInstanceNum[curr]

		if curr == int(i.rowId) {
			// set deps on its immediate precessor
			conflict := uint64(i.id - 1)
			deps[curr] = conflict
			continue
		}

		conflict, _ := r.scanConflicts(instances, cmds, start, conflictNotFound)
		deps[curr] = conflict
	}
	i.cmds, i.deps = cmds, deps
	// we can only update here because
	// now we are safe to have cmds, etc. inside instance
	if !i.replica.updateMaxInstanceNum(i.rowId, i.id) {
		panic("")
	}
}

// This func updates the passed in dependencies from replica[from].
// return updated dependencies and whether the dependencies has changed.
// TODO: this operation should synchronized/atomic
func (r *Replica) updateInstance(cmds data.Commands, deps data.Dependencies, from uint8, i *Instance) bool {
	changed := false

	for curr := range r.InstanceMatrix {
		// the sender knows the latest dependencies for its instance space
		if curr == int(from) {
			continue
		}

		instances := r.InstanceMatrix[curr]
		start, end := r.MaxInstanceNum[curr], deps[curr]

		if conflict, ok := r.scanConflicts(instances, cmds, start, end); ok {
			changed = true
			deps[curr] = conflict
		}
	}

	i.cmds, i.deps = cmds, deps
	// we can only update here because
	// now we are safe to have cmds, etc. inside instance
	i.replica.updateMaxInstanceNum(i.rowId, i.id)
	return changed
}

func (r *Replica) IsCheckpoint(n uint64) bool {
	return n%r.CheckpointCycle == 0
}

// scanConflicts scans the instances from start to end (high to low).
// return the highest instance that has conflicts with passed in cmds.
func (r *Replica) scanConflicts(instances []*Instance, cmds data.Commands, start uint64, end uint64) (uint64, bool) {
	for i := start; i > end; i-- {
		if r.IsCheckpoint(i) {
			return i, true
		}
		if instances[i] == nil {
			continue
		}
		// we only need to find the highest instance in conflict
		if r.StateMachine.HaveConflicts(cmds, instances[i].cmds) {
			return i, true
		}
	}

	return conflictNotFound, false
}

func (r *Replica) updateMaxInstanceNum(rowId uint8, instanceId uint64) bool {
	if r.MaxInstanceNum[rowId] < instanceId {
		r.MaxInstanceNum[rowId] = instanceId
		return true
	}
	return false
}

// ******************************
// ********  EXECUTION **********
// ******************************

func (r *Replica) findAndExecute() {
	for i := 0; i < int(r.Size); i++ {
		// search this instance space
		for {
			up := r.ExecutedUpTo[i] + 1

			if r.IsCheckpoint(up) {
				r.ExecutedUpTo[i]++
				continue
			}

			instance := r.InstanceMatrix[i][up]
			if instance == nil || !instance.isAtStatus(committed) {
				break
			}
			if instance.executed {
				r.ExecutedUpTo[i]++
				continue
			}

			if ok := r.execute(instance); !ok {
				break
			}
		}
	}
}

// NOTE: atomic
func (r *Replica) execute(i *Instance) bool {
	r.sccStack = list.New()
	r.sccResult = list.New()
	r.sccIndex = 1
	if ok := r.resolveConflicts(i); !ok {
		return false
	}
	// execute elements in the result list
	// the list will maintain the order of nodes that:
	// - The lower level strongly connected component locates at smaller index.
	// - In the same component, nodes at higher instance space index locates
	// - at smaller index.

	r.executeList()
	return true
}

// this should be a transaction.
func (r *Replica) executeList() {
	panic("")
}

// Assumption:
// - If a node is executed, then all the strongly connected components it belongs to or
// - lower than its has been executed. This is useful to reduce search space.
func (r *Replica) resolveConflicts(node *Instance) bool {
	node.sccIndex = r.sccIndex
	node.sccLowlink = r.sccIndex
	r.sccIndex++

	r.sccStack.PushBack(node)
	for iSpace := 0; iSpace < int(r.Size); iSpace++ {
		dep := node.deps[iSpace]
		neighbor := r.InstanceMatrix[iSpace][dep]
		if neighbor.status != committed {
			return false
		}
		if neighbor.executed {
			continue
		}

		if neighbor.sccIndex == 0 {
			if ok := r.resolveConflicts(neighbor); !ok {
				return false
			}
			if neighbor.sccLowlink < node.sccLowlink {
				node.sccLowlink = neighbor.sccLowlink
			}
		} else if r.inSccStack(neighbor) {
			if neighbor.sccLowlink < node.sccLowlink {
				node.sccLowlink = neighbor.sccLowlink
			}
		}
	}

	if node.sccLowlink == node.sccIndex {
		var neighbor *Instance

		for neighbor == nil ||
			node.rowId != neighbor.rowId ||
			node.id != neighbor.id {
			neighbor = r.popSccStack()
			r.sccResult.PushBack(neighbor)
		}
	}

	return true
}

func (r *Replica) inSccStack(other *Instance) bool {
	iter := r.sccStack.Front()
	for iter != nil {
		self := iter.Value.(*Instance)
		if self.rowId == other.rowId && self.id == other.id {
			return true
		}
		iter = iter.Next()
	}
	return false
}

func (r *Replica) popSccStack() *Instance {
	res := r.sccStack.Back().Value.(*Instance)
	r.sccStack.Remove(r.sccStack.Back())
	return res
}
