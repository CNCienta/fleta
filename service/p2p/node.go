package p2p

import (
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fletaio/fleta/common"
	"github.com/fletaio/fleta/common/hash"
	"github.com/fletaio/fleta/common/key"
	"github.com/fletaio/fleta/common/queue"
	"github.com/fletaio/fleta/core/chain"
	"github.com/fletaio/fleta/core/txpool"
	"github.com/fletaio/fleta/core/types"
	"github.com/fletaio/fleta/encoding"
)

// Node receives a block by the consensus
type Node struct {
	sync.Mutex
	key          key.Key
	ms           *NodeMesh
	cn           *chain.Chain
	myPublicHash common.PublicHash
	requestTimer *RequestTimer
	blockQ       *queue.SortedQueue
	txMsgChans   []*chan *TxMsgItem
	txMsgIdx     uint64
	txpool       *txpool.TransactionPool
	txQ          *queue.ExpireQueue
	isRunning    bool
	closeLock    sync.RWMutex
	runEnd       chan struct{}
	isClose      bool
}

// NewNode returns a Node
func NewNode(key key.Key, SeedNodeMap map[common.PublicHash]string, cn *chain.Chain) *Node {
	nd := &Node{
		key:          key,
		cn:           cn,
		myPublicHash: common.NewPublicHash(key.PublicKey()),
		blockQ:       queue.NewSortedQueue(),
		txpool:       txpool.NewTransactionPool(),
		txQ:          queue.NewExpireQueue(),
	}
	nd.ms = NewNodeMesh(key, SeedNodeMap, nd)
	nd.requestTimer = NewRequestTimer(nd)
	nd.txQ.AddGroup(60 * time.Second)
	nd.txQ.AddGroup(600 * time.Second)
	nd.txQ.AddGroup(3600 * time.Second)
	nd.txQ.AddHandler(nd)
	return nd
}

// Init initializes node
func (nd *Node) Init() error {
	fc := encoding.Factory("message")
	fc.Register(types.DefineHashedType("p2p.PingMessage"), &PingMessage{})
	fc.Register(types.DefineHashedType("p2p.StatusMessage"), &StatusMessage{})
	fc.Register(types.DefineHashedType("p2p.BlockMessage"), &BlockMessage{})
	fc.Register(types.DefineHashedType("p2p.TransactionMessage"), &TransactionMessage{})
	return nil
}

// Close terminates the node
func (nd *Node) Close() {
	nd.closeLock.Lock()
	defer nd.closeLock.Unlock()

	nd.Lock()
	defer nd.Unlock()

	nd.isClose = true
	nd.cn.Close()
	nd.runEnd <- struct{}{}
}

// OnItemExpired is called when the item is expired
func (nd *Node) OnItemExpired(Interval time.Duration, Key string, Item interface{}, IsLast bool) {
	msg := Item.(*TransactionMessage)
	nd.ms.BroadcastMessage(msg)
	if IsLast {
		var TxHash hash.Hash256
		copy(TxHash[:], []byte(Key))
		nd.txpool.Remove(TxHash, msg.Tx)
	}
}

// Run starts the node
func (nd *Node) Run(BindAddress string) {
	nd.Lock()
	if nd.isRunning {
		nd.Unlock()
		return
	}
	nd.isRunning = true
	nd.Unlock()

	go nd.ms.Run(BindAddress)
	go nd.requestTimer.Run()

	WorkerCount := runtime.NumCPU() - 1
	if WorkerCount < 1 {
		WorkerCount = 1
	}
	workerEnd := make([]*chan struct{}, WorkerCount)
	nd.txMsgChans = make([]*chan *TxMsgItem, WorkerCount)
	for i := 0; i < WorkerCount; i++ {
		mch := make(chan *TxMsgItem)
		nd.txMsgChans[i] = &mch
		ch := make(chan struct{})
		workerEnd[i] = &ch
		go func(pMsgCh *chan *TxMsgItem, pEndCh *chan struct{}) {
			for {
				select {
				case item := <-(*pMsgCh):
					if err := nd.AddTx(item.Message.Tx, item.Message.Sigs); err != nil {
						if err != txpool.ErrPastSeq || err != txpool.ErrTooFarSeq {
							(*item.ErrCh) <- err
						} else {
							(*item.ErrCh) <- nil
						}
						break
					}
					(*item.ErrCh) <- nil
					if len(item.PeerID) > 0 {
						//nd.pm.ExceptCast(item.PeerID, item.Message)
						//nd.pm.ExceptCastLimit(item.PeerID, item.Message, 7)
					} else {
						//nd.pm.BroadCast(item.Message)
						//nd.pm.BroadCastLimit(item.Message, 7)
					}
				case <-(*pEndCh):
					return
				}
			}
		}(&mch, &ch)
	}

	blockTimer := time.NewTimer(time.Millisecond)
	for !nd.isClose {
		select {
		case <-blockTimer.C:
			cp := nd.cn.Provider()
			nd.Lock()
			TargetHeight := uint64(cp.Height() + 1)
			item := nd.blockQ.PopUntil(TargetHeight)
			for item != nil {
				b := item.(*types.Block)
				if err := nd.cn.ConnectBlock(b); err != nil {
					log.Println(err)
					panic(err)
					break
				}
				log.Println("Node", nd.myPublicHash.String(), cp.Height(), "BlockConnected", b.Header.Height)
				nd.broadcastStatus()
				TargetHeight++
				item = nd.blockQ.PopUntil(TargetHeight)
			}
			nd.Unlock()
			blockTimer.Reset(50 * time.Millisecond)
		case <-nd.runEnd:
			return
		}
	}
}

// OnTimerExpired called when rquest expired
func (nd *Node) OnTimerExpired(height uint32, value interface{}) {
	TargetPublicHash := value.(common.PublicHash)
	list := nd.ms.Peers()
	for _, p := range list {
		var pubhash common.PublicHash
		copy(pubhash[:], []byte(p.ID()))
		if pubhash != nd.myPublicHash && pubhash != TargetPublicHash {
			nd.sendRequestBlockTo(pubhash, height)
			break
		}
	}
}

// OnRecv called when message received
func (nd *Node) OnRecv(p Peer, m interface{}) error {
	cp := nd.cn.Provider()

	var SenderPublicHash common.PublicHash
	copy(SenderPublicHash[:], []byte(p.ID()))

	switch msg := m.(type) {
	case *RequestMessage:
		b, err := cp.Block(msg.Height)
		if err != nil {
			return err
		}
		sm := &BlockMessage{
			Block: b,
		}
		if err := nd.ms.SendTo(SenderPublicHash, sm); err != nil {
			return err
		}
		return nil
	case *StatusMessage:
		Height := cp.Height()
		if Height < msg.Height {
			for i := Height + 1; i <= Height+100 && i <= msg.Height; i++ {
				if !nd.requestTimer.Exist(i) {
					nd.sendRequestBlockTo(SenderPublicHash, i)
				}
			}
		} else {
			h, err := cp.Hash(msg.Height)
			if err != nil {
				return err
			}
			if h != msg.LastHash {
				//TODO : critical error signal
				log.Println(p.Name(), h.String(), msg.LastHash.String(), msg.Height)
				panic(chain.ErrFoundForkedBlock)
			}
		}
		return nil
	case *BlockMessage:
		if err := nd.addBlock(msg.Block); err != nil {
			return err
		}
		nd.requestTimer.Remove(msg.Block.Header.Height)
		return nil
	case *TransactionMessage:
		errCh := make(chan error)
		idx := atomic.AddUint64(&nd.txMsgIdx, 1) % uint64(len(nd.txMsgChans))
		(*nd.txMsgChans[idx]) <- &TxMsgItem{
			Message: msg,
			PeerID:  "",
			ErrCh:   &errCh,
		}
		err := <-errCh
		if err != ErrInvalidUTXO && err != txpool.ErrExistTransaction {
			return err
		}
		return nil
	default:
		panic(ErrUnknownMessage) //TEMP
		return ErrUnknownMessage
	}
	return nil
}

func (nd *Node) addBlock(b *types.Block) error {
	cp := nd.cn.Provider()
	if b.Header.Height <= cp.Height() {
		h, err := cp.Hash(b.Header.Height)
		if err != nil {
			return err
		}
		if h != encoding.Hash(b.Header) {
			//TODO : critical error signal
			panic(chain.ErrFoundForkedBlock)
		}
	} else {
		if item := nd.blockQ.FindOrInsert(b, uint64(b.Header.Height)); item != nil {
			old := item.(*types.Block)
			if encoding.Hash(old.Header) != encoding.Hash(b.Header) {
				//TODO : critical error signal
				panic(chain.ErrFoundForkedBlock)
			}
		}
	}
	return nil
}

// AddTx adds tx to txpool that only have valid signatures
func (nd *Node) AddTx(tx types.Transaction, sigs []common.Signature) error {
	if nd.txpool.Size() > 65535 {
		return txpool.ErrTransactionPoolOverflowed
	}

	fc := encoding.Factory("transaction")
	t, err := fc.TypeOf(tx)
	if err != nil {
		return err
	}

	TxHash := chain.HashTransactionByType(t, tx)

	ctx := nd.cn.NewContext()
	if nd.txpool.IsExist(TxHash) {
		return txpool.ErrExistTransaction
	}
	if atx, is := tx.(txpool.AccountTransaction); is {
		seq := ctx.Seq(atx.From())
		if atx.Seq() <= seq {
			return txpool.ErrPastSeq
		} else if atx.Seq() > seq+100 {
			return txpool.ErrTooFarSeq
		}
	}
	signers := make([]common.PublicHash, 0, len(sigs))
	for _, sig := range sigs {
		pubkey, err := common.RecoverPubkey(TxHash, sig)
		if err != nil {
			return err
		}
		signers = append(signers, common.NewPublicHash(pubkey))
	}
	pid := uint8(t >> 8)
	p, err := nd.cn.Process(pid)
	if err != nil {
		return err
	}
	ctw := types.NewContextWrapper(pid, ctx)
	if err := tx.Validate(p, ctw, signers); err != nil {
		return err
	}
	if err := nd.txpool.Push(t, TxHash, tx, sigs, signers); err != nil {
		return err
	}
	nd.txQ.Push(string(TxHash[:]), &TransactionMessage{
		TxType: t,
		Tx:     tx,
		Sigs:   sigs,
	})
	return nil
}

func (nd *Node) cleanPool(b *types.Block) {
	for i, tx := range b.Transactions {
		t := b.TransactionTypes[i]
		TxHash := chain.HashTransactionByType(t, tx)
		nd.txpool.Remove(TxHash, tx)
		nd.txQ.Remove(string(TxHash[:]))
	}
}

// TxMsgItem used to store transaction message
type TxMsgItem struct {
	Message *TransactionMessage
	PeerID  string
	ErrCh   *chan error
}
