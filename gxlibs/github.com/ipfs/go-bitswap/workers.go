package bitswap

import (
	"context"
	"math/rand"
	"sync"
	"time"

	bsmsg "github.com/ipsn/go-ipfs/gxlibs/github.com/ipfs/go-bitswap/message"

	cid "github.com/ipsn/go-ipfs/gxlibs/github.com/ipfs/go-cid"
	logging "github.com/ipsn/go-ipfs/gxlibs/github.com/ipfs/go-log"
	process "github.com/ipsn/go-ipfs/gxlibs/github.com/jbenet/goprocess"
	procctx "github.com/ipsn/go-ipfs/gxlibs/github.com/jbenet/goprocess/context"
	peer "github.com/ipsn/go-ipfs/gxlibs/github.com/libp2p/go-libp2p-peer"
)

var TaskWorkerCount = 8

func (bs *Bitswap) startWorkers(px process.Process, ctx context.Context) {
	// Start up a worker to handle block requests this node is making
	px.Go(func(px process.Process) {
		bs.providerQueryManager(ctx)
	})

	// Start up workers to handle requests from other nodes for the data on this node
	for i := 0; i < TaskWorkerCount; i++ {
		i := i
		px.Go(func(px process.Process) {
			bs.taskWorker(ctx, i)
		})
	}

	// Start up a worker to manage periodically resending our wantlist out to peers
	px.Go(func(px process.Process) {
		bs.rebroadcastWorker(ctx)
	})

	// Start up a worker to manage sending out provides messages
	px.Go(func(px process.Process) {
		bs.provideCollector(ctx)
	})

	// Spawn up multiple workers to handle incoming blocks
	// consider increasing number if providing blocks bottlenecks
	// file transfers
	px.Go(bs.provideWorker)
}

func (bs *Bitswap) taskWorker(ctx context.Context, id int) {
	idmap := logging.LoggableMap{"ID": id}
	defer log.Debug("bitswap task worker shutting down...")
	for {
		log.Event(ctx, "Bitswap.TaskWorker.Loop", idmap)
		select {
		case nextEnvelope := <-bs.engine.Outbox():
			select {
			case envelope, ok := <-nextEnvelope:
				if !ok {
					continue
				}
				// update the BS ledger to reflect sent message
				// TODO: Should only track *useful* messages in ledger
				outgoing := bsmsg.New(false)
				for _, block := range envelope.Message.Blocks() {
					log.Event(ctx, "Bitswap.TaskWorker.Work", logging.LoggableF(func() map[string]interface{} {
						return logging.LoggableMap{
							"ID":     id,
							"Target": envelope.Peer.Pretty(),
							"Block":  block.Cid().String(),
						}
					}))
					outgoing.AddBlock(block)
				}
				bs.engine.MessageSent(envelope.Peer, outgoing)

				bs.wm.SendBlocks(ctx, envelope)
				bs.counterLk.Lock()
				for _, block := range envelope.Message.Blocks() {
					bs.counters.blocksSent++
					bs.counters.dataSent += uint64(len(block.RawData()))
				}
				bs.counterLk.Unlock()
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (bs *Bitswap) provideWorker(px process.Process) {

	limit := make(chan struct{}, provideWorkerMax)

	limitedGoProvide := func(k cid.Cid, wid int) {
		defer func() {
			// replace token when done
			<-limit
		}()
		ev := logging.LoggableMap{"ID": wid}

		ctx := procctx.OnClosingContext(px) // derive ctx from px
		defer log.EventBegin(ctx, "Bitswap.ProvideWorker.Work", ev, k).Done()

		ctx, cancel := context.WithTimeout(ctx, provideTimeout) // timeout ctx
		defer cancel()

		if err := bs.network.Provide(ctx, k); err != nil {
			log.Warning(err)
		}
	}

	// worker spawner, reads from bs.provideKeys until it closes, spawning a
	// _ratelimited_ number of workers to handle each key.
	for wid := 2; ; wid++ {
		ev := logging.LoggableMap{"ID": 1}
		log.Event(procctx.OnClosingContext(px), "Bitswap.ProvideWorker.Loop", ev)

		select {
		case <-px.Closing():
			return
		case k, ok := <-bs.provideKeys:
			if !ok {
				log.Debug("provideKeys channel closed")
				return
			}
			select {
			case <-px.Closing():
				return
			case limit <- struct{}{}:
				go limitedGoProvide(k, wid)
			}
		}
	}
}

func (bs *Bitswap) provideCollector(ctx context.Context) {
	defer close(bs.provideKeys)
	var toProvide []cid.Cid
	var nextKey cid.Cid
	var keysOut chan cid.Cid

	for {
		select {
		case blkey, ok := <-bs.newBlocks:
			if !ok {
				log.Debug("newBlocks channel closed")
				return
			}

			if keysOut == nil {
				nextKey = blkey
				keysOut = bs.provideKeys
			} else {
				toProvide = append(toProvide, blkey)
			}
		case keysOut <- nextKey:
			if len(toProvide) > 0 {
				nextKey = toProvide[0]
				toProvide = toProvide[1:]
			} else {
				keysOut = nil
			}
		case <-ctx.Done():
			return
		}
	}
}

func (bs *Bitswap) rebroadcastWorker(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	broadcastSignal := time.NewTicker(rebroadcastDelay.Get())
	defer broadcastSignal.Stop()

	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	for {
		log.Event(ctx, "Bitswap.Rebroadcast.idle")
		select {
		case <-tick.C:
			n := bs.wm.WantCount()
			if n > 0 {
				log.Debug(n, " keys in bitswap wantlist")
			}
		case <-broadcastSignal.C: // resend unfulfilled wantlist keys
			log.Event(ctx, "Bitswap.Rebroadcast.active")
			entries := bs.wm.CurrentWants()
			if len(entries) == 0 {
				continue
			}

			// TODO: come up with a better strategy for determining when to search
			// for new providers for blocks.
			i := rand.Intn(len(entries))
			bs.findKeys <- &blockRequest{
				Cid: entries[i].Cid,
				Ctx: ctx,
			}
		case <-parent.Done():
			return
		}
	}
}

func (bs *Bitswap) providerQueryManager(ctx context.Context) {
	var activeLk sync.Mutex
	kset := cid.NewSet()

	for {
		select {
		case e := <-bs.findKeys:
			select { // make sure its not already cancelled
			case <-e.Ctx.Done():
				continue
			default:
			}

			activeLk.Lock()
			if kset.Has(e.Cid) {
				activeLk.Unlock()
				continue
			}
			kset.Add(e.Cid)
			activeLk.Unlock()

			go func(e *blockRequest) {
				child, cancel := context.WithTimeout(e.Ctx, providerRequestTimeout)
				defer cancel()
				providers := bs.network.FindProvidersAsync(child, e.Cid, maxProvidersPerRequest)
				wg := &sync.WaitGroup{}
				for p := range providers {
					wg.Add(1)
					go func(p peer.ID) {
						defer wg.Done()
						err := bs.network.ConnectTo(child, p)
						if err != nil {
							log.Debug("failed to connect to provider %s: %s", p, err)
						}
					}(p)
				}
				wg.Wait()
				activeLk.Lock()
				kset.Remove(e.Cid)
				activeLk.Unlock()
			}(e)

		case <-ctx.Done():
			return
		}
	}
}
