// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package livepeer

import (
	"bytes"
	"crypto/ecdsa"
	"fmt"
	"strconv"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/chequebook"
	"github.com/ethereum/go-ethereum/contracts/ens"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/golang/glog"
	"github.com/livepeer/livepeer-swarm/livepeer/api"
	httpapi "github.com/livepeer/livepeer-swarm/livepeer/api/http"
	"github.com/livepeer/livepeer-swarm/livepeer/network"
	"github.com/livepeer/livepeer-swarm/livepeer/storage"
	"github.com/livepeer/livepeer-swarm/livepeer/streaming"
	"github.com/livepeer/livepeer-swarm/mediaserver"
	streamingVizClient "github.com/livepeer/streamingviz/client"
	"golang.org/x/net/context"
)

// the swarm stack
type Swarm struct {
	config      *api.Config            // swarm configuration
	api         *api.Api               // high level api layer (fs/manifest)
	dns         api.Resolver           // DNS registrar
	dbAccess    *network.DbAccess      // access to local chunk db iterator and storage counter
	storage     storage.ChunkStore     // internal access to storage, common interface to cloud storage backends
	dpa         *storage.DPA           // distributed preimage archive, the local API to the storage with document level storage/retrieval support
	depo        network.StorageHandler // remote request handler, interface between bzz protocol and the storage
	cloud       storage.CloudStore     // procurement, cloud storage backend (can multi-cloud)
	hive        *network.Hive          // the logistic manager
	backend     chequebook.Backend     // simple blockchain Backend
	privateKey  *ecdsa.PrivateKey
	corsString  string
	swapEnabled bool
	streamer    *streaming.Streamer
	streamDB    *network.StreamDB
	viz         *streamingVizClient.Client
}

type SwarmAPI struct {
	Api     *api.Api
	Backend chequebook.Backend
	PrvKey  *ecdsa.PrivateKey
}

func (self *Swarm) API() *SwarmAPI {
	return &SwarmAPI{
		Api:     self.api,
		Backend: self.backend,
		PrvKey:  self.privateKey,
	}
}

// creates a new swarm service instance
// implements node.Service
// LIVEPEER: Here we can initialize the streamer (handles streaming channels)
func NewSwarm(ctx *node.ServiceContext, backend chequebook.Backend, config *api.Config, swapEnabled, syncEnabled bool, cors string, viz *streamingVizClient.Client) (self *Swarm, err error) {

	if bytes.Equal(common.FromHex(config.PublicKey), storage.ZeroKey) {
		return nil, fmt.Errorf("empty public key")
	}
	if bytes.Equal(common.FromHex(config.BzzKey), storage.ZeroKey) {
		return nil, fmt.Errorf("empty bzz key")
	}

	self = &Swarm{
		config:      config,
		swapEnabled: swapEnabled,
		backend:     backend,
		privateKey:  config.Swap.PrivateKey(),
		corsString:  cors,
	}
	glog.Infof("Setting up Swarm service components")

	hash := storage.MakeHashFunc(config.ChunkerParams.Hash)
	lstore, err := storage.NewLocalStore(hash, config.StoreParams)
	if err != nil {
		return
	}

	// setup local store
	glog.Infof("Set up local storage")

	self.dbAccess = network.NewDbAccess(lstore)
	glog.Infof("Set up local db access (iterator/counter)")

	// set up the kademlia hive
	self.hive = network.NewHive(
		common.HexToHash(self.config.BzzKey), // key to hive (kademlia base address)
		config.HiveParams,                    // configuration parameters
		swapEnabled,                          // SWAP enabled
		syncEnabled,                          // syncronisation enabled
	)
	glog.Infof("Set up swarm network with Kademlia hive")

	// setup cloud storage backend
	self.cloud = network.NewForwarder(self.hive)
	glog.Infof("-> set swarm forwarder as cloud storage backend")
	// setup cloud storage internal access layer

	self.storage = storage.NewNetStore(hash, lstore, self.cloud, config.StoreParams)
	glog.Infof("-> swarm net store shared access layer to Swarm Chunk Store")

	// set up Depo (storage handler = cloud storage access layer for incoming remote requests)
	self.depo = network.NewDepo(hash, lstore, self.storage)
	glog.Infof("-> REmote Access to CHunks")

	self.streamer, err = streaming.NewStreamer(common.HexToHash(self.config.BzzKey))
	if err != nil {
		return
	}

	self.streamDB = network.NewStreamDB()

	self.viz = viz

	// set up DPA, the cloud storage local access layer
	dpaChunkStore := storage.NewDpaChunkStore(lstore, self.storage)
	glog.Infof("-> Local Access to Swarm")
	// Swarm Hash Merklised Chunking for Arbitrary-length Document/File storage
	self.dpa = storage.NewDPA(dpaChunkStore, self.config.ChunkerParams)
	glog.Infof("-> Content Store API")

	// set up high level api
	transactOpts := bind.NewKeyedTransactor(self.privateKey)

	self.dns, err = ens.NewENS(transactOpts, config.EnsRoot, self.backend)
	if err != nil {
		return nil, err
	}
	glog.Infof("-> Swarm Domain Name Registrar @ address %v", config.EnsRoot.Hex())

	self.api = api.NewApi(self.dpa, self.dns)
	// Manifests for Smart Hosting
	glog.Infof("-> Web3 virtual server API")

	return self, nil
}

/*
Start is called when the stack is started
* starts the network kademlia hive peer management
* (starts netStore level 0 api)
* starts DPA level 1 api (chunking -> store/retrieve requests)
* (starts level 2 api)
* starts http proxy server
* registers url scheme handlers for bzz, etc
* TODO: start subservices like sword, swear, swarmdns
*/
// implements the node.Service interface
func (self *Swarm) Start(net *p2p.Server) error {

	connectPeer := func(url string) error {
		node, err := discover.ParseNode(url)
		if err != nil {
			return fmt.Errorf("invalid node URL: %v", err)
		}
		net.AddPeer(node)
		return nil
	}
	// set chequebook
	if self.swapEnabled {
		ctx := context.Background() // The initial setup has no deadline.
		err := self.SetChequebook(ctx)
		if err != nil {
			return fmt.Errorf("Unable to set chequebook for SWAP: %v", err)
		}
		glog.Infof("-> cheque book for SWAP: %v", self.config.Swap.Chequebook())
	} else {
		glog.Infof("SWAP disabled: no cheque book set")
	}

	glog.Infof("Starting Swarm service")
	self.hive.Start(
		discover.PubkeyID(&net.PrivateKey.PublicKey),
		func() string { return net.ListenAddr },
		connectPeer,
	)
	glog.Infof("Swarm network started on bzz address: %v", self.hive.Addr())

	self.dpa.Start()
	glog.Infof("Swarm DPA started")

	// start swarm http proxy server
	if self.config.Port != "" {
		addr := ":" + self.config.Port
		go httpapi.StartHttpServer(self.api, &httpapi.Server{Addr: addr, CorsString: self.corsString})
	}

	glog.Infof("Livepeer.go: RTMPport: %v", self.config.RTMPPort)
	if self.config.RTMPPort != "" {
		//StartRTMPServer spins up a go routine internally.  It would be good to know the convention
		//around this.  Go routines are spun up all over the place in this codebase, it's a little tough
		//to understand whether you are in the main thread sometimes (or does that just not matter in Go?)
		rtmpPort := self.config.RTMPPort
		rtmpPortNum, _ := strconv.Atoi(rtmpPort)
		httpPort := strconv.Itoa(rtmpPortNum + 7000)

		go mediaserver.StartLPMS(rtmpPort, httpPort, self.streamer, self.cloud, self.streamDB, self.viz, self.hive, self.config.FFMpegPath, self.config.VodPath)
	}

	glog.Infof("Swarm http proxy started on port: %v", self.config.Port)

	if self.corsString != "" {
		glog.Infof("Swarm http proxy started with corsdomain:", self.corsString)
	}

	return nil
}

// implements the node.Service interface
// stops all component services.
func (self *Swarm) Stop() error {
	self.dpa.Stop()
	self.hive.Stop()
	if ch := self.config.Swap.Chequebook(); ch != nil {
		ch.Stop()
		ch.Save()
	}
	return self.config.Save()
}

// implements the node.Service interface
func (self *Swarm) Protocols() []p2p.Protocol {
	proto, err := network.Bzz(self.depo, self.backend, self.hive, self.dbAccess, self.config.Swap, self.config.SyncParams, self.config.NetworkId, self.streamer, self.streamDB, &self.cloud, self.viz)
	if err != nil {
		return nil
	}
	return []p2p.Protocol{proto}
}

// implements node.Service
// Apis returns the RPC Api descriptors the Swarm implementation offers
func (self *Swarm) APIs() []rpc.API {
	return []rpc.API{
		// public APIs
		{
			Namespace: "bzz",
			Version:   "0.1",
			Service:   api.NewStorage(self.api),
			Public:    true,
		},
		{
			Namespace: "bzz",
			Version:   "0.1",
			Service:   &Info{self.config, chequebook.ContractParams},
			Public:    true,
		},
		// admin APIs
		{
			Namespace: "bzz",
			Version:   "0.1",
			Service:   api.NewFileSystem(self.api),
			Public:    false},
		{
			Namespace: "bzz",
			Version:   "0.1",
			Service:   api.NewControl(self.api, self.hive),
			Public:    false,
		},
		{
			Namespace: "chequebook",
			Version:   chequebook.Version,
			Service:   chequebook.NewApi(self.config.Swap.Chequebook),
			Public:    false,
		},
		// {Namespace, Version, api.NewAdmin(self), false},
	}
}

func (self *Swarm) Api() *api.Api {
	return self.api
}

// SetChequebook ensures that the local checquebook is set up on chain.
func (self *Swarm) SetChequebook(ctx context.Context) error {
	err := self.config.Swap.SetChequebook(ctx, self.backend, self.config.Path)
	if err != nil {
		return err
	}
	glog.Infof("new chequebook set (%v): saving config file, resetting all connections in the hive", self.config.Swap.Contract.Hex())
	self.config.Save()
	self.hive.DropAll()
	return nil
}

// Local swarm without netStore
func NewLocalSwarm(datadir, port string) (self *Swarm, err error) {
	glog.Infof("Creating New Local Swarm with Datadir: %v", datadir)

	prvKey, err := crypto.GenerateKey()
	if err != nil {
		return
	}

	config, err := api.NewConfig(datadir, common.Address{}, prvKey, network.NetworkId, "", "")
	if err != nil {
		return
	}
	config.Port = port

	dpa, err := storage.NewLocalDPA(datadir)
	if err != nil {
		return
	}

	self = &Swarm{
		api:    api.NewApi(dpa, nil),
		config: config,
	}

	return
}

// serialisable info about swarm
type Info struct {
	*api.Config
	*chequebook.Params
}

func (self *Info) Info() *Info {
	return self
}
