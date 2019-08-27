package messenger

import (
	"bufio"
	"net"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"io/ioutil"
	"math/rand"

	log "github.com/sirupsen/logrus"
	// "github.com/spf13/viper"

	"github.com/thetatoken/theta/common"
	"github.com/thetatoken/theta/crypto"
	"github.com/thetatoken/theta/p2pl"
	p2ptypes "github.com/thetatoken/theta/p2pl/types"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-peerstore"
	"github.com/libp2p/go-libp2p-core/protocol"
	cr "github.com/libp2p/go-libp2p-crypto"
	// "github.com/libp2p/go-libp2p/p2p/discovery"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	dhtopts "github.com/libp2p/go-libp2p-kad-dht/opts"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"

	ds "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	
	ma "github.com/multiformats/go-multiaddr"
)

var logger *log.Entry = log.WithFields(log.Fields{"prefix": "p2pl"})

const (
	thetaP2PProtocolPrefix 			  = "/theta/1.0.0/"
	defaultPeerDiscoveryPulseInterval = 3 * time.Second
	discoverInterval                  = 3000    // 3 sec
)

type Messenger struct {
	host          host.Host
	msgHandlerMap map[common.ChannelIDEnum](p2pl.MessageHandler)
	config        MessengerConfig
	seedPeers	  []*peer.AddrInfo
	gsub          *pubsub.PubSub
	dht           *kaddht.IpfsDHT

	// Life cycle
	wg      *sync.WaitGroup
	quit    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
}

//
// MessengerConfig specifies the configuration for Messenger
//
type MessengerConfig struct {
	networkProtocol     string
}

func createP2PAddr(netAddr, networkProtocol string) (ma.Multiaddr, error) {
	ip, port, err := net.SplitHostPort(netAddr)
	if err != nil {
		return nil, err
	}
	multiAddr, err := ma.NewMultiaddr(fmt.Sprintf("/ip4/%v/%v/%v", ip, networkProtocol, port))
	if err != nil {
		return nil, err
	}
	return multiAddr, nil
}

// GetDefaultMessengerConfig returns the default config for messenger, not necessary
func GetDefaultMessengerConfig() MessengerConfig {
	return MessengerConfig{
		networkProtocol:     "tcp",
	}
}

// CreateMessenger creates an instance of Messenger
func CreateMessenger(privKey *crypto.PrivateKey, seedPeerMultiAddresses []string,
	port int, msgrConfig MessengerConfig) (*Messenger, error) {

	ctx, cancel := context.WithCancel(context.Background())

	messenger := &Messenger{
		msgHandlerMap: make(map[common.ChannelIDEnum](p2pl.MessageHandler)),
		config: msgrConfig,
		wg:     &sync.WaitGroup{},
	}

	hostId, _, err := cr.GenerateEd25519Key(strings.NewReader(common.Bytes2Hex(privKey.ToBytes())))
	if err != nil {
		return messenger, err
	}
	localNetAddress, err := createP2PAddr(fmt.Sprintf("0.0.0.0:%v", strconv.Itoa(port)), msgrConfig.networkProtocol)
	if err != nil {
		return messenger, err
	}
	host, err := libp2p.New(
		ctx,
		libp2p.EnableRelay(),
		libp2p.Identity(hostId),
		libp2p.ListenAddrs([]ma.Multiaddr{localNetAddress}...),
	)
	if err != nil {
		cancel()
		return messenger, err
	}
	messenger.host = host

	// seeds
	for _, seedPeerMultiAddrStr := range seedPeerMultiAddresses {
		addr, err := ma.NewMultiaddr(seedPeerMultiAddrStr)
		if err != nil {
			cancel()
			return messenger, err
		}
		peer, err := peerstore.InfoFromP2pAddr(addr)
		if err != nil {
			cancel()
			return messenger, err
		}
		messenger.seedPeers = append(messenger.seedPeers, peer)
	}

	// kad-dht
	dopts := []dhtopts.Option{
		// dhtopts.Validator(NullValidator{}),
		dhtopts.Datastore(dsync.MutexWrap(ds.NewMapDatastore())),
		dhtopts.Protocols(
			protocol.ID(thetaP2PProtocolPrefix + "dht"),
		),
	}

	dht, err := kaddht.New(ctx, host, dopts...)
	if err != nil {
		cancel()
		return messenger, err
	}
	host = rhost.Wrap(host, dht)
	messenger.dht = dht

	// pubsub
	psOpts := []pubsub.Option{
		pubsub.WithMessageSigning(false),
		pubsub.WithStrictSignatureVerification(false),
	}
	gsub, err := pubsub.NewGossipSub(ctx, host, psOpts...)
	if err != nil {
		cancel()
		return messenger, err
	}
	messenger.gsub = gsub

	logger.Infof("Created node %v, %v", host.ID(), host.Addrs())
	return messenger, nil
}

// Start is called when the Messenger starts
func (msgr *Messenger) Start(ctx context.Context) error {
	c, cancel := context.WithCancel(ctx)
	msgr.ctx = c
	msgr.cancel = cancel

	// seeds
	perm := rand.Perm(len(msgr.seedPeers))
	for i := 0; i < len(perm); i++ { // create outbound peers in a random order
		msgr.wg.Add(1)
		go func(i int) {
			defer msgr.wg.Done()

			time.Sleep(time.Duration(rand.Int63n(discoverInterval)) * time.Millisecond)
			j := perm[i]
			seedPeer := msgr.seedPeers[j]
			var err error
			for i := 0; i < 3; i++ { // try up to 3 times
				// err = msgr.host.Connect(ctx, *seedPeer)
				// if err == nil {
				// 	logger.Infof("Successfully connected to seed peer %v", seedPeer)
				// 	break
				// }
				time.Sleep(time.Second * 3)
			}

			if err != nil {
				logger.Errorf("Failed to connect to seed peer %v: %v", seedPeer, err)
			}
		}(i)
	}

	// kad-dht
	if len(msgr.seedPeers) > 0 {
		peerinfo := msgr.seedPeers[0]
		if err := msgr.host.Connect(ctx, *peerinfo); err != nil {
			logger.Errorf("Could not start peer discovery via DHT: %v", err)
		}
	}	

	bcfg := kaddht.DefaultBootstrapConfig
	bcfg.Period = time.Duration(defaultPeerDiscoveryPulseInterval)
	if err := msgr.dht.BootstrapWithConfig(ctx, bcfg); err != nil {
		logger.Errorf("Failed to bootstrap DHT: %v", err)
	}

	// mDns
	// mdnsService, err := discovery.NewMdnsService(ctx, msgr.host, defaultPeerDiscoveryPulseInterval, viper.GetString(common.CfgLibP2PRendezvous))
	// if err != nil {
	// 	return err
	// }
	// mdnsService.RegisterNotifee(&discoveryNotifee{ctx, msgr.host})

	return nil
}

// Stop is called when the Messenger stops
func (msgr *Messenger) Stop() {
	msgr.cancel()
}

// Wait suspends the caller goroutine
func (msgr *Messenger) Wait() {
	msgr.wg.Wait()
}

// Broadcast broadcasts the given message to all the connected peers
func (msgr *Messenger) Broadcast(message p2ptypes.Message) (successes chan bool) {
	logger.Debugf("Broadcasting messages...")
	// allPeers := msgr.host.Peerstore().Peers()
	
	// successes = make(chan bool, allPeers.Len())
	// for _, peer := range allPeers {
	// 	if (peer == msgr.host.ID()) {
	// 		continue
	// 	}

	// 	go func(peer string) {
	// 		success := msgr.Send(peer, message)
	// 		successes <- success
	// 	}(peer.String())
	// }

	// return successes

	msgHandler := msgr.msgHandlerMap[message.ChannelID]
	bytes, err := msgHandler.EncodeMessage(message.Content)
	if err != nil {
		logger.Errorf("Encoding error: %v", err)
	} else {
		if err := msgr.gsub.Publish(strconv.Itoa(int(message.ChannelID)), bytes); err != nil {
			log.Errorf("Failed to publish to gossipsub topic: %v", err)
		}
	}

	logger.Infof("======== peerstore: %v", msgr.host.Peerstore().Peers())

	return nil
}

// Send sends the given message to the specified peer
func (msgr *Messenger) Send(peerID string, message p2ptypes.Message) bool {
	id, err := peer.IDB58Decode(peerID)
	if err != nil {
		logger.Warnf("Can't decode peer id, %v", err)
		return false
	}

	peer := msgr.host.Peerstore().PeerInfo(id)
	if peer.ID == "" {
		return false
	}

	msgHandler := msgr.msgHandlerMap[message.ChannelID]
	bytes, err := msgHandler.EncodeMessage(message.Content)
	if err != nil {
		logger.Errorf("Encoding error: %v", err)
		return false
	}
	
	stream, err := msgr.host.NewStream(msgr.ctx, id, protocol.ID(thetaP2PProtocolPrefix+strconv.Itoa(int(message.ChannelID))))
	if err != nil {
		logger.Errorf("Stream open failed: %v", err)
		return false
	}
	defer stream.Close()

	w := bufio.NewWriter(stream)
	w.Write([]byte(bytes))
	err = w.Flush()
	if err != nil {
		logger.Errorf("Error flushing buffer %v", err)
		return false
	}

	return true
}

// ID returns the ID of the current node
func (msgr *Messenger) ID() string {
	return string(msgr.host.ID())
}

// RegisterMessageHandler registers the message handler
func (msgr *Messenger) RegisterMessageHandler(msgHandler p2pl.MessageHandler) {
	channelIDs := msgHandler.GetChannelIDs()
	for _, channelID := range channelIDs {
		if msgr.msgHandlerMap[channelID] != nil {
			logger.Errorf("Message handler is already added for channelID: %v", channelID)
			return
		}
		msgr.msgHandlerMap[channelID] = msgHandler

		msgr.registerStreamHandler(channelID)

		sub, err := msgr.gsub.Subscribe(strconv.Itoa(int(channelID)))
		if err != nil {
			logger.Errorf("Failed to subscribe to channel %v, %v", channelID, err)
			continue
		}
		go func() {
			defer sub.Cancel()
	
			var msg *pubsub.Message
			var err error
	
			// // Recover from any panic as part of the receive p2p msg process.
			// defer func() {
			// 	if r := recover(); r != nil {
			// 		log.WithFields(logrus.Fields{
			// 			"r":        r,
			// 			"msg.Data": attemptToConvertPbToString(msg.Data, message),
			// 		}).Error("P2P message caused a panic! Recovering...")
			// 	}
			// }()
	
			for {
				msg, err = sub.Next(context.Background())
	
				if msgr.ctx != nil && msgr.ctx.Err() != nil {
					logger.Errorf("Context error %v", msgr.ctx.Err())
					return
				}
				if err != nil {
					logger.Errorf("Failed to get next message: %v", err)
					continue
				}
	
				if msg == nil || msg.GetFrom() == msgr.host.ID() {
					continue
				}
	
				message, err := msgHandler.ParseMessage(msg.GetFrom().String(), channelID, msg.Data)
				if err != nil {
					logger.Errorf("Failed to parse message, %v", err)
					return
				}

				msgHandler.HandleMessage(message)
			}
		}()
	}
}

func (msgr *Messenger) registerStreamHandler(channelID common.ChannelIDEnum) {
	msgr.host.SetStreamHandler(protocol.ID(thetaP2PProtocolPrefix+strconv.Itoa(int(channelID))), func(stream network.Stream) {
		peerID := stream.Conn().RemotePeer().String()
		defer stream.Close()

		bytes, err := ioutil.ReadAll(stream)
		if err != nil {
			logger.Errorf("Failed to read stream: %v", err)
			return
		}

		msgHandler := msgr.msgHandlerMap[channelID]
		message, err := msgHandler.ParseMessage(peerID, channelID, bytes)
		if err != nil {
			logger.Errorf("Failed to parse message: %v", err)
			return
		}
		msgHandler.HandleMessage(message)
	})
}