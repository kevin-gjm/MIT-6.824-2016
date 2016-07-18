package shardmaster


import "raft"
import "labrpc"
import "sync"
import "encoding/gob"
import "container/list"
import "time"
import "sort"
// import "fmt"


type ShardMaster struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg

	// Your data here.

	configs []Config // indexed by config num
	executedID map[int64]int
	notify     map[int][]chan OpReply
	Recv       chan raft.ApplyMsg
	Msgs       *list.List
	QuitCH     chan bool
	lastIncludedIndex int
}


type Op struct {
	// Your data here.
	Client     int64
	Sequence   int
	Type       string
	Servers    map[int][]string
	GIDs       []int
	Shard      int
	GID        int
	Num        int
}

type OpReply struct {
	Client      int64
	Sequence    int
	WrongLeader bool
	Err         Err
	Config      Config
}

func (sm * ShardMaster) clientRequest(op Op, reply *OpReply) {
	sm.mu.Lock()
	if _, ok := sm.executedID[op.Client]; !ok {
		sm.executedID[op.Client] = -1
	}
	index, term, isLeader := sm.rf.Start(op)
	if !isLeader {
		sm.mu.Unlock()
		reply.WrongLeader = true
		return
	}
	if _, ok := sm.notify[index]; !ok {
		sm.notify[index] = make([]chan OpReply, 0)
	}
	notifyMe := make(chan OpReply)
	sm.notify[index] = append(sm.notify[index], notifyMe)
	sm.mu.Unlock()
	
	var executedOp OpReply
	var notified = false
	for {
		select {
		case executedOp = <- notifyMe:
			notified = true
			break
		case <- time.After(10*time.Millisecond):
			sm.mu.Lock()
			if currentTerm, _ := sm.rf.GetState(); term != currentTerm {
				if sm.lastIncludedIndex < index {
					reply.WrongLeader = true
					delete(sm.notify, index)
					sm.mu.Unlock()
					return
				}
			}
			sm.mu.Unlock()
		case <- sm.QuitCH:
			reply.Err = "ServerFail"
			return
		}
		if notified {
			break
		}
	}

	reply.WrongLeader = false
	if executedOp.Client != op.Client || executedOp.Sequence != op.Sequence {
		reply.Err = "FailCommit"
		reply.WrongLeader = true
	} else {
		reply.Err = OK
		reply.Config = executedOp.Config
		reply.Client = executedOp.Client
		reply.Sequence = executedOp.Sequence
	}
}

func (sm *ShardMaster) Join(args *JoinArgs, reply *JoinReply) {
	// Your code here.
	servers := make(map[int][]string)
	for key := range args.Servers {
		servers[key] = args.Servers[key]
	}
	op := Op{Client:args.Client, Sequence:args.Sequence, Type: "Join", Servers:servers}
	opreply := &OpReply{}
	sm.clientRequest(op, opreply)
	reply.WrongLeader = opreply.WrongLeader
	reply.Err = opreply.Err
	return
}

func (sm *ShardMaster) Leave(args *LeaveArgs, reply *LeaveReply) {
	// Your code here.
	op := Op{Client:args.Client, Sequence:args.Sequence, Type: "Leave", GIDs: args.GIDs}
	opreply := &OpReply{}
	sm.clientRequest(op, opreply)
	reply.WrongLeader = opreply.WrongLeader
	reply.Err = opreply.Err
	return
}

func (sm *ShardMaster) Move(args *MoveArgs, reply *MoveReply) {
	// Your code here.
	op := Op{Client:args.Client, Sequence:args.Sequence, Type: "Move", Shard:args.Shard, GID:args.GID}
	opreply := &OpReply{}
	sm.clientRequest(op, opreply)
	reply.WrongLeader = opreply.WrongLeader
	reply.Err = opreply.Err
	return
}

func (sm *ShardMaster) Query(args *QueryArgs, reply *QueryReply) {
	// Your code here.
	op := Op{Client:args.Client, Sequence:args.Sequence, Type: "Query", Num:args.Num}
	opreply := &OpReply{}
	sm.clientRequest(op, opreply)
	reply.WrongLeader = opreply.WrongLeader
	reply.Err = opreply.Err
	reply.Config = opreply.Config
	return
}

func (sm *ShardMaster) getLastConfig() *Config {
	lastConfig := sm.configs[len(sm.configs)-1]
	newConfig := &Config{Num:lastConfig.Num}
	newConfig.Shards = [NShards]int{}
	newConfig.Groups = make(map[int][]string)
	for i := range lastConfig.Shards {
		newConfig.Shards[i] = lastConfig.Shards[i]
	}
	for key := range lastConfig.Groups {
		newConfig.Groups[key] = lastConfig.Groups[key]
	}
	return newConfig
	
}

func (sm *ShardMaster) getShardsCount(cf Config) map[int]int {
	count := make(map[int]int)
	for _, gid := range cf.Shards {
		if _, ok := count[gid]; !ok {
			count[gid] = 1
		} else {
			count[gid] = count[gid] + 1
		}
	}
	return count
}

func (sm *ShardMaster) applyJoin(op Op) {
	
	newConfig := sm.getLastConfig()
	
	shardsCount := sm.getShardsCount(*newConfig)
	for key := range op.Servers {
		newConfig.Groups[key] = op.Servers[key]
		shardsCount[key] = 0
	}
	
	if len(sm.configs) == 1 {
		for gid := range op.Servers {
			shardsCount[gid] = NShards
			for s := range newConfig.Shards {
				newConfig.Shards[s] = gid
			}
			break
		}
	}
	
	mean := NShards / len(newConfig.Groups)
	
	groupsID := []int{}
	for i := range newConfig.Groups {
		groupsID = append(groupsID, i)
	}
	sort.Ints(groupsID)
	
	for i := 0; i < 2; i++ {
		for shard, gid := range newConfig.Shards {
			if shardsCount[gid] > mean+1-i {
				for _, newGid := range groupsID {
					if shardsCount[newGid] < mean+1-i {
						newConfig.Shards[shard] = newGid
						shardsCount[newGid]++
						shardsCount[gid]--
						break
					}
				}
			}
		}
	}
	newConfig.Num++
	sm.configs = append(sm.configs, *newConfig)
}

func (sm *ShardMaster) applyLeave(op Op) {
	newConfig := sm.getLastConfig()
	shardsCount := sm.getShardsCount(*newConfig)
	for _, v := range op.GIDs {
		delete(newConfig.Groups, v)
	}
	mean := NShards / len(newConfig.Groups)

	groupsID := []int{}
	for i := range newConfig.Groups {
		groupsID = append(groupsID, i)
	}
	sort.Ints(groupsID)
	
	for i := 0; i < 2; i++ {
		for shard, gid := range newConfig.Shards {
			if _, ok := newConfig.Groups[gid]; !ok {
				for _, newGid := range groupsID {
					if shardsCount[newGid] < mean+i {
						newConfig.Shards[shard] = newGid
						shardsCount[newGid]++
						shardsCount[gid]--
						break
					}
				}
			}
		}
	}
	newConfig.Num++
	sm.configs = append(sm.configs, *newConfig)
}

func (sm *ShardMaster) applyMove(op Op) {
	newConfig := sm.getLastConfig()
	newConfig.Shards[op.Shard] = op.GID
	newConfig.Num++
	sm.configs = append(sm.configs, *newConfig)
}

func (sm *ShardMaster) applyQuery(op Op, res *OpReply) {
	if op.Num == -1 || op.Num >= len(sm.configs) {
		res.Config = sm.configs[len(sm.configs)-1]
	} else {
		res.Config = sm.configs[op.Num]
	}
}

//
// the tester calls Kill() when a ShardMaster instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (sm *ShardMaster) Kill() {
	sm.rf.Kill()
	// Your code here, if desired.
	close(sm.QuitCH)
}

// needed by shardkv tester
func (sm *ShardMaster) Raft() *raft.Raft {
	return sm.rf
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Paxos to
// form the fault-tolerant shardmaster service.
// me is the index of the current server in servers[].
//
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister) *ShardMaster {
	sm := new(ShardMaster)
	sm.me = me

	sm.configs = make([]Config, 1)
	sm.configs[0].Groups = map[int][]string{}

	gob.Register(Op{})
	sm.applyCh = make(chan raft.ApplyMsg)
	sm.rf = raft.Make(servers, me, persister, sm.applyCh)

	// Your code here.
	sm.executedID = make(map[int64]int)
	sm.notify = make(map[int][]chan OpReply)
	sm.QuitCH = make(chan bool)
	sm.Msgs = list.New()
	sm.Recv = make(chan raft.ApplyMsg)
	sm.lastIncludedIndex = 0
	
	go func() {

		for {
			var (
				recvChan chan raft.ApplyMsg
				recvVal  raft.ApplyMsg
			)
			if sm.Msgs.Len() > 0 {
				recvChan = sm.Recv
				recvVal = sm.Msgs.Front().Value.(raft.ApplyMsg)
			}
			select {
			case msg := <- sm.applyCh:
				sm.Msgs.PushBack(msg)
			case recvChan <- recvVal:
				sm.Msgs.Remove(sm.Msgs.Front())
			case <- sm.QuitCH:
				return
			}
		}
	} ()

	go func() {
		for {
			select {
			case msg := <- sm.Recv:
				sm.mu.Lock()
				sm.applyCommand(msg)
				sm.mu.Unlock()
			case <- sm.QuitCH:
				return
			}
		}
	} ()
	
	return sm
}

func (sm *ShardMaster) applyCommand(msg raft.ApplyMsg) {
	sm.lastIncludedIndex = msg.Index
	op := msg.Command.(Op)
	logIndex := msg.Index
	clientID := op.Client
	opSequence := op.Sequence
	res := &OpReply{Client: clientID, Sequence: opSequence}

	if _, ok := sm.executedID[clientID]; !ok {
		sm.executedID[clientID] = -1
	}
    
	if opSequence > sm.executedID[clientID] {
		switch op.Type {
		case "Join":
			sm.applyJoin(op)
		case "Leave":
			sm.applyLeave(op)
		case "Move":
			sm.applyMove(op)
		case "Query":
			sm.applyQuery(op, res)
		}
		sm.executedID[clientID] = opSequence
	} else {
		if op.Type == "Query" {
			if op.Num == -1 || op.Num >= len(sm.configs) {
				res.Config = sm.configs[len(sm.configs)-1]
			} else {
				res.Config = sm.configs[op.Num]
			}
		}
	}

	if _, ok := sm.notify[logIndex]; !ok {
		return
	}

	for _,  c := range sm.notify[logIndex] {
		sm.mu.Unlock()
		c <- *res
		sm.mu.Lock()
	}

	delete(sm.notify, logIndex)

}

