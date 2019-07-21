// Copyright 2019 The Swarm Authors
// This file is part of the Swarm library.
//
// The Swarm library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Swarm library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Swarm library. If not, see <http://www.gnu.org/licenses/>.

package retrieve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/simulations/adapters"
	"github.com/ethersphere/swarm/chunk"
	"github.com/ethersphere/swarm/network"
	"github.com/ethersphere/swarm/network/simulation"
	"github.com/ethersphere/swarm/p2p/protocols"
	"github.com/ethersphere/swarm/state"
	"github.com/ethersphere/swarm/storage"
	"github.com/ethersphere/swarm/storage/localstore"
	"github.com/ethersphere/swarm/storage/mock"
	"github.com/ethersphere/swarm/testutil"
	"golang.org/x/crypto/sha3"
)

var (
	loglevel           = flag.Int("loglevel", 5, "verbosity of logs")
	bucketKeyFileStore = simulation.BucketKey("filestore")
	bucketKeyNetstore  = simulation.BucketKey("netstore")

	hash0         = sha3.Sum256([]byte{0})
	hash1         = sha3.Sum256([]byte{1})
	hash2         = sha3.Sum256([]byte{2})
	hashesTmp     = append(hash0[:], hash1[:]...)
	hashes        = append(hashesTmp, hash2[:]...)
	corruptHashes = append(hashes[:40])
)

func init() {
	flag.Parse()

	log.PrintOrigins(true)
	log.Root().SetHandler(log.LvlFilterHandler(log.Lvl(*loglevel), log.StreamHandler(os.Stderr, log.TerminalFormat(false))))
}

// TestChunkDelivery brings up two nodes, stores a few chunks on the first node, then tries to retrieve them through the second node
func TestChunkDelivery(t *testing.T) {
	chunkCount := 10
	filesize := chunkCount * 4096

	sim := simulation.NewBzzInProc(map[string]simulation.ServiceFunc{
		"bzz-retrieve": newBzzRetrieveWithLocalstore,
	})
	defer sim.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, err := sim.AddNode()
	if err != nil {
		t.Fatal(err)
	}

	result := sim.Run(ctx, func(ctx context.Context, sim *simulation.Simulation) error {
		nodeIDs := sim.UpNodeIDs()
		log.Debug("uploader node", "enode", nodeIDs[0])

		item := sim.NodeItem(nodeIDs[0], bucketKeyFileStore)

		//put some data into just the first node
		data := make([]byte, filesize)
		if _, err := io.ReadFull(rand.Reader, data); err != nil {
			t.Fatalf("reading from crypto/rand failed: %v", err.Error())
		}
		refs, err := getAllRefs(data)
		if err != nil {
			return nil
		}
		log.Trace("got all refs", "refs", refs)
		_, wait, err := item.(*storage.FileStore).Store(context.Background(), bytes.NewReader(data), int64(filesize), false)
		if err != nil {
			return err
		}
		if err := wait(context.Background()); err != nil {
			return err
		}

		id, err := sim.AddNodes(1)
		if err != nil {
			return err
		}
		err = sim.Net.ConnectNodesStar(id, nodeIDs[0])
		if err != nil {
			return err
		}
		nodeIDs = sim.UpNodeIDs()
		if len(nodeIDs) != 2 {
			return fmt.Errorf("wrong number of nodes, expected %d got %d", 2, len(nodeIDs))
		}
		time.Sleep(100 * time.Millisecond)
		log.Debug("fetching through node", "enode", nodeIDs[1])
		ns := sim.NodeItem(nodeIDs[1], bucketKeyNetstore).(*storage.NetStore)
		ctr := 0
		for _, ch := range refs {
			ctr++
			_, err := ns.Get(context.Background(), chunk.ModeGetRequest, storage.NewRequest(ch))
			if err != nil {
				return err
			}
		}
		if ctr != len(refs) {
			return fmt.Errorf("did not process enough refs. got %d want %d", ctr, len(refs))
		}
		return nil
	})
	if result.Error != nil {
		t.Fatal(result.Error)
	}
}

func TestDeliveryForwarding(t *testing.T) {
	chunkCount := 100
	filesize := chunkCount * 4096
	sim, uploader, forwarder, fetcher := setupTestDeliveryForwardingSimulation(t)
	defer sim.Close()
	log.Debug("test delivery forwarding", "uploader", uploader, "forwarder", forwarder, "fetcher", fetcher)
	uploaderNodeStore := sim.NodeItem(uploader, bucketKeyFileStore).(*storage.FileStore)
	fetcherKad := sim.NodeItem(fetcher, simulation.BucketKeyKademlia).(*network.Kademlia).BaseAddr()
	uploaderKad := sim.NodeItem(fetcher, simulation.BucketKeyKademlia).(*network.Kademlia).BaseAddr()
	ctx := context.Background()
	_, wait, err := uploaderNodeStore.Store(ctx, testutil.RandomReader(101010, filesize), int64(filesize), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = wait(ctx); err != nil {
		t.Fatal(err)
	}

	chunks, err := getChunks(uploaderNodeStore.ChunkStore)
	if err != nil {
		t.Fatal(err)
	}
	for c, _ := range chunks {
		addr, err := hex.DecodeString(c)
		if err != nil {
			t.Fatal(err)
		}

		// try to retrieve all of the chunks which have no bits in common with the
		// fetcher, but have more than one bit in common with the uploader node
		if chunk.Proximity(addr, fetcherKad) == 0 && chunk.Proximity(addr, uploaderKad) > 1 {
			req := storage.NewRequest(chunk.Address(addr))
			fetcherNetstore := sim.NodeItem(fetcher, bucketKeyNetstore).(*storage.NetStore)
			_, err := fetcherNetstore.Get(ctx, chunk.ModeGetRequest, req)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func setupTestDeliveryForwardingSimulation(t *testing.T) (sim *simulation.Simulation, uploader, forwarder, fetching enode.ID) {
	// initial node count

	sim = simulation.NewBzzInProc(map[string]simulation.ServiceFunc{
		"bzz-retrieve": newBzzRetrieveWithLocalstore,
	})

	fetching, err := sim.AddNode()
	if err != nil {
		t.Fatal(err)
	}

	fetcherBase := sim.NodeItem(fetching, simulation.BucketKeyKademlia).(*network.Kademlia).BaseAddr()

	override := func(o *adapters.NodeConfig) func(*adapters.NodeConfig) {
		return func(c *adapters.NodeConfig) {
			*o = *c
		}
	}

	// create a node that will be in po 1 from fetcher
	forwarderConfig := createNodeConfigAtPo(t, fetcherBase, 1)
	forwarder, err = sim.AddNode(override(forwarderConfig))
	if err != nil {
		t.Fatal(err)
	}

	err = sim.Net.Connect(fetching, forwarder)
	if err != nil {
		t.Fatal(err)
	}

	forwarderBase := sim.NodeItem(forwarder, simulation.BucketKeyKademlia).(*network.Kademlia).BaseAddr()

	uploaderConfig := createNodeConfigAtPo(t, forwarderBase, 2)
	uploader, err = sim.AddNode(override(uploaderConfig))
	if err != nil {
		t.Fatal(err)
	}

	err = sim.Net.Connect(forwarder, uploader)
	if err != nil {
		t.Fatal(err)
	}

	return sim, uploader, forwarder, fetching
}

// createNodeConfigAtPo brute forces a node config to create a node that has an overlay address at the provided po in relation to the given baseaddr
func createNodeConfigAtPo(t *testing.T, baseaddr []byte, po int) *adapters.NodeConfig {
	foundPo := -1
	var conf *adapters.NodeConfig
	for foundPo != po {
		conf = adapters.RandomNodeConfig()
		ip := net.IPv4(127, 0, 0, 1)
		enrIp := enr.IP(ip)
		conf.Record.Set(&enrIp)
		enrTcpPort := enr.TCP(conf.Port)
		conf.Record.Set(&enrTcpPort)
		enrUdpPort := enr.UDP(0)
		conf.Record.Set(&enrUdpPort)

		err := enode.SignV4(&conf.Record, conf.PrivateKey)
		if err != nil {
			t.Fatalf("unable to generate ENR: %v", err)
		}
		nod, err := enode.New(enode.V4ID{}, &conf.Record)
		if err != nil {
			t.Fatalf("unable to create enode: %v", err)
		}

		n := network.NewAddr(nod)
		foundPo = chunk.Proximity(baseaddr, n.Over())
	}

	return conf
}

/*
more test cases:
1. connect 3 nodes in chain and make sure that the retrieve request is being forwarded between the nodes
2. make sure that the whole thing plays out with a root hash and an actual trie that needs to be retrieved
3. make sure it works with manifests too
*/

// if there is one peer in the Kademlia, RequestFromPeers should return it
func TestRequestFromPeers(t *testing.T) {
	dummyPeerID := enode.HexID("3431c3939e1ee2a6345e976a8234f9870152d64879f30bc272a074f6859e75e8")

	addr := network.RandomAddr()
	to := network.NewKademlia(addr.OAddr, network.NewKadParams())
	protocolsPeer := protocols.NewPeer(p2p.NewPeer(dummyPeerID, "dummy", nil), nil, nil)
	peer := network.NewPeer(&network.BzzPeer{
		BzzAddr:   network.RandomAddr(),
		LightNode: false,
		Peer:      protocolsPeer,
	}, to)

	to.On(peer)

	s := NewRetrieval(to, nil)

	req := storage.NewRequest(storage.Address(hash0[:]))
	id, err := s.findPeer(context.TODO(), req)
	if err != nil {
		t.Fatal(err)
	}

	if id.ID() != dummyPeerID {
		t.Fatalf("Expected an id, got %v", id)
	}
}

// RequestFromPeers should not return light nodes
func TestRequestFromPeersWithLightNode(t *testing.T) {
	dummyPeerID := enode.HexID("3431c3939e1ee2a6345e976a8234f9870152d64879f30bc272a074f6859e75e8")

	addr := network.RandomAddr()
	to := network.NewKademlia(addr.OAddr, network.NewKadParams())

	protocolsPeer := protocols.NewPeer(p2p.NewPeer(dummyPeerID, "dummy", nil), nil, nil)

	// setting up a lightnode
	peer := network.NewPeer(&network.BzzPeer{
		BzzAddr:   network.RandomAddr(),
		LightNode: true,
		Peer:      protocolsPeer,
	}, to)

	to.On(peer)

	r := NewRetrieval(to, nil)
	req := storage.NewRequest(storage.Address(hash0[:]))

	// making a request which should return with "no peer found"
	_, err := r.findPeer(context.TODO(), req)

	expectedError := "no peer found"
	if err.Error() != expectedError {
		t.Fatalf("expected '%v', got %v", expectedError, err)
	}
}

func newBzzRetrieveWithLocalstore(ctx *adapters.ServiceContext, bucket *sync.Map) (s node.Service, cleanup func(), err error) {
	n := ctx.Config.Node()
	addr := network.NewAddr(n)

	localStore, localStoreCleanup, err := newTestLocalStore(n.ID(), addr, nil)
	if err != nil {
		return nil, nil, err
	}

	var kad *network.Kademlia
	if kv, ok := bucket.Load(simulation.BucketKeyKademlia); ok {
		kad = kv.(*network.Kademlia)
	} else {
		//eee := fmt.Sprintf("over %s, under %s", hex.EncodeToString(addr.Over()), hex.EncodeToString(addr.Under()))
		//panic(eee)
		kad = network.NewKademlia(addr.Over(), network.NewKadParams())
		bucket.Store(simulation.BucketKeyKademlia, kad)
	}

	netStore := storage.NewNetStore(localStore, n.ID())
	lnetStore := storage.NewLNetStore(netStore)
	fileStore := storage.NewFileStore(lnetStore, storage.NewFileStoreParams(), chunk.NewTags())

	var store *state.DBStore
	// Use on-disk DBStore to reduce memory consumption in race tests.
	dir, err := ioutil.TempDir("", "statestore-")
	if err != nil {
		return nil, nil, err
	}
	store, err = state.NewDBStore(dir)
	if err != nil {
		return nil, nil, err
	}

	r := NewRetrieval(kad, netStore)
	netStore.RemoteGet = r.RequestFromPeers
	bucket.Store(bucketKeyFileStore, fileStore)
	bucket.Store(bucketKeyNetstore, netStore)
	bucket.Store(simulation.BucketKeyKademlia, kad)

	cleanup = func() {
		localStore.Close()
		localStoreCleanup()
		store.Close()
		os.RemoveAll(dir)
	}

	return r, cleanup, nil
}

func newTestLocalStore(id enode.ID, addr *network.BzzAddr, globalStore mock.GlobalStorer) (localStore *localstore.DB, cleanup func(), err error) {
	dir, err := ioutil.TempDir("", "localstore-")
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() {
		os.RemoveAll(dir)
	}

	var mockStore *mock.NodeStore
	if globalStore != nil {
		mockStore = globalStore.NewNodeStore(common.BytesToAddress(id.Bytes()))
	}

	localStore, err = localstore.New(dir, addr.Over(), &localstore.Options{
		MockStore: mockStore,
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return localStore, cleanup, nil
}

func getAllRefs(testData []byte) (storage.AddressCollection, error) {
	datadir, err := ioutil.TempDir("", "chunk-debug")
	if err != nil {
		return nil, fmt.Errorf("unable to create temp dir: %v", err)
	}
	defer os.RemoveAll(datadir)
	fileStore, cleanup, err := storage.NewLocalFileStore(datadir, make([]byte, 32), chunk.NewTags())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	reader := bytes.NewReader(testData)
	return fileStore.GetAllReferences(context.Background(), reader, false)
}

func getChunks(store chunk.Store) (chunks map[string]struct{}, err error) {
	chunks = make(map[string]struct{})
	for po := uint8(0); po <= chunk.MaxPO; po++ {
		last, err := store.LastPullSubscriptionBinID(uint8(po))
		if err != nil {
			return nil, err
		}
		if last == 0 {
			continue
		}
		ch, _ := store.SubscribePull(context.Background(), po, 0, last)
		for c := range ch {
			addr := c.Address.Hex()
			if _, ok := chunks[addr]; ok {
				return nil, fmt.Errorf("duplicate chunk %s", addr)
			}
			chunks[addr] = struct{}{}
		}
	}
	return chunks, nil
}
